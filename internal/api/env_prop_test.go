package api

// Shared scaffolding for the property tests in this package (files named
// *_prop_test.go). Nothing here asserts anything; it builds
// the production router (NewServer) against the shared testpg Postgres,
// drives it in-process, and counts what the generated histories reached.

import (
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	cryptossh "golang.org/x/crypto/ssh"
	"pgregory.net/rapid"

	"github.com/bluesky-social/beyond/tokenverify"

	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// TestMain purges the shared Postgres container when this package's tests
// finish; without it the container outlives the test binary until testpg's
// one-hour expiry.
func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Shutdown()
	os.Exit(code)
}

const (
	propOperatorToken = "op-tok"      // in the required group
	propOtherOpToken  = "op2-tok"     // a second operator in the required group
	propNoGroupToken  = "nogroup-tok" // valid token, outside the required group
	propCredToken     = "cred-tok"    // required group and credential group
)

// propSeed is the fixed PRNG seed of a default run.
const propSeed = 20261119

// propCheck is rapid.Check with defaults suited to properties whose every
// case truncates the database and rebuilds the router: a plain `go test`
// runs `checks` cases from a fixed seed, so the default run is short and
// repeatable. Passing -rapid.checks or -rapid.seed (or RAPID_CHECKS /
// RAPID_SEED) hands both back to rapid, which is how to search further:
//
//	go test ./internal/api/ -run TestProp_ -rapid.checks=2000
func propCheck(t *testing.T, checks int, prop func(*rapid.T)) {
	t.Helper()
	explicit := os.Getenv("RAPID_CHECKS") != "" || os.Getenv("RAPID_SEED") != ""
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "rapid.checks" || f.Name == "rapid.seed" {
			explicit = true
		}
	})
	if !explicit {
		for name, value := range map[string]string{"rapid.checks": strconv.Itoa(checks), "rapid.seed": strconv.Itoa(propSeed)} {
			f := flag.Lookup(name)
			prev := f.Value.String()
			if err := f.Value.Set(value); err != nil {
				t.Fatalf("set -%s: %v", name, err)
			}
			defer func() { _ = f.Value.Set(prev) }()
		}
	}
	rapid.Check(t, prop)
}

// propStats counts behavioral coverage across every rapid case of a test.
type propStats struct {
	mu sync.Mutex
	n  map[string]int
}

func newPropStats() *propStats { return &propStats{n: map[string]int{}} }

func (s *propStats) add(key string) {
	s.mu.Lock()
	s.n[key]++
	s.mu.Unlock()
}

func (s *propStats) report(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.n))
	for k := range s.n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-58s %d\n", k, s.n[k])
	}
	t.Logf("coverage counters:\n%s", b.String())
}

// propResp is one in-process response.
type propResp struct {
	code int
	body []byte
}

func (r propResp) json(v any) error { return json.Unmarshal(r.body, v) }

func (r propResp) String() string {
	b := string(r.body)
	if len(b) > 400 {
		b = b[:400] + "..."
	}
	return fmt.Sprintf("%d %s", r.code, strings.TrimSpace(b))
}

// propEnv is one production router over a truncated database.
type propEnv struct {
	st  *store.Store
	srv *Server
	h   http.Handler
	inv *fakeInvalidator
	pub *fakeOpPublisher
}

// propVerifier knows the harness's fixed tokens.
func propVerifier() *fakeVerifier {
	return &fakeVerifier{claims: map[string]*tokenverify.Claims{
		propOperatorToken: {Subject: "op", Groups: []string{"platform"}},
		propOtherOpToken:  {Subject: "op2", Groups: []string{"platform"}},
		propNoGroupToken:  {Subject: "eng", Groups: []string{"engineering"}},
		propCredToken:     {Subject: "cred", Groups: []string{"platform", "bmc-readers"}},
	}}
}

// newPropEnv truncates the shared database and builds the production
// router over it with operator auth, the log and console stream routes, and
// recording fakes for snapshot invalidation and the op stream. mutate may
// adjust the Config (wire NetBox, secrets, a snapshot builder) before the
// server is built.
func newPropEnv(t *testing.T, mutate func(*Config)) *propEnv {
	t.Helper()
	st := store.New(testpg.DB(t, store.Migrate))
	inv := &fakeInvalidator{}
	pub := &fakeOpPublisher{}
	cfg := Config{
		Store:            st,
		RequiredGroups:   []string{"platform"},
		CredentialGroups: []string{"bmc-readers"},
		Verifier:         propVerifier(),
		LogStore:         logstore.New(st.DB()),
		ConsoleStore:     NewConsoleStoreAdapter(st),
		Invalidator:      inv,
		OpPublisher:      pub,
		Version:          "prop",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s := NewServer(cfg)
	s.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &propEnv{st: st, srv: s, h: s.Echo(), inv: inv, pub: pub}
}

// propReq describes one request. A nil Header map means "operator token,
// JSON content type when there is a body".
type propReq struct {
	method string
	path   string
	body   string
	// ctype overrides the content type sent with a non-empty body
	// ("" keeps application/json; "-" sends none).
	ctype string
	// token overrides the bearer token ("" keeps the operator token; "-"
	// sends no Authorization header).
	token string
}

func (e *propEnv) do(r propReq) propResp {
	var body io.Reader = http.NoBody
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	req := httptest.NewRequest(r.method, r.path, body)
	if r.body != "" {
		switch r.ctype {
		case "":
			req.Header.Set("Content-Type", "application/json")
		case "-":
		default:
			req.Header.Set("Content-Type", r.ctype)
		}
	}
	switch r.token {
	case "":
		req.Header.Set("Authorization", "Bearer "+propOperatorToken)
	case "-":
	default:
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return propResp{code: w.Code, body: w.Body.Bytes()}
}

func (e *propEnv) get(path string) propResp { return e.do(propReq{method: http.MethodGet, path: path}) }

func (e *propEnv) post(path, body string) propResp {
	return e.do(propReq{method: http.MethodPost, path: path, body: body})
}

// propSSHKey returns the i-th of a fixed set of valid ed25519 public keys in
// authorized_keys form (no comment, no trailing newline).
func propSSHKey(i int) string {
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = byte(i + 1)
	pub, err := cryptossh.NewPublicKey(ed25519.NewKeyFromSeed(seed).Public())
	if err != nil {
		panic(err)
	}
	return strings.TrimSpace(string(cryptossh.MarshalAuthorizedKey(pub)))
}
