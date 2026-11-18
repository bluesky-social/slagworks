package hubhandlers

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

type fakeCastStore struct {
	mu sync.Mutex

	// machineID -> cast id; lazily assigned
	castIDs map[uuid.UUID]int64
	nextID  int64

	// Captured per-cast events
	events map[int64][]store.CastEventInput

	// Resume offset per machine (test sets this)
	resume map[uuid.UUID]store.CastResumeOffset

	// Failure injection
	ensureErr error
	appendErr error
	resumeErr error

	ensureCalls int
}

func newFakeCastStore() *fakeCastStore {
	return &fakeCastStore{
		castIDs: map[uuid.UUID]int64{},
		events:  map[int64][]store.CastEventInput{},
		resume:  map[uuid.UUID]store.CastResumeOffset{},
	}
}

func (s *fakeCastStore) EnsureConsoleCast(machineID uuid.UUID) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureCalls++
	if s.ensureErr != nil {
		return 0, s.ensureErr
	}
	if id, ok := s.castIDs[machineID]; ok {
		return id, nil
	}
	s.nextID++
	s.castIDs[machineID] = s.nextID
	return s.nextID, nil
}

func (s *fakeCastStore) AppendConsoleCastEvents(_ context.Context, batch store.CastEventBatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return s.appendErr
	}
	s.events[batch.CastID] = append(s.events[batch.CastID], batch.Events...)
	return nil
}

func (s *fakeCastStore) CastResumeOffsetForMachine(machineID uuid.UUID) (store.CastResumeOffset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resumeErr != nil {
		return store.CastResumeOffset{}, s.resumeErr
	}
	if r, ok := s.resume[machineID]; ok {
		return r, nil
	}
	return store.CastResumeOffset{}, nil
}

func (s *fakeCastStore) eventsFor(t *testing.T, castID int64) []store.CastEventInput {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.CastEventInput, len(s.events[castID]))
	copy(out, s.events[castID])
	return out
}

// TestCastBatch_HappyPath: a batch with frames for one machine
// gets ensured once, then appended in order.
func TestCastBatch_HappyPath(t *testing.T) {
	st := newFakeCastStore()
	h := NewCastBatch(st)
	mid := uuid.New()

	if err := h.HandleCastBatch(context.Background(), "worker-1", ws.CastBatchMsg{
		Frames: []ws.CastFrameMsg{
			{MachineID: mid, TUs: 100, Data: []byte("a")},
			{MachineID: mid, TUs: 200, Data: []byte("b")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	castID := st.castIDs[mid]
	st.mu.Unlock()
	if castID == 0 {
		t.Fatal("expected cast id assigned")
	}
	got := st.eventsFor(t, castID)
	if len(got) != 2 || string(got[0].Data) != "a" || string(got[1].Data) != "b" {
		t.Errorf("events = %+v, want [a b]", got)
	}
}

// TestCastBatch_GroupsByMachine: a single batch with frames for two
// machines lands as two separate cast inserts, each under the
// correct cast id.
func TestCastBatch_GroupsByMachine(t *testing.T) {
	st := newFakeCastStore()
	h := NewCastBatch(st)
	a, b := uuid.New(), uuid.New()

	if err := h.HandleCastBatch(context.Background(), "worker-1", ws.CastBatchMsg{
		Frames: []ws.CastFrameMsg{
			{MachineID: a, TUs: 1, Data: []byte("alpha")},
			{MachineID: b, TUs: 2, Data: []byte("beta")},
			{MachineID: a, TUs: 3, Data: []byte("alpha2")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	castA := st.castIDs[a]
	castB := st.castIDs[b]
	st.mu.Unlock()
	if castA == castB {
		t.Errorf("two machines got the same cast id: %d", castA)
	}
	gotA := st.eventsFor(t, castA)
	gotB := st.eventsFor(t, castB)
	if len(gotA) != 2 || len(gotB) != 1 {
		t.Errorf("event counts: A=%d B=%d, want 2 1", len(gotA), len(gotB))
	}
}

// TestCastBatch_CachesCastID: subsequent batches for the same
// machine don't re-call EnsureConsoleCast.
func TestCastBatch_CachesCastID(t *testing.T) {
	st := newFakeCastStore()
	h := NewCastBatch(st)
	mid := uuid.New()

	for i := range 5 {
		if err := h.HandleCastBatch(context.Background(), "worker-1", ws.CastBatchMsg{
			Frames: []ws.CastFrameMsg{{MachineID: mid, TUs: int64(i), Data: []byte{'a'}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if st.ensureCalls != 1 {
		t.Errorf("EnsureConsoleCast called %d times, want 1 (cache should kick in)", st.ensureCalls)
	}
}

// TestCastBatch_AppendErrorContinuesOtherMachines: a per-machine
// append failure logs but doesn't abort the batch.
func TestCastBatch_AppendErrorContinuesOtherMachines(t *testing.T) {
	st := newFakeCastStore()
	st.appendErr = errors.New("simulated pg failure")
	h := NewCastBatch(st)
	a, b := uuid.New(), uuid.New()

	if err := h.HandleCastBatch(context.Background(), "worker-1", ws.CastBatchMsg{
		Frames: []ws.CastFrameMsg{
			{MachineID: a, TUs: 1, Data: []byte("alpha")},
			{MachineID: b, TUs: 2, Data: []byte("beta")},
		},
	}); err != nil {
		t.Fatalf("HandleCastBatch should swallow per-machine failures, got %v", err)
	}
	// Both machines should have had EnsureConsoleCast attempted.
	if st.ensureCalls != 2 {
		t.Errorf("EnsureConsoleCast called %d times, want 2", st.ensureCalls)
	}
}

// TestCastResume_HappyPath: the handler ensures the cast (so a
// brand-new machine gets a row) then returns the resume offset.
func TestCastResume_HappyPath(t *testing.T) {
	st := newFakeCastStore()
	mid := uuid.New()
	st.resume[mid] = store.CastResumeOffset{
		CastID: 7, Width: 80, Height: 24, PrevMaxUs: 12345,
	}
	h := NewCastResume(st)

	resp, err := h.HandleCastResume(context.Background(), "worker-1", ws.CastResumeRequestMsg{
		MachineID: mid,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.PrevMaxUs != 12345 {
		t.Errorf("PrevMaxUs = %d, want 12345", resp.PrevMaxUs)
	}
	if resp.Width != 80 || resp.Height != 24 {
		t.Errorf("size = %dx%d, want 80x24", resp.Width, resp.Height)
	}
	if st.ensureCalls != 1 {
		t.Errorf("EnsureConsoleCast called %d times, want 1", st.ensureCalls)
	}
}
