package cmd

// Property: the watch TUI's log source (logSource / streamLogEnvelopes)
// delivers every envelope exactly once and in id order across connection
// drops, when the server honors last_seen_id the way handleLogsStream does.
// The stub server's ids are sparse (a filtered stream skips ids), each
// connection is dropped after a random number of envelopes with or without
// a close frame, and some connections fail before the subscribe ack.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/ws"
)

func TestLogSourceResumesExactlyOnce_Prop(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Sparse increasing ids.
		n := rapid.IntRange(1, 40).Draw(rt, "n")
		ids := make([]int64, 0, n)
		var id int64
		for i := 0; i < n; i++ {
			id += int64(rapid.IntRange(1, 5).Draw(rt, "gap"))
			ids = append(ids, id)
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
					ID: id, Envelope: slogkit.Envelope{TS: time.Now().UTC(), Level: slogkit.LevelInfo, Source: slogkit.SourceAgent, Msg: "m"},
				})
				if c.Write(ctx, websocket.MessageText, buf) != nil {
					return
				}
				sent++
			}
			<-ctx.Done()
		}))
		defer srv.Close()
		prevURL, prevToken := apiURL, apiToken
		apiURL, apiToken = srv.URL, "tok"
		defer func() { apiURL, apiToken = prevURL, prevToken }()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var got []int64
		var gmu sync.Mutex
		done := make(chan error, 1)
		go func() {
			done <- logSource(uuid.NewString(), uuid.NewString(), time.Time{})(ctx, func(id int64, _ slogkit.Envelope) {
				gmu.Lock()
				got = append(got, id)
				full := len(got) >= len(ids)
				gmu.Unlock()
				if full {
					cancel()
				}
			})
		}()
		<-done
		gmu.Lock()
		defer gmu.Unlock()
		if len(got) != len(ids) || !sort.SliceIsSorted(got, func(i, j int) bool { return got[i] < got[j] }) {
			rt.Fatalf("delivered %v, want each of %v exactly once in order (subscribes carried last_seen_id %v)", got, ids, subs)
		}
		for i := range ids {
			if got[i] != ids[i] {
				rt.Fatalf("delivered %v, want %v (subscribes carried last_seen_id %v)", got, ids, subs)
			}
		}
	})
}
