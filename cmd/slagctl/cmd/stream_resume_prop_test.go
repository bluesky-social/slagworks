package cmd

// Property: both of slagctl's log followers deliver every envelope exactly
// once and in id order across any sequence of connection drops, when the
// server honors last_seen_id the way handleLogsStream does. The followers
// are `machine logs -f` (logsStreamSession, which runLogsStream redials)
// and the watch view's log pane (streamLogEnvelopes, which logSource
// redials). The property drives the single-connection functions with the
// cursor each keeps, so a case does not sit out the followers' one-second
// redial waits.
//
// The stub server's ids are sparse (a filtered stream skips ids), some
// envelopes are far larger than coder/websocket's 32 KiB default read limit,
// each connection is dropped after a scripted number of envelopes with or
// without a close frame, and some are dropped before the subscribe is
// acknowledged. Checked besides the delivery: each redial subscribes with
// the highest id delivered so far, and `machine logs -f` classifies every
// drop as one to redial.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"pgregory.net/rapid"

	slclient "github.com/bluesky-social/slagworks/cmd/slagctl/client"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

func TestLogFollowersResumeExactlyOnce_Prop(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Sparse increasing ids, and how large each envelope's message is.
		n := rapid.IntRange(1, 40).Draw(rt, "n")
		ids := make([]int64, 0, n)
		sizes := map[int64]int{}
		var id int64
		for i := 0; i < n; i++ {
			id += int64(rapid.IntRange(1, 5).Draw(rt, "gap"))
			ids = append(ids, id)
			sizes[id] = 1
			if rapid.IntRange(0, 9).Draw(rt, "large") == 0 {
				sizes[id] = rapid.IntRange(33<<10, 400<<10).Draw(rt, "largeLen")
			}
		}
		// Per-connection script: how many envelopes before the drop, and how.
		type connPlan struct {
			beforeAck  bool // drop before the subscribed ack
			sendN      int
			closeFrame bool
		}
		plans := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) connPlan {
			return connPlan{
				beforeAck:  rapid.IntRange(0, 5).Draw(t, "beforeAck") == 0,
				sendN:      rapid.IntRange(0, 8).Draw(t, "sendN"),
				closeFrame: rapid.Bool().Draw(t, "closeFrame"),
			}
		}), 0, 6).Draw(rt, "plans")
		follower := rapid.SampledFrom([]string{"machine logs -f", "watch log pane"}).Draw(rt, "follower")

		var mu sync.Mutex
		connNo := 0
		var subs []int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer c.CloseNow() //nolint:errcheck
			ctx := r.Context()
			mu.Lock()
			k := connNo
			connNo++
			mu.Unlock()
			plan := connPlan{sendN: 1 << 30} // after the scripted drops, serve everything
			if k < len(plans) {
				plan = plans[k]
			}
			_, raw, err := c.Read(ctx)
			if err != nil {
				return
			}
			var env ws.Envelope
			var sub ws.LogStreamSubscribeMsg
			if json.Unmarshal(raw, &env) != nil || json.Unmarshal(env.Data, &sub) != nil {
				return
			}
			mu.Lock()
			subs = append(subs, sub.LastSeenID)
			mu.Unlock()
			if plan.beforeAck {
				return
			}
			ack, _ := ws.MarshalEnvelope(ws.TypeSubscribed, "", ws.SubscribedMsg{})
			if c.Write(ctx, websocket.MessageText, ack) != nil {
				return
			}
			sent := 0
			for _, id := range ids {
				if id <= sub.LastSeenID {
					continue
				}
				if sent == plan.sendN {
					if plan.closeFrame {
						_ = c.Close(websocket.StatusGoingAway, "proxy rollout")
					}
					return
				}
				buf, _ := ws.MarshalEnvelope(ws.TypeLogStreamEnvelope, "", ws.LogStreamEnvelopeMsg{
					ID: id, Envelope: slogkit.Envelope{TS: time.Now().UTC(), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent, Msg: strings.Repeat("m", sizes[id])},
				})
				if c.Write(ctx, websocket.MessageText, buf) != nil {
					return
				}
				sent++
			}
			// Everything is sent; hold the stream until the client leaves
			// (the request context of a hijacked connection never ends).
			<-c.CloseRead(ctx).Done()
		}))
		defer srv.Close()
		prevURL, prevClient := apiURL, apiClient
		apiURL = srv.URL
		apiClient = slclient.New(slclient.Config{BaseURL: srv.URL, HTTPClient: srv.Client(),
			Bearer: func(context.Context) (string, error) { return "stream-test-token", nil }})
		defer func() { apiURL, apiClient = prevURL, prevClient }()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var got []int64
		deliver := func(id int64, env slogkit.Envelope) {
			if len(env.Msg) != sizes[id] {
				rt.Fatalf("envelope %d arrived with a %d-byte message, want %d", id, len(env.Msg), sizes[id])
			}
			got = append(got, id)
			if len(got) >= len(ids) {
				cancel()
			}
		}

		// One session per connection, redialed at once with the follower's
		// own cursor. wantSubs is the model: a redial subscribes with the
		// highest id delivered so far.
		var wantSubs []int64
		streamURL, err := buildLogsStreamURL(apiURL)
		if err != nil {
			rt.Fatalf("stream URL: %v", err)
		}
		sub := ws.LogStreamSubscribeMsg{MachineID: uuid.NewString()}
		var lastSeen int64
		machineID, opID := uuid.NewString(), uuid.NewString()
		for session := 0; ctx.Err() == nil; session++ {
			if session > len(plans) {
				rt.Fatalf("%s: session %d ended with %d of %d envelopes delivered, after every scripted drop was spent", follower, session, len(got), len(ids))
			}
			high := int64(0)
			if len(got) > 0 {
				high = slices.Max(got)
			}
			wantSubs = append(wantSubs, high)
			if follower == "machine logs -f" {
				_, err := logsStreamSession(ctx, streamURL, &sub, func(env slogkit.Envelope, id int64) error {
					deliver(id, env)
					return nil
				}, true)
				var dropped *logsStreamDropped
				if ctx.Err() == nil && !errors.As(err, &dropped) {
					rt.Fatalf("machine logs -f: connection %d ended with %v, which follow mode does not redial; every drop here is a dropped connection", session, err)
				}
			} else if err := streamLogEnvelopes(ctx, machineID, opID, time.Time{}, &lastSeen, deliver); ctx.Err() == nil && err == nil {
				rt.Fatalf("watch log pane: connection %d ended without an error, which the pane takes as the end of the stream", session)
			}
		}

		if !slices.Equal(got, ids) {
			rt.Fatalf("%s delivered %v, want each of %v exactly once in order (subscribes carried last_seen_id %v)", follower, got, ids, subs)
		}
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(subs, wantSubs) {
			rt.Fatalf("%s subscribed with last_seen_id %v, want %v: each redial resumes after the highest id delivered", follower, subs, wantSubs)
		}
	})
}
