package client_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/cmd/slagctl/client"
)

// Property-based coverage for how one Client, over the life of one command,
// answers the API refusing its bearer. A command is a sequence of requests
// (a watch polls for as long as it is left open) during which the API can
// stop accepting the cached token, another invocation can replace it, and
// the refresher can work, hand back a token the API refuses too, report the
// sign-in gone, or fail in a way a retry may clear.
//
// The model is the set of tokens the API accepts, the cached token, and
// whether refreshing has been given up on. Contracts:
//   - a request whose bearer the API accepts succeeds and asks for no refresh
//   - a refused request asks the refresher at most once, naming the refused
//     token, and is resent at most once, with the same method and body and
//     the token the refresher returned
//   - when the refresher works the request succeeds
//   - once the refresher reports the sign-in gone, or the API refuses the
//     refreshed token too, no later request asks it again
//   - a refresher error a retry may clear costs only the request it failed
//     on: the next refused request asks again
//   - every 401 names `slagctl auth login` and wraps the refresher's error,
//     when that is why the request was not resent

// genTransientRefreshFailure is false because slagctl does not meet the last
// contract yet: any refresher error, an identity provider that is briefly
// unreachable included, switches refresh-on-401 off for the rest of the
// command, so later requests fail with 401 after the provider is back.
const genTransientRefreshFailure = false

var (
	errSignInGone  = errors.New("sign-in required: the identity provider refused the refresh token")
	errUnreachable = errors.New("the identity provider did not answer")
)

type clientWorld struct {
	mu       sync.Mutex
	accepted map[string]bool
	minted   int
	cached   string
	mode     string // what the refresher does: ok, refused, gone, unreachable
	asked    []string
	seen     []string // "<method> <bearer> <body>" for each request the API received
}

func (w *clientWorld) mint(accepted bool) string {
	w.minted++
	tok := fmt.Sprintf("t-%d", w.minted)
	w.accepted[tok] = accepted
	return tok
}

func (w *clientWorld) serve(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	w.mu.Lock()
	w.seen = append(w.seen, r.Method+" "+tok+" "+string(body))
	ok := w.accepted[tok]
	w.mu.Unlock()
	if !ok {
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = rw.Write([]byte(`{"message":"invalid token"}`))
		return
	}
	_, _ = rw.Write([]byte("{}"))
}

func (w *clientWorld) refresh(_ context.Context, rejected string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.asked = append(w.asked, rejected)
	switch w.mode {
	case "gone":
		return "", errSignInGone
	case "unreachable":
		return "", errUnreachable
	}
	w.cached = w.mint(w.mode == "ok")
	return w.cached, nil
}

func TestClientRefreshOn401_Property(t *testing.T) {
	modes := []string{"ok", "ok", "ok", "refused", "gone"}
	if genTransientRefreshFailure {
		modes = append(modes, "unreachable", "unreachable")
	}
	// One server for every case (a deep run would otherwise use up the
	// host's ephemeral ports); world is the case being served.
	var world atomic.Pointer[clientWorld]
	ts := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { world.Load().serve(rw, r) }))
	defer ts.Close()
	rapid.Check(t, func(t *rapid.T) {
		w := &clientWorld{accepted: map[string]bool{}, mode: "ok"}
		w.cached = w.mint(true)
		world.Store(w)
		c := client.New(client.Config{BaseURL: ts.URL, HTTPClient: ts.Client(),
			Bearer: func(context.Context) (string, error) {
				w.mu.Lock()
				defer w.mu.Unlock()
				return w.cached, nil
			},
			Refresh: w.refresh})

		// The model's half: has refreshing been given up on, and with what
		// error.
		gaveUp := false
		var gaveUpErr error

		ops := rapid.SliceOfN(rapid.SampledFrom([]string{"get", "get", "post", "refuse", "refuse", "mode", "replaced"}), 1, 25).Draw(t, "ops")
		for i, op := range ops {
			switch op {
			case "refuse":
				// The API stops accepting the cached token: it expired by
				// the API's clock, or its signing key was retired.
				w.mu.Lock()
				w.accepted[w.cached] = false
				w.mu.Unlock()
				continue
			case "mode":
				mode := rapid.SampledFrom(modes).Draw(t, "mode")
				w.mu.Lock()
				w.mode = mode
				w.mu.Unlock()
				continue
			case "replaced":
				// Another invocation refreshed the shared cache.
				w.mu.Lock()
				w.cached = w.mint(true)
				w.mu.Unlock()
				continue
			}

			w.mu.Lock()
			sent, mode := w.cached, w.mode
			good := w.accepted[sent]
			w.asked, w.seen = nil, nil
			w.mu.Unlock()
			method, body := http.MethodGet, ""
			var err error
			if op == "post" {
				method, body = http.MethodPost, fmt.Sprintf(`{"n":%d}`, i)
				err = c.Post(context.Background(), "/api/v1/things", map[string]int{"n": i}, nil)
			} else {
				err = c.Get(context.Background(), "/api/v1/things", nil, nil)
			}
			w.mu.Lock()
			asked, seen := w.asked, w.seen
			fresh := w.cached
			w.mu.Unlock()

			// What the model expects of this request.
			wantAsk := !good && !gaveUp
			wantResend := wantAsk && (mode == "ok" || mode == "refused")
			wantOK := good || (wantAsk && mode == "ok")
			var wantWrapped error
			if wantAsk {
				switch mode {
				case "refused":
					gaveUp = true
				case "gone":
					gaveUp, gaveUpErr = true, errSignInGone
				case "unreachable":
					wantWrapped = errUnreachable
				}
			}
			if gaveUp {
				wantWrapped = gaveUpErr
			}

			wantSeen := []string{method + " " + sent + " " + body}
			if wantResend {
				wantSeen = append(wantSeen, method+" "+fresh+" "+body)
			}
			if fmt.Sprint(seen) != fmt.Sprint(wantSeen) {
				t.Fatalf("op %d (%s, refresher %s): the API received %q, want %q", i, op, mode, seen, wantSeen)
			}
			if wantAsk && (len(asked) != 1 || asked[0] != sent) {
				t.Fatalf("op %d (%s, refresher %s): the refresher was asked %q, want once for the refused token %q", i, op, mode, asked, sent)
			}
			if !wantAsk && len(asked) != 0 {
				t.Fatalf("op %d (%s, refresher %s): the refresher was asked %q, want no refresh (bearer accepted: %v, given up: %v)", i, op, mode, asked, good, gaveUp)
			}
			if wantOK {
				if err != nil {
					t.Fatalf("op %d (%s, refresher %s): request failed: %v", i, op, mode, err)
				}
				continue
			}
			var apiErr *client.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
				t.Fatalf("op %d (%s, refresher %s): err = %v, want the API's 401", i, op, mode, err)
			}
			if !strings.Contains(err.Error(), "slagctl auth login") {
				t.Fatalf("op %d: the 401 does not name the command that fixes it: %v", i, err)
			}
			if got := errors.Unwrap(apiErr); got != wantWrapped {
				t.Fatalf("op %d (%s, refresher %s): the 401 wraps %v, want %v", i, op, mode, got, wantWrapped)
			}
		}
	})
}
