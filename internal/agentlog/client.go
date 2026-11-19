// Package agentlog implements the agent-side log envelope client:
// a slogkit.Sink that batches envelopes and POSTs them to the site
// worker's /logs/ingest HTTP endpoint.
//
// Critical design property: logging MUST NOT block deploy execution.
// On any failure (timeout, connection refused, 5xx), the batch is
// dropped and the agent continues. A small ring buffers envelopes
// between the slog handler and the HTTP-batching goroutine; ring
// overflows drop newest. Drops are surfaced periodically via a
// synthetic envelope with the dropped count.
package agentlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// Client is a slogkit.Sink that POSTs envelopes to the site worker.
type Client struct {
	endpoint   string
	httpClient *http.Client

	mu        sync.Mutex
	notEmpty  *sync.Cond
	pending   []slogkit.Envelope
	maxRing   int
	dropped   atomic.Int64
	closed    bool
	closeOnce sync.Once

	source slogkit.Source
}

// NewClient returns a Client that POSTs to {server}/logs/ingest.
// server is the host:port of the site worker (without scheme).
// Source is "agent" for the deploy/commission agent.
func NewClient(server string, source slogkit.Source) *Client {
	c := &Client{
		endpoint:   fmt.Sprintf("http://%s/logs/ingest", server),
		httpClient: &http.Client{Timeout: 2 * time.Second},
		maxRing:    8192, // ~1 MiB at ~150B/envelope
		source:     source,
	}
	c.notEmpty = sync.NewCond(&c.mu)
	return c
}

// Push enqueues an envelope. Drops newest if the ring is full so the
// caller path stays bounded and never blocks. Implements slogkit.Sink.
func (c *Client) Push(env slogkit.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if len(c.pending) >= c.maxRing {
		c.dropped.Add(1)
		return
	}
	c.pending = append(c.pending, env)
	c.notEmpty.Signal()
}

// Run drains the ring and POSTs batches until ctx is done. Failures
// drop the batch — no retry. The drop counter is surfaced as a
// synthetic envelope on the next successful POST: a failed POST puts
// its count back so the following batch reports it again.
func (c *Client) Run(ctx context.Context) {
	const batchMax = 64
	for {
		batch := c.takeBatch(ctx, batchMax)
		if batch == nil {
			return
		}
		dropped := c.dropped.Swap(0)
		if dropped > 0 {
			batch = append(batch, slogkit.Envelope{
				TS:     time.Now(),
				Level:  slogkit.LevelWarn,
				Source: c.source,
				Msg:    fmt.Sprintf("dropped %d log lines due to local backpressure", dropped),
			})
		}
		if err := c.postBatch(ctx, batch); err != nil && dropped > 0 {
			c.dropped.Add(dropped)
		}
	}
}

// takeBatch waits for at least one envelope, then drains up to max.
// Returns nil when ctx is done and the ring is empty.
func (c *Client) takeBatch(ctx context.Context, max int) []slogkit.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.pending) == 0 && !c.closed {
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				c.mu.Lock()
				c.notEmpty.Broadcast()
				c.mu.Unlock()
			case <-stop:
			}
		}()
		c.notEmpty.Wait()
		close(stop)
		if ctx.Err() != nil && len(c.pending) == 0 {
			return nil
		}
	}
	if len(c.pending) == 0 {
		return nil
	}
	n := min(len(c.pending), max)
	batch := append([]slogkit.Envelope(nil), c.pending[:n]...)
	c.pending = c.pending[n:]
	return batch
}

// Close marks the client closed. takeBatch returns nil after the
// current pending drains.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.closed = true
		c.notEmpty.Broadcast()
	})
}

func (c *Client) postBatch(ctx context.Context, envelopes []slogkit.Envelope) error {
	if len(envelopes) == 0 {
		return nil
	}
	body, err := json.Marshal(ws.LogBatchMsg{Envelopes: envelopes})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("site /logs/ingest returned %d", resp.StatusCode)
	}
	return nil
}
