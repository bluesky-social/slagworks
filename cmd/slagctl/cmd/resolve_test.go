package cmd_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	slclient "github.com/bluesky-social/slagworks/cmd/slagctl/client"
	"github.com/bluesky-social/slagworks/cmd/slagctl/cmd"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/google/uuid"
)

// resolverFixture spins up a fake API that serves both the serial
// (?serial=) and hostname (?hostname=) branches of resolveMachine
// from a fixed fixture set. It returns the slclient pointed at it.
type resolverFixture struct {
	machines []model.Machine // each has Hostname populated
}

func (f *resolverFixture) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		serial := r.URL.Query().Get("serial")
		hostname := r.URL.Query().Get("hostname")
		var hits []model.Machine
		switch {
		case serial != "":
			for _, m := range f.machines {
				if m.NetBoxSerial == serial {
					hits = append(hits, m)
				}
			}
		case hostname != "":
			for _, m := range f.machines {
				if strings.Contains(strings.ToLower(m.Hostname), strings.ToLower(hostname)) {
					hits = append(hits, m)
				}
			}
		default:
			t.Errorf("unexpected query: %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(hits)
	})
	return mux
}

// TestResolveMachine_UUIDShortCircuits pins the no-network path: a
// well-formed UUID never hits the API. Important because operator
// automation that already knows the UUID should not pay a NetBox
// round-trip per command.
func TestResolveMachine_UUIDShortCircuits(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode([]model.Machine{})
	})
	cl, _ := testServerWithURL(t, mux)
	id := uuid.New()
	r, err := cmd.ResolveMachine(context.Background(), cl, id.String())
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id {
		t.Errorf("got %s, want %s", r.ID, id)
	}
	if calls != 0 {
		t.Errorf("UUID input must not hit the API, got %d calls", calls)
	}
}

func TestResolveMachine_ExactSerial(t *testing.T) {
	id := uuid.New()
	f := &resolverFixture{
		machines: []model.Machine{
			{ID: id, NetBoxSerial: "ABC-123"},
		},
	}
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, f.handler(t))})
	r, err := cmd.ResolveMachine(context.Background(), cl, "ABC-123")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id {
		t.Errorf("got %s, want %s", r.ID, id)
	}
}

func TestResolveMachine_ExactHostname(t *testing.T) {
	id := uuid.New()
	f := &resolverFixture{
		machines: []model.Machine{
			{ID: id, NetBoxSerial: "S-1", Hostname: "cpu1.pop1"},
			{ID: uuid.New(), NetBoxSerial: "S-2", Hostname: "cpu1.pop3"},
		},
	}
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, f.handler(t))})
	r, err := cmd.ResolveMachine(context.Background(), cl, "cpu1.pop1")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id {
		t.Errorf("got %s, want %s (hostnames matched: cpu1.pop1)", r.ID, id)
	}
}

// TestResolveMachine_UniqueSubstring covers the cpu1 → cpu1.pop1
// case: only one machine matches, so we resolve it. This is the
// sugar that lets operators type short names.
func TestResolveMachine_UniqueSubstring(t *testing.T) {
	id := uuid.New()
	f := &resolverFixture{
		machines: []model.Machine{
			{ID: id, NetBoxSerial: "S-1", Hostname: "cpu7.pop1"},
			{ID: uuid.New(), NetBoxSerial: "S-2", Hostname: "kworker-l34-1.pop1"},
		},
	}
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, f.handler(t))})
	r, err := cmd.ResolveMachine(context.Background(), cl, "cpu7")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id {
		t.Errorf("got %s, want %s", r.ID, id)
	}
}

func TestResolveMachine_AmbiguousSubstring(t *testing.T) {
	f := &resolverFixture{
		machines: []model.Machine{
			{ID: uuid.New(), NetBoxSerial: "S-1", Hostname: "cpu1.pop1"},
			{ID: uuid.New(), NetBoxSerial: "S-2", Hostname: "cpu1.pop3"},
			{ID: uuid.New(), NetBoxSerial: "S-3", Hostname: "cpu2.pop1"},
		},
	}
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, f.handler(t))})
	_, err := cmd.ResolveMachine(context.Background(), cl, "cpu1")
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected 'ambiguous' in error, got: %v", err)
	}
	// Both candidates should appear in the message so on-call can copy-paste.
	for _, want := range []string{"cpu1.pop1", "cpu1.pop3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected candidate %q in error, got: %v", want, err)
		}
	}
}

// TestResolveMachine_ExactBeatsAmbiguous: even when other rows
// substring-match, an exact hostname win cuts disambiguation
// short. Mirrors the on-call common case where typing the FQDN
// should always land on that host.
func TestResolveMachine_ExactBeatsAmbiguous(t *testing.T) {
	id := uuid.New()
	f := &resolverFixture{
		machines: []model.Machine{
			{ID: id, NetBoxSerial: "S-1", Hostname: "cpu1.pop1"},
			{ID: uuid.New(), NetBoxSerial: "S-2", Hostname: "cpu1.pop3"},
		},
	}
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, f.handler(t))})
	r, err := cmd.ResolveMachine(context.Background(), cl, "cpu1.pop1")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id {
		t.Errorf("exact hostname match must win; got %s, want %s", r.ID, id)
	}
}

// TestResolveMachine_ExactMatchIsCaseInsensitive covers the
// case-mismatch path: NetBox's name__ic filter is case-insensitive,
// so a typed `CPU1.pop1` must still resolve to the stored
// `cpu1.pop1` even when sibling rows (e.g. `cpu1.pop10`) would
// otherwise make the substring branch ambiguous.
func TestResolveMachine_ExactMatchIsCaseInsensitive(t *testing.T) {
	id := uuid.New()
	f := &resolverFixture{
		machines: []model.Machine{
			{ID: id, NetBoxSerial: "S-1", Hostname: "cpu1.pop1"},
			{ID: uuid.New(), NetBoxSerial: "S-2", Hostname: "cpu1.pop10"},
		},
	}
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, f.handler(t))})
	r, err := cmd.ResolveMachine(context.Background(), cl, "CPU1.POP1")
	if err != nil {
		t.Fatalf("expected case-insensitive exact match, got: %v", err)
	}
	if r.ID != id {
		t.Errorf("got %s, want %s", r.ID, id)
	}
}

// TestResolveMachine_ExactHostnameSharedIsAmbiguous: NetBox enforces
// device-name uniqueness per site, not globally, so two machines in
// different sites can carry the same hostname. Typing that hostname must
// be refused with both UUIDs listed, not resolved to whichever row the
// API returned first.
func TestResolveMachine_ExactHostnameSharedIsAmbiguous(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	f := &resolverFixture{
		machines: []model.Machine{
			{ID: a, NetBoxSerial: "S-1", Hostname: "node-1.rack"},
			{ID: b, NetBoxSerial: "S-2", Hostname: "node-1.rack"},
		},
	}
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, f.handler(t))})
	r, err := cmd.ResolveMachine(context.Background(), cl, "node-1.rack")
	if err == nil {
		t.Fatalf("two machines are named node-1.rack, want an ambiguity error; resolved to %s", r.ID)
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected 'ambiguous' in error, got: %v", err)
	}
	for _, want := range []string{a.String(), b.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected candidate UUID %s in error, got: %v", want, err)
		}
	}
}

func TestResolveMachine_NoMatch(t *testing.T) {
	f := &resolverFixture{machines: nil}
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, f.handler(t))})
	_, err := cmd.ResolveMachine(context.Background(), cl, "nope")
	if err == nil {
		t.Fatal("expected no-match error")
	}
	if !strings.Contains(err.Error(), "no machine matches") {
		t.Errorf("expected 'no machine matches', got: %v", err)
	}
}

// TestResolveDeviceIdent_UUIDToSerial pins the new behavior: a slagworks
// machine UUID (which the /devices/:ident endpoint can't resolve) is
// translated to the machine's NetBox serial via a single /machines/<uuid>
// lookup. Before this, `machine creds <uuid>` 404'd.
func TestResolveDeviceIdent_UUIDToSerial(t *testing.T) {
	id := uuid.New()
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(model.Machine{ID: id, NetBoxSerial: "S500002X0000001"})
	})
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, mux)})
	ident, err := cmd.ResolveDeviceIdent(context.Background(), cl, id.String())
	if err != nil {
		t.Fatal(err)
	}
	if ident != "S500002X0000001" {
		t.Errorf("got ident %q, want the machine serial", ident)
	}
	if gotPath != "/api/v1/machines/"+id.String() {
		t.Errorf("expected per-machine lookup, got path %q", gotPath)
	}
}

// TestResolveDeviceIdent_NonUUIDPassthrough: any non-UUID shape (name, id,
// serial — including non-machine devices like PDUs that have no machine row)
// is passed through verbatim with NO API call, so the server's resolveDevice
// keeps owning name/id/serial resolution.
func TestResolveDeviceIdent_NonUUIDPassthrough(t *testing.T) {
	for _, arg := range []string{"pdu-0102-left", "leaf-3.pop1", "583", "S500002X0000001"} {
		calls := 0
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusInternalServerError)
		})
		cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, mux)})
		ident, err := cmd.ResolveDeviceIdent(context.Background(), cl, arg)
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", arg, err)
		}
		if ident != arg {
			t.Errorf("%q: got ident %q, want passthrough", arg, ident)
		}
		if calls != 0 {
			t.Errorf("%q: non-UUID must not hit the API, got %d calls", arg, calls)
		}
	}
}

// TestResolveDeviceIdent_UUIDNotAMachineFallsBack: a UUID-shaped arg that
// doesn't name a registered machine (404) falls back to the verbatim arg, so
// the server's NetBox resolver still gets a shot — e.g. a non-machine device
// whose serial is itself UUID-shaped. Preserves the documented non-machine
// path through the shape detector.
func TestResolveDeviceIdent_UUIDNotAMachineFallsBack(t *testing.T) {
	id := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	})
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, mux)})
	ident, err := cmd.ResolveDeviceIdent(context.Background(), cl, id.String())
	if err != nil {
		t.Fatalf("404 on the machine lookup must fall back, got error: %v", err)
	}
	if ident != id.String() {
		t.Errorf("got ident %q, want the verbatim UUID arg %q", ident, id.String())
	}
}

// TestResolveDeviceIdent_UUIDServerError: a non-404 error (5xx, network) on the
// machine lookup is real and must propagate, not silently fall back.
func TestResolveDeviceIdent_UUIDServerError(t *testing.T) {
	id := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	})
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, mux)})
	_, err := cmd.ResolveDeviceIdent(context.Background(), cl, id.String())
	if err == nil {
		t.Fatal("expected a 5xx to propagate, not fall back")
	}
}

// TestResolveDeviceIdent_UUIDNoSerial: a machine with no NetBox serial can't
// have its device creds resolved (the admin secret is serial-keyed), so the
// translation fails with a clear message rather than passing an empty ident.
func TestResolveDeviceIdent_UUIDNoSerial(t *testing.T) {
	id := uuid.New()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(model.Machine{ID: id}) // NetBoxSerial empty
	})
	cl := slclient.New(slclient.Config{BaseURL: testHTTPServer(t, mux)})
	_, err := cmd.ResolveDeviceIdent(context.Background(), cl, id.String())
	if err == nil {
		t.Fatal("expected error for machine with no serial")
	}
	if !strings.Contains(err.Error(), "no NetBox serial") {
		t.Errorf("expected 'no NetBox serial' in error, got: %v", err)
	}
}

// testHTTPServer registers a httptest.Server with t.Cleanup and
// returns its base URL. Lighter than testServerWithURL when the test
// constructs its own Config.
func testHTTPServer(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}
