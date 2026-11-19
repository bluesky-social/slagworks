package worker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// CastManager runs one persistent SOL cast pump per active machine,
// reconciling its pump set against the cache on every Reconcile() call
// (driven by snapshot apply). It is the asciinema-cast successor to the
// deleted SOLManager: same per-machine-pump-with-reconcile shape, but
// each pump is a CastPump streaming asciicast frames to global rather
// than source=sol log envelopes.
//
// Per-pump independence: a dead BMC backs off in its own goroutine
// without affecting siblings. Lock contention is scoped to the
// manager's map; pump goroutines share no state.
//
// Pump-set membership (v0): keyed strictly on ListActiveMachineIDs() —
// one pump per ACTIVE machine. A machine leaving the active set tears
// its pump down. This is deliberate for v0: it avoids spawning
// ipmitool against powered-off/credless hosts, and matches the
// "capture every byte from the associated kworker" cheat while the
// "exactly one site worker owns each console" ownership mechanism is
// deferred.
type CastManager struct {
	dialer CastDialer
	sink   CastSink
	resume CastResumeSource
	cache  CastMachineLister
	log    *slog.Logger

	rootCtx    context.Context
	rootCancel context.CancelFunc

	// reconcileMu serializes whole Reconcile passes. The site worker
	// calls Reconcile from the snapshot-applied callback and directly at
	// startup, on different goroutines. A pass reads the active set
	// before taking mu and drains dropped pumps after releasing it, so
	// without this an older read could commit after a newer one, and a
	// re-add could spawn while the dropped pump still holds the SOL
	// session. Shutdown does not take it, so it never waits behind a
	// drain.
	reconcileMu sync.Mutex

	mu    sync.Mutex
	pumps map[uuid.UUID]*castPumpHandle

	reconnectBackoff    time.Duration
	maxReconnectBackoff time.Duration
	fastFailThreshold   time.Duration
	silenceThreshold    time.Duration
	silenceCheckEvery   time.Duration
}

// CastMachineLister is the cache contract CastManager needs (*Cache, or
// AMTOnlyCastLister on a worker without ipmitool). Mirrors
// the deleted SOLManager's machineLister; defined locally to avoid an
// import cycle (model imports worker).
type CastMachineLister interface {
	ListActiveMachineIDs() []uuid.UUID
	// MachineState returns the machine's current MachineState string,
	// or "" if unknown. Used by the per-pump silence watchdog to
	// suppress reconnect churn when the chassis is intentionally down.
	MachineState(machineID uuid.UUID) string
}

type castPumpHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// CastManagerConfig collects the dependencies CastManager needs.
//
// Dialer is the production TransportCastDialer or a test fake; Sink and
// Resume are the site worker's WSConn in production. Log nil defaults to
// slog.Default().
type CastManagerConfig struct {
	Dialer CastDialer
	Sink   CastSink
	Resume CastResumeSource
	Cache  CastMachineLister
	Log    *slog.Logger
}

// NewCastManager returns an unstarted manager. Call Start, then drive
// Reconcile (typically via the cache's snapshot-applied callback).
func NewCastManager(cfg CastManagerConfig) *CastManager {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &CastManager{
		dialer:              cfg.Dialer,
		sink:                cfg.Sink,
		resume:              cfg.Resume,
		cache:               cfg.Cache,
		log:                 log,
		pumps:               make(map[uuid.UUID]*castPumpHandle),
		reconnectBackoff:    time.Second,
		maxReconnectBackoff: 60 * time.Second,
		fastFailThreshold:   2 * time.Second,
		silenceThreshold:    5 * time.Minute,
		silenceCheckEvery:   30 * time.Second,
	}
}

// SetBackoff overrides the per-pump reconnect backoff. Test-only.
func (m *CastManager) SetBackoff(d time.Duration) {
	m.reconnectBackoff = d
}

// SetSilenceWatchdog overrides the per-pump silence detector schedule.
// Test-only — production leaves the defaults alone.
func (m *CastManager) SetSilenceWatchdog(threshold, checkEvery time.Duration) {
	m.silenceThreshold = threshold
	m.silenceCheckEvery = checkEvery
}

// Start binds the manager to a parent ctx. Callers invoke Reconcile
// (wired to snapshot apply) afterward to bring up pumps.
func (m *CastManager) Start(parent context.Context) {
	m.rootCtx, m.rootCancel = context.WithCancel(parent)
}

// Reconcile aligns the pump set with the cache's active machines.
// Idempotent. Safe to call from several goroutines: passes run one at a
// time, from the active-set read through the dropped-pump drain, so the
// pump set ends at the set the last pass read and a re-add waits out an
// earlier pass's drain. No-op before Start or after Shutdown.
func (m *CastManager) Reconcile() {
	if m.rootCtx == nil {
		return
	}
	if err := m.rootCtx.Err(); err != nil {
		return
	}
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	want := make(map[uuid.UUID]struct{})
	for _, id := range m.cache.ListActiveMachineIDs() {
		want[id] = struct{}{}
	}

	m.mu.Lock()
	// Re-check cancellation INSIDE the lock. The early-return above is a
	// cheap fast path, but Shutdown can slip in between it and this Lock:
	// Shutdown cancels rootCtx and clears the map (also under m.mu), so
	// without this re-check we'd spawn pumps into a map Shutdown already
	// drained — they'd exit immediately on the cancelled ctx but their
	// handles would linger forever, since Shutdown already returned
	// without waiting on them. Holding the lock makes Shutdown and this
	// mutually exclusive: we either see the cancelled ctx and bail, or we
	// spawn before Shutdown runs and Shutdown later cancels our pumps.
	if m.rootCtx.Err() != nil {
		m.mu.Unlock()
		return
	}
	// Tear down pumps for machines that left the active set. Cancel
	// under the lock; wait outside it so ipmitool exit doesn't block
	// concurrent ops on m.mu. Waiting matters for drop-then-re-add
	// (snapshot churn): without it, the new pump races the old one
	// for the BMC's single SOL slot.
	dropped := make([]chan struct{}, 0)
	droppedIDs := make([]uuid.UUID, 0)
	for id, h := range m.pumps {
		if _, ok := want[id]; ok {
			continue
		}
		h.cancel()
		dropped = append(dropped, h.done)
		droppedIDs = append(droppedIDs, id)
		delete(m.pumps, id)
	}
	spawnedIDs := make([]uuid.UUID, 0)
	for id := range want {
		if _, ok := m.pumps[id]; ok {
			continue
		}
		m.pumps[id] = m.spawnPump(id)
		spawnedIDs = append(spawnedIDs, id)
	}
	totalActive := len(m.pumps)
	m.mu.Unlock()

	if len(spawnedIDs) > 0 || len(droppedIDs) > 0 {
		m.log.Info("cast reconcile",
			"active_machines", len(want),
			"pumps_total", totalActive,
			"spawned", spawnedIDs,
			"dropped", droppedIDs,
		)
	} else {
		m.log.Debug("cast reconcile (no changes)",
			"active_machines", len(want),
			"pumps_total", totalActive,
		)
	}

	// Drain dropped pumps without holding the lock, against ONE shared
	// 2s deadline — not 2s per pump. This runs on the snapshot-apply
	// path: a mass drop (site-config change, machine sweep) with a
	// serial per-pump wait would stall apply for minutes. Pumps exit
	// concurrently anyway (all were cancelled above); stragglers are
	// logged and left to the parent ctx.
	if len(dropped) > 0 {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		expired := false // timer.C fires once; after that only poll
		stragglers := 0
		for _, d := range dropped {
			if !expired {
				select {
				case <-d:
					continue
				case <-timer.C:
					expired = true
				}
			}
			select {
			case <-d:
			default:
				stragglers++
			}
		}
		if stragglers > 0 {
			m.log.Warn("dropped cast pumps did not exit within 2s", "count", stragglers)
		}
	}
}

// Shutdown stops every pump and waits up to 5s total for the child
// ipmitool processes to exit. Idempotent.
func (m *CastManager) Shutdown() {
	if m.rootCancel != nil {
		m.rootCancel()
	}
	m.mu.Lock()
	handles := make([]*castPumpHandle, 0, len(m.pumps))
	for _, h := range m.pumps {
		handles = append(handles, h)
		h.cancel()
	}
	m.pumps = make(map[uuid.UUID]*castPumpHandle)
	m.mu.Unlock()

	// One shared 5s deadline for the whole pump set (pumps exit
	// concurrently; a serial per-pump wait would hold shutdown for
	// 5s x N under a wedged ipmitool fleet).
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	expired := false
	stragglers := 0
	for _, h := range handles {
		if !expired {
			select {
			case <-h.done:
				continue
			case <-timer.C:
				expired = true
			}
		}
		select {
		case <-h.done:
		default:
			stragglers++
		}
	}
	if stragglers > 0 {
		m.log.Warn("cast pumps did not exit within 5s during shutdown", "count", stragglers)
	}
}

func (m *CastManager) spawnPump(machineID uuid.UUID) *castPumpHandle {
	// gosec G118 false positive: cancel is stored in castPumpHandle.cancel
	// and called by Reconcile (drop) or Shutdown (teardown).
	ctx, cancel := context.WithCancel(m.rootCtx) //nolint:gosec // G118: cancel stored in handle, called by Reconcile/Shutdown
	done := make(chan struct{})

	mid := machineID
	pump := NewCastPump(CastPumpConfig{
		MachineID:           mid,
		Dialer:              m.dialer,
		Resume:              m.resume,
		Sink:                m.sink,
		Logger:              m.log,
		ReconnectBackoff:    m.reconnectBackoff,
		MaxReconnectBackoff: m.maxReconnectBackoff,
		FastFailThreshold:   m.fastFailThreshold,
		SilenceThreshold:    m.silenceThreshold,
		SilenceCheckEvery:   m.silenceCheckEvery,
		// StateFunc gates the silence watchdog on machine state; nil
		// would disable it, so always supply it.
		StateFunc: func() string { return m.cache.MachineState(mid) },
	})

	go func() {
		defer close(done)
		pump.Run(ctx)
	}()
	return &castPumpHandle{cancel: cancel, done: done}
}
