package agentlog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// TestClient_DropCountSurvivesFailedPost: the ring-overflow count must
// reach the site on the next successful POST, even when the POST that
// first carried the synthetic "dropped N" envelope failed. Overflow and
// a failing site tend to coincide (a slow site is what lets the ring
// fill), so losing the count on a failed POST hides the drops exactly
// when they happen.
func TestClient_DropCountSurvivesFailedPost(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	var delivered []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg ws.LogBatchMsg
		_ = json.NewDecoder(r.Body).Decode(&msg)
		mu.Lock()
		defer mu.Unlock()
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		for _, e := range msg.Envelopes {
			delivered = append(delivered, e.Msg)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A one-slot ring: "first" is accepted, the second push overflows
	// and is counted as dropped. Both happen before Run starts, so the
	// first POST deterministically carries "first" plus the drop report.
	c := NewClient(strings.TrimPrefix(srv.URL, "http://"), slogkit.SourceAgent)
	c.maxRing = 1
	c.Push(slogkit.Envelope{TS: time.Now(), Level: slogkit.LevelInfo, Msg: "first"})
	c.Push(slogkit.Envelope{TS: time.Now(), Level: slogkit.LevelInfo, Msg: "dropped-by-backpressure"})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	waitRequests := func(n int) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := requests
			mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("site saw fewer than %d POSTs", n)
	}
	// The first POST gets a 503. Push one more line so Run makes a
	// second POST, which succeeds and must carry the drop report.
	waitRequests(1)
	c.Push(slogkit.Envelope{TS: time.Now(), Level: slogkit.LevelInfo, Msg: "second"})
	waitRequests(2)
	cancel()
	c.Close()
	<-done

	mu.Lock()
	defer mu.Unlock()
	for _, m := range delivered {
		if m == "dropped 1 log lines due to local backpressure" {
			return
		}
	}
	t.Fatalf("site never received the drop report; delivered %q", delivered)
}
