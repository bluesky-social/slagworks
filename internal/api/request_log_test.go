package api

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/beyond/tokenverify"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// syncBuffer is a log sink that is safe to write from the server's
// goroutines while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *syncBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *syncBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestRequestLog_DoesNotLogSiteWorkerToken: a site worker authenticates its
// WebSocket upgrade with ?token=<bearer>, and the API server's request
// logger writes a line for every request when its handler returns (for the
// upgrade, when the worker disconnects). That line must not carry the
// token: global's log is read by more people than may act as a site worker.
func TestRequestLog_DoesNotLogSiteWorkerToken(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	_, workerToken, err := st.RegisterSiteWorker("popa", "worker-1", "w1.popa")
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	snap := model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "popa"}}
	hub := ws.NewHub(ws.HubConfig{Auth: st, Rebuilder: staticRebuilder{snap}, Logger: log})
	hub.Push("popa", snap)
	s := NewServer(Config{
		RequiredGroups: []string{"platform"},
		Store:          st, WSHub: hub, OpPublisher: hub,
		Verifier: &fakeVerifier{claims: map[string]*tokenverify.Claims{}},
	})
	s.SetLogger(log)
	srv := httptest.NewServer(s.Echo())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	worker, err := ws.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/ws/site?token="+workerToken, nil)
	if err != nil {
		t.Fatalf("worker dial: %v", err)
	}
	if err := worker.WriteTyped(ctx, ws.TypeHello, "", ws.HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 1, Proto: ws.OpStreamProto}); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.ReadMessage(ctx); err != nil {
		t.Fatalf("hello reply: %v", err)
	}
	_ = worker.CloseNow()

	// The request log line is written when the upgrade handler returns.
	const line = `msg="api request" method=GET path=/api/v1/ws/site status=101`
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), `msg="api request" method=GET`) {
		if time.Now().After(deadline) {
			t.Fatalf("no request log line for the worker upgrade within 5s; log:\n%s",
				strings.ReplaceAll(logs.String(), workerToken, "<TOKEN>"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := logs.String()
	if strings.Contains(got, workerToken) {
		t.Errorf("global's log contains the site worker's bearer token; log:\n%s",
			strings.ReplaceAll(got, workerToken, "<TOKEN>"))
	}
	if !strings.Contains(got, line) {
		t.Errorf("request log has no %q line; log:\n%s", line, strings.ReplaceAll(got, workerToken, "<TOKEN>"))
	}
}
