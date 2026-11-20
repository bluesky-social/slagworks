package agentlog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// Property test for the agent log client against an httptest site
// worker with a generated fault schedule (per-request 200 or 5xx).
//
// Model: Push accepts while fewer than maxRing are pending, else counts
// a drop (drop newest). Run posts batches of at most 64 accepted
// envelopes in push order, plus a synthetic "dropped N" envelope when
// drops happened. Properties:
//   - accepted envelopes appear across all POSTs (successful or failed)
//     exactly once and in push order, never more than 64 real ones per
//     batch;
//   - the sum of N over synthetic envelopes in successful POSTs is at
//     least the number of drops (a failed POST puts its count back for
//     the next one, per the Run doc), and exactly the number of drops
//     when the client saw every POST the site accepted succeed. A POST
//     the site accepted but the client saw fail is reported again,
//     which the fix that added the put-back accepts as over-reporting.

type ingestRecorder struct {
	mu       sync.Mutex
	schedule []int // status per request; 200 after it runs out
	posts    []recordedPost
	nReal    int // non-synthetic envelopes received, across all posts
}

type recordedPost struct {
	status int
	envs   []slogkit.Envelope
}

func (r *ingestRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var msg ws.LogBatchMsg
	_ = json.NewDecoder(req.Body).Decode(&msg)
	r.mu.Lock()
	status := http.StatusOK
	if n := len(r.posts); n < len(r.schedule) {
		status = r.schedule[n]
	}
	r.posts = append(r.posts, recordedPost{status: status, envs: msg.Envelopes})
	for _, e := range msg.Envelopes {
		if _, ok := dropCount(e); !ok {
			r.nReal++
		}
	}
	r.mu.Unlock()
	w.WriteHeader(status)
}

// received returns how many non-synthetic envelopes the site has seen.
func (r *ingestRecorder) received() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nReal
}

func (r *ingestRecorder) postCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.posts)
}

// outcomeTransport records what the client saw of each POST: how many
// round trips finished, and how many returned a 2xx.
type outcomeTransport struct {
	mu   sync.Mutex
	done int
	ok   int
}

func (o *outcomeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.done++
	if err == nil && resp.StatusCode/100 == 2 {
		o.ok++
	}
	return resp, err
}

func (o *outcomeTransport) counts() (done, ok int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.done, o.ok
}

func dropCount(e slogkit.Envelope) (int, bool) {
	var n int
	if _, err := fmt.Sscanf(e.Msg, "dropped %d log lines", &n); err == nil {
		return n, true
	}
	return 0, false
}

func TestClient_Property_DeliveryAndDropAccounting(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		rec := &ingestRecorder{}
		nFaults := rapid.IntRange(0, 6).Draw(t, "nFaults")
		for i := 0; i < nFaults; i++ {
			rec.schedule = append(rec.schedule, rapid.SampledFrom([]int{
				http.StatusOK, http.StatusInternalServerError, http.StatusServiceUnavailable,
			}).Draw(t, "status"))
		}
		srv := httptest.NewServer(rec)
		defer srv.Close()

		c := NewClient(strings.TrimPrefix(srv.URL, "http://"), slogkit.SourceAgent)
		c.maxRing = rapid.IntRange(1, 100).Draw(t, "maxRing")
		tr := &outcomeTransport{}
		c.httpClient.Transport = tr

		// Pre-fill before Run so drops are deterministic, then push more
		// while Run is posting.
		pre := rapid.IntRange(0, 200).Draw(t, "pre")
		seq := 0
		accepted := []string{}
		drops := 0
		for i := 0; i < pre; i++ {
			msg := fmt.Sprintf("e%d", seq)
			seq++
			if len(c.pending) < c.maxRing {
				accepted = append(accepted, msg)
			} else {
				drops++
			}
			c.Push(slogkit.Envelope{TS: time.Now(), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent, Msg: msg})
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); c.Run(ctx) }()

		// Every accepted envelope reaches the site in some POST, whatever
		// status that POST gets, so wait for the site to have seen them
		// all rather than for the ring to look empty.
		waitDelivered := func() {
			deadline := time.Now().Add(5 * time.Second)
			for rec.received() < len(accepted) {
				if time.Now().After(deadline) {
					t.Fatalf("site received %d of %d accepted envelopes", rec.received(), len(accepted))
				}
				time.Sleep(time.Millisecond)
			}
		}
		waitDelivered()
		// A trickle afterwards, one POST each, outlasting the fault
		// schedule so a later successful POST exists to carry any
		// outstanding drop count.
		post := nFaults + rapid.IntRange(1, 3).Draw(t, "post")
		for i := 0; i < post; i++ {
			msg := fmt.Sprintf("e%d", seq)
			seq++
			accepted = append(accepted, msg)
			c.Push(slogkit.Envelope{TS: time.Now(), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent, Msg: msg})
			waitDelivered()
		}
		// Let the last POST's response reach the client before stopping
		// Run, so cancellation does not turn a delivered POST into a
		// failure on the client's side.
		deadline := time.Now().Add(5 * time.Second)
		for {
			finished, _ := tr.counts()
			if finished >= rec.postCount() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("client finished %d of %d POSTs", finished, rec.postCount())
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		c.Close()
		<-done

		rec.mu.Lock()
		defer rec.mu.Unlock()
		var seen []string
		reported := 0
		siteOK := 0
		for i, p := range rec.posts {
			if p.status == http.StatusOK {
				siteOK++
			}
			nReal := 0
			for _, e := range p.envs {
				if n, ok := dropCount(e); ok {
					if p.status == http.StatusOK {
						reported += n
					}
					continue
				}
				nReal++
				seen = append(seen, e.Msg)
			}
			if nReal > 64 {
				t.Fatalf("post %d carried %d envelopes, cap is 64", i, nReal)
			}
		}
		if fmt.Sprint(seen) != fmt.Sprint(accepted) {
			t.Fatalf("posted envelopes differ from accepted\n got  %v\n want %v", seen, accepted)
		}
		if reported < drops {
			t.Fatalf("%d drops, but successful POSTs reported only %d (schedule %v)", drops, reported, rec.schedule)
		}
		if _, clientOK := tr.counts(); clientOK == siteOK && reported != drops {
			t.Fatalf("%d drops, but successful POSTs reported %d (schedule %v)", drops, reported, rec.schedule)
		}
	})
}
