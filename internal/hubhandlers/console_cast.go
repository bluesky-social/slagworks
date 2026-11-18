// Console-cast hub handlers: persist asciinema cast frames pushed
// by site workers, and answer the resume-offset RPC pump sessions
// fire on every start.
package hubhandlers

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// CastStore is the subset of store.Store the cast handlers touch.
// EnsureConsoleCast returns just the cast id (not the full
// *model.ConsoleCast) so test fakes don't need to import model.
type CastStore interface {
	EnsureConsoleCast(machineID uuid.UUID) (int64, error)
	AppendConsoleCastEvents(ctx context.Context, batch store.CastEventBatch) error
	CastResumeOffsetForMachine(machineID uuid.UUID) (store.CastResumeOffset, error)
}

// StoreAdapter wraps *store.Store into CastStore. Production wires
// hubhandlers.NewCastBatch(StoreAdapter{S: st}); tests substitute a
// hand-rolled fake.
type StoreAdapter struct{ S *store.Store }

func (a StoreAdapter) EnsureConsoleCast(machineID uuid.UUID) (int64, error) {
	c, err := a.S.EnsureConsoleCast(machineID)
	if err != nil {
		return 0, err
	}
	return c.ID, nil
}

func (a StoreAdapter) AppendConsoleCastEvents(ctx context.Context, batch store.CastEventBatch) error {
	return a.S.AppendConsoleCastEvents(ctx, batch)
}

func (a StoreAdapter) CastResumeOffsetForMachine(machineID uuid.UUID) (store.CastResumeOffset, error) {
	return a.S.CastResumeOffsetForMachine(machineID)
}

// CastBatch persists incoming TypeCastBatch frames into pg. Looks
// up cast_id once per machine_id and caches it (the cast row is
// upserted on first sight; afterwards it's a stable pg id).
type CastBatch struct {
	store CastStore
	log   *slog.Logger

	mu      sync.RWMutex
	castIDs map[uuid.UUID]int64
}

func NewCastBatch(s CastStore) *CastBatch {
	return &CastBatch{
		store:   s,
		log:     slog.Default(),
		castIDs: map[uuid.UUID]int64{},
	}
}

func (h *CastBatch) SetLogger(l *slog.Logger) { h.log = l }

// HandleCastBatch implements ws.CastBatchHandler.
func (h *CastBatch) HandleCastBatch(ctx context.Context, _ string, msg ws.CastBatchMsg) error {
	if len(msg.Frames) == 0 {
		return nil
	}
	// Group frames by machine_id so we can do one ensure + one bulk
	// insert per machine, regardless of how the relay batched them.
	byMachine := map[uuid.UUID][]ws.CastFrameMsg{}
	for _, f := range msg.Frames {
		byMachine[f.MachineID] = append(byMachine[f.MachineID], f)
	}
	for mid, frames := range byMachine {
		castID, err := h.castIDFor(mid)
		if err != nil {
			h.log.Warn("cast_batch: ensure cast failed",
				"machine_id", mid, "err", err)
			continue
		}
		events := make([]store.CastEventInput, len(frames))
		for i, f := range frames {
			events[i] = store.CastEventInput{TUs: f.TUs, Data: f.Data}
		}
		if err := h.store.AppendConsoleCastEvents(ctx, store.CastEventBatch{
			CastID: castID,
			Events: events,
		}); err != nil {
			h.log.Warn("cast_batch: append events failed",
				"machine_id", mid, "cast_id", castID, "frames", len(frames), "err", err)
			// Don't return — keep going for other machines.
		}
	}
	return nil
}

// castIDFor resolves and caches the cast id for a machine. Cache
// hits are hot-path (typical batch is a few frames for one machine).
func (h *CastBatch) castIDFor(machineID uuid.UUID) (int64, error) {
	h.mu.RLock()
	if id, ok := h.castIDs[machineID]; ok {
		h.mu.RUnlock()
		return id, nil
	}
	h.mu.RUnlock()
	id, err := h.store.EnsureConsoleCast(machineID)
	if err != nil {
		return 0, fmt.Errorf("ensure cast: %w", err)
	}
	h.mu.Lock()
	h.castIDs[machineID] = id
	h.mu.Unlock()
	return id, nil
}

// CastResume answers the worker→global RPC pumps fire on every
// session start.
type CastResume struct {
	store CastStore
	log   *slog.Logger
}

func NewCastResume(s CastStore) *CastResume {
	return &CastResume{store: s, log: slog.Default()}
}

func (h *CastResume) SetLogger(l *slog.Logger) { h.log = l }

// HandleCastResume implements ws.CastResumeHandler. EnsureConsoleCast
// upfront so a brand-new machine still gets a usable response.
func (h *CastResume) HandleCastResume(_ context.Context, _ string, msg ws.CastResumeRequestMsg) (ws.CastResumeResponseMsg, error) {
	if _, err := h.store.EnsureConsoleCast(msg.MachineID); err != nil {
		return ws.CastResumeResponseMsg{}, fmt.Errorf("ensure cast: %w", err)
	}
	out, err := h.store.CastResumeOffsetForMachine(msg.MachineID)
	if err != nil {
		return ws.CastResumeResponseMsg{}, fmt.Errorf("resume offset: %w", err)
	}
	return ws.CastResumeResponseMsg{
		CastID:        out.CastID,
		CastStartedAt: out.CastStartedAt,
		Width:         out.Width,
		Height:        out.Height,
		PrevMaxUs:     out.PrevMaxUs,
	}, nil
}
