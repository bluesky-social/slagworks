package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TestRunConsoleGoroutine_ReconnectDrivesBanner wires a console source that
// cannot connect into the model through the runner: the pane must show the
// reconnect banner instead of "catching up…" while the source retries, and
// drop the banner once the source reports it is connected again.
func TestRunConsoleGoroutine_ReconnectDrivesBanner(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connected := make(chan struct{})
	src := func(ctx context.Context, _ func([]byte, bool), _ func(), onReconnecting func(bool)) error {
		onReconnecting(true) // the first dial failed
		select {
		case <-connected:
			onReconnecting(false)
		case <-ctx.Done():
		}
		<-ctx.Done()
		return ctx.Err()
	}
	msgs := make(chan tea.Msg, 16)
	go runConsoleGoroutine(ctx, func(m tea.Msg) { msgs <- m }, src)

	m := newTestModel(fixedClock(at(time.Second)))
	apply := func(until func(tea.Msg) bool) {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case msg := <-msgs:
				m = send(m, msg)
				if until(msg) {
					return
				}
			case <-timeout:
				t.Fatalf("runner did not deliver the expected message within 5s; view:\n%s", viewText(m))
			}
		}
	}
	sawBanner, sawDone := false, false
	apply(func(msg tea.Msg) bool {
		switch msg := msg.(type) {
		case consoleReconnectMsg:
			sawBanner = msg.reconnecting
		case consoleBackfillDoneMsg:
			sawDone = true
		}
		return sawBanner && sawDone
	})
	view := viewText(m)
	if !strings.Contains(view, "reconnecting") || strings.Contains(view, "catching up") {
		t.Errorf("while the source retries, want the reconnect banner and no catch-up placeholder:\n%s", view)
	}

	close(connected)
	apply(func(msg tea.Msg) bool {
		r, ok := msg.(consoleReconnectMsg)
		return ok && !r.reconnecting
	})
	if view := viewText(m); strings.Contains(view, "reconnecting") {
		t.Errorf("banner should clear once the source is connected again:\n%s", view)
	}
}
