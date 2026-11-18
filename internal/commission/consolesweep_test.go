package commission

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// fakeCastScanner reports a hit for the nonces in `hits`.
type fakeCastScanner struct {
	mu   sync.Mutex
	hits map[string]bool
	err  error
}

func (f *fakeCastScanner) ScanCastEventsForNonce(_ context.Context, _ uuid.UUID, nonce string, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.hits[nonce], nil
}

func sweepFixture() (*fakeNetBox, *model.ConsoleSweep, int, int) {
	const deviceID, typeID = 42, 7
	nb := &fakeNetBox{
		devices: map[int]*netbox.Device{
			deviceID: {ID: deviceID, DeviceType: &netbox.NestedRef{ID: typeID}},
		},
		deviceTypes: map[int]*netbox.DeviceType{
			typeID: {ID: typeID, Model: "NewBox 9000"},
		},
	}
	sweep := &model.ConsoleSweep{Candidates: []model.ConsoleSweepCandidate{
		{TTY: "ttyS0", Baud: 115200, Nonce: "aaaa000011112222"},
		{TTY: "ttyS1", Baud: 115200, Nonce: "bbbb000011112222"},
	}}
	return nb, sweep, deviceID, typeID
}

// fastAttempts collapses the retry ladder so tests don't sleep.
func fastAttempts(t *testing.T) {
	t.Helper()
	orig := sweepResolveAttempts
	sweepResolveAttempts = []time.Duration{0}
	t.Cleanup(func() { sweepResolveAttempts = orig })
}

// TestConsoleSweep_WinnerWrittenToDeviceType: the candidate whose nonce
// reached the cast stream becomes the device-type's kernel_params.
func TestConsoleSweep_WinnerWrittenToDeviceType(t *testing.T) {
	fastAttempts(t)
	nb, sweep, deviceID, typeID := sweepFixture()
	h := New(&fakeStore{}, nb, nil)
	h.SetCastScanner(&fakeCastScanner{hits: map[string]bool{"bbbb000011112222": true}})

	h.resolveConsoleSweep(context.Background(), uuid.New(), deviceID, sweep)

	got := nb.kernelParamsWrites[typeID]
	if got != "console=ttyS1,115200n8" {
		t.Fatalf("device-type write: got %q, want console=ttyS1,115200n8", got)
	}
}

// TestConsoleSweep_NoHitWritesNothing: zero nonce hits (no SOL, wrong
// baud, no serial hardware) must leave the field untouched.
func TestConsoleSweep_NoHitWritesNothing(t *testing.T) {
	fastAttempts(t)
	nb, sweep, deviceID, _ := sweepFixture()
	h := New(&fakeStore{}, nb, nil)
	h.SetCastScanner(&fakeCastScanner{hits: map[string]bool{}})

	h.resolveConsoleSweep(context.Background(), uuid.New(), deviceID, sweep)

	if len(nb.kernelParamsWrites) != 0 {
		t.Fatalf("no nonce landed but the device-type was written: %v", nb.kernelParamsWrites)
	}
}

// TestConsoleSweep_NeverOverwritesConfiguredType: a device-type whose
// slagworks_kernel_params is already set (operator value, or a sibling
// device's sweep that resolved first) must not be overwritten.
func TestConsoleSweep_NeverOverwritesConfiguredType(t *testing.T) {
	fastAttempts(t)
	nb, sweep, deviceID, typeID := sweepFixture()
	nb.deviceTypes[typeID].CustomFields = json.RawMessage(
		`{"slagworks_kernel_params":"console=ttyS0,57600n8"}`)
	h := New(&fakeStore{}, nb, nil)
	h.SetCastScanner(&fakeCastScanner{hits: map[string]bool{"bbbb000011112222": true}})

	h.resolveConsoleSweep(context.Background(), uuid.New(), deviceID, sweep)

	if len(nb.kernelParamsWrites) != 0 {
		t.Fatalf("configured device-type must never be overwritten, got write: %v", nb.kernelParamsWrites)
	}
}

// TestConsoleSweep_NilOrEmptySweepNoOp: reports without sweep data (the
// already-configured verification path) must not touch NetBox.
func TestConsoleSweep_NilOrEmptySweepNoOp(t *testing.T) {
	fastAttempts(t)
	nb, _, deviceID, _ := sweepFixture()
	h := New(&fakeStore{}, nb, nil)
	h.SetCastScanner(&fakeCastScanner{hits: map[string]bool{"aaaa000011112222": true}})

	h.resolveConsoleSweep(context.Background(), uuid.New(), deviceID, nil)
	h.resolveConsoleSweep(context.Background(), uuid.New(), deviceID, &model.ConsoleSweep{})

	if len(nb.kernelParamsWrites) != 0 {
		t.Fatalf("nil/empty sweep must be a no-op, got write: %v", nb.kernelParamsWrites)
	}
}

// countingNetBox wraps fakeNetBox to count kernel-params writes for
// the sibling-race test.
type countingNetBox struct {
	*fakeNetBox
	writes atomic.Int32
}

func (c *countingNetBox) SetDeviceTypeKernelParams(ctx context.Context, id int, kp string) error {
	c.writes.Add(1)
	// Make the write visible to the NEXT reader's only-if-empty check,
	// mirroring real NetBox: after one sibling writes, the field is set.
	c.fakeNetBox.mu.Lock()
	c.fakeNetBox.deviceTypes[id].CustomFields = json.RawMessage(
		`{"slagworks_kernel_params":"` + kp + `"}`)
	c.fakeNetBox.mu.Unlock()
	return c.fakeNetBox.SetDeviceTypeKernelParams(ctx, id, kp)
}

// TestConsoleSweep_SiblingRaceSingleWrite pins the concurrency
// contract: N machines of the SAME device-type resolving sweeps
// concurrently produce exactly ONE device-type write — the check and
// write are serialized under sweepMu, so later siblings observe the
// first sibling's value as "already configured".
func TestConsoleSweep_SiblingRaceSingleWrite(t *testing.T) {
	fastAttempts(t)
	nb, sweep, deviceID, _ := sweepFixture()
	cnb := &countingNetBox{fakeNetBox: nb}
	h := New(&fakeStore{}, cnb, nil)
	h.SetCastScanner(&fakeCastScanner{hits: map[string]bool{"bbbb000011112222": true}})

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := *sweep // per-goroutine copy; candidates shared read-only
			h.resolveConsoleSweep(context.Background(), uuid.New(), deviceID, &s)
		}()
	}
	wg.Wait()

	if got := cnb.writes.Load(); got != 1 {
		t.Fatalf("sibling race: got %d device-type writes, want exactly 1", got)
	}
}

// TestConsoleSweep_ReplayDedup: a worker WS replay of the same
// commission report must not fan out a second resolver goroutine for
// the machine while one is in flight.
func TestConsoleSweep_ReplayDedup(t *testing.T) {
	nb, sweep, deviceID, _ := sweepFixture()
	h := New(&fakeStore{}, nb, nil)

	// Scanner that blocks until released, holding the first resolution
	// in flight while the replay arrives.
	release := make(chan struct{})
	var scans atomic.Int32
	h.SetCastScanner(scannerFunc(func(context.Context, uuid.UUID, string, time.Duration) (bool, error) {
		scans.Add(1)
		<-release
		return false, nil
	}))
	orig := sweepResolveAttempts
	sweepResolveAttempts = []time.Duration{0}
	defer func() { sweepResolveAttempts = orig }()

	machineID := uuid.New()
	h.startSweepResolution(machineID, deviceID, sweep)
	h.startSweepResolution(machineID, deviceID, sweep) // the replay

	// Give the (single) goroutine time to reach the scanner.
	deadline := time.Now().Add(time.Second)
	for scans.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(release)

	// Only the first goroutine ran: one scan per candidate at most,
	// but crucially not double. With 2 candidates and 1 attempt the
	// ceiling for a single resolution is 2.
	deadline = time.Now().Add(time.Second)
	for {
		h.sweepMu.Lock()
		inflight := len(h.sweepInFlight)
		h.sweepMu.Unlock()
		if inflight == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := scans.Load(); got > 2 {
		t.Fatalf("replay was not deduped: %d scans (max 2 for one resolution)", got)
	}
}

// scannerFunc adapts a func to CastScanner.
type scannerFunc func(context.Context, uuid.UUID, string, time.Duration) (bool, error)

func (f scannerFunc) ScanCastEventsForNonce(ctx context.Context, m uuid.UUID, n string, l time.Duration) (bool, error) {
	return f(ctx, m, n, l)
}
