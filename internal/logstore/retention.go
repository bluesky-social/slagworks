package logstore

import (
	"context"
	"log/slog"
	"time"
)

// RetentionPolicy maps an envelope source to how long its rows are
// kept. Sources absent from the map are never deleted.
type RetentionPolicy map[string]time.Duration

// DefaultRetention keeps agent/site/sol envelopes 30 days and global
// envelopes 90 days. log_envelopes is the `slagctl logs` backing store;
// the same lines also flow to Loki via the stdout re-emit, so pg is
// not the only copy. console_cast_events (the SOL audit record) is
// deliberately NOT covered here — casts are kept forever per the
// 2026-07-03 operator ruling.
func DefaultRetention() RetentionPolicy {
	return RetentionPolicy{
		"agent":  30 * 24 * time.Hour,
		"site":   30 * 24 * time.Hour,
		"sol":    30 * 24 * time.Hour,
		"global": 90 * 24 * time.Hour,
	}
}

// RunRetention deletes expired log_envelopes per the policy once per
// interval (first pass fires immediately), until ctx is cancelled.
// Deletion failures are logged and retried next tick — retention is
// best-effort and must never take the server down.
func (s *Store) RunRetention(ctx context.Context, policy RetentionPolicy, interval time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	tick := func() {
		for source, keep := range policy {
			cutoff := time.Now().Add(-keep)
			n, err := s.DeleteOlderThan(ctx, cutoff, source)
			if err != nil {
				log.Error("log retention sweep failed", "source", source, "err", err)
				continue
			}
			if n > 0 {
				log.Info("log retention sweep", "source", source,
					"deleted", n, "cutoff", cutoff.Format(time.RFC3339))
			}
		}
	}
	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
