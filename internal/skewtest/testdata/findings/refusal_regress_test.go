package ws

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDial_RefusalCarriesTheServersReason: a worker whose connection global
// refuses logs Dial's error and nothing else. The hub answers a refused
// upgrade with a sentence that says what is wrong ("missing worker token:
// send it as Authorization: Bearer <token>", "unauthorized"); Dial must not
// reduce it to a bare status code.
func TestDial_RefusalCarriesTheServersReason(t *testing.T) {
	const reason = "missing worker token: send it as Authorization: Bearer <token>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, reason, http.StatusUnauthorized)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("Dial succeeded against a server that answers 401")
	}
	for _, want := range []string{"401", reason} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Dial error %q does not contain %q", err, want)
		}
	}
}

// TestHub_SaysWhyItRefusedAWorkerThatSentItsTokenInTheURL: between upgrading
// global across v0.6.0 and upgrading each worker by hand, every worker
// reconnects forever with its token in the URL and is answered 401. The hub
// is the only side that can tell the operator which workers are still on
// the old build and what to do; it must log that, without the token.
func TestHub_SaysWhyItRefusedAWorkerThatSentItsTokenInTheURL(t *testing.T) {
	var logs bytes.Buffer
	hub := NewHub(HubConfig{Auth: newFakeAuth(), Rebuilder: &fakeRebuilder{}, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	defer hub.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ws/site?token=s3cret-worker-token", nil)
	hub.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	got := logs.String()
	if strings.Contains(got, "s3cret-worker-token") {
		t.Fatalf("the hub logged the worker's token: %s", got)
	}
	if !strings.Contains(got, "token in the URL") || !strings.Contains(got, "upgrade") {
		t.Fatalf("the hub refused a worker that sent its token in the URL without logging why or what to do; log:\n%s", got)
	}
}
