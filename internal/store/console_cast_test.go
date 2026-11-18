package store

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// mustEnsureCast creates the parent machine row + a console_cast row,
// returning the cast id for the per-test cases below. Tests that
// don't care about machine semantics use this so we never have to
// recompute the boilerplate.
func mustEnsureCast(t *testing.T, st *Store) (uuid.UUID, *model.ConsoleCast) {
	t.Helper()
	mid := uuid.New()
	if err := st.CreateMachine(&model.Machine{
		ID: mid, NetBoxSerial: "SN-cast-" + mid.String()[:8],
		State: model.MachineStateNew,
	}); err != nil {
		t.Fatalf("create machine: %v", err)
	}
	cast, err := st.EnsureConsoleCast(mid)
	if err != nil {
		t.Fatalf("ensure cast: %v", err)
	}
	return mid, cast
}

// TestEnsureConsoleCast_Idempotent: first call inserts, subsequent
// calls return the same row without rewriting it. Mirrors the design
// doc's "DO NOTHING + SELECT fallback" shape — no DO UPDATE write
// amplification on conflict.
func TestEnsureConsoleCast_Idempotent(t *testing.T) {
	st := testStore(t)
	mid := uuid.New()
	if err := st.CreateMachine(&model.Machine{
		ID: mid, NetBoxSerial: "SN-cast-idem", State: model.MachineStateNew,
	}); err != nil {
		t.Fatal(err)
	}

	c1, err := st.EnsureConsoleCast(mid)
	if err != nil {
		t.Fatalf("first EnsureConsoleCast: %v", err)
	}
	if c1.ID == 0 || c1.MachineID != mid {
		t.Errorf("first cast = %+v", c1)
	}
	if c1.Width != 80 || c1.Height != 24 {
		t.Errorf("default width/height = %dx%d, want 80x24", c1.Width, c1.Height)
	}

	c2, err := st.EnsureConsoleCast(mid)
	if err != nil {
		t.Fatalf("second EnsureConsoleCast: %v", err)
	}
	if c2.ID != c1.ID {
		t.Errorf("second call returned different cast id: %d vs %d", c2.ID, c1.ID)
	}
	if !c2.CastStartedAt.Equal(c1.CastStartedAt) {
		t.Errorf("cast_started_at changed across calls: %v -> %v",
			c1.CastStartedAt, c2.CastStartedAt)
	}
}

// TestAppendConsoleCastEvents_RoundTrip: insert a small batch, fetch
// it back via CastEventsSince, assert ordering + payload + t_us.
func TestAppendConsoleCastEvents_RoundTrip(t *testing.T) {
	st := testStore(t)
	_, cast := mustEnsureCast(t, st)

	batch := CastEventBatch{
		CastID: cast.ID,
		Events: []CastEventInput{
			{TUs: 1_000, Data: []byte("first")},
			{TUs: 2_500, Data: []byte("second")},
			{TUs: 9_000, Data: []byte("third")},
		},
	}
	if err := st.AppendConsoleCastEvents(context.Background(), batch); err != nil {
		t.Fatalf("AppendConsoleCastEvents: %v", err)
	}

	rows, err := st.CastEventsSince(context.Background(), cast.ID, 0, 0)
	if err != nil {
		t.Fatalf("CastEventsSince: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	for i, want := range batch.Events {
		if rows[i].TUs != want.TUs || string(rows[i].Data) != string(want.Data) {
			t.Errorf("row %d: got (t_us=%d data=%q), want (t_us=%d data=%q)",
				i, rows[i].TUs, rows[i].Data, want.TUs, want.Data)
		}
	}

	// since_us=2500 drops the first event, returns the other two.
	rows, err = st.CastEventsSince(context.Background(), cast.ID, 2_500, 0)
	if err != nil {
		t.Fatalf("CastEventsSince(2500): %v", err)
	}
	if len(rows) != 2 || rows[0].TUs != 2_500 || rows[1].TUs != 9_000 {
		t.Errorf("since_us=2500 = %+v, want [2500, 9000]", rows)
	}
}

// TestLiveEdgeCastFloor: NULL fallback (empty cast) returns 0;
// otherwise returns MAX(t_us)+1. The +1 matters: setting the floor
// to exactly MAX(t_us) would still match the historical event with
// t_us >= floor, streaming a stale event outside the requested
// window. See design doc §API for the rationale.
func TestLiveEdgeCastFloor(t *testing.T) {
	st := testStore(t)
	_, cast := mustEnsureCast(t, st)

	// Empty cast: COALESCE(MAX(t_us), -1) + 1 = 0.
	floor, err := st.LiveEdgeCastFloor(context.Background(), cast.ID)
	if err != nil {
		t.Fatalf("LiveEdgeCastFloor (empty): %v", err)
	}
	if floor != 0 {
		t.Errorf("empty cast floor = %d, want 0", floor)
	}

	// With events: MAX(t_us) + 1.
	if err := st.AppendConsoleCastEvents(context.Background(), CastEventBatch{
		CastID: cast.ID,
		Events: []CastEventInput{
			{TUs: 100, Data: []byte("a")},
			{TUs: 250, Data: []byte("b")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	floor, err = st.LiveEdgeCastFloor(context.Background(), cast.ID)
	if err != nil {
		t.Fatalf("LiveEdgeCastFloor (populated): %v", err)
	}
	if floor != 251 {
		t.Errorf("populated cast floor = %d, want 251", floor)
	}
}

// TestSinceFloorByDuration: events inside the wall-clock window
// produce the earliest in-window t_us; events outside produce the
// live-edge fallback (so we don't quietly dump the entire history
// when the window finds nothing).
func TestSinceFloorByDuration(t *testing.T) {
	st := testStore(t)
	_, cast := mustEnsureCast(t, st)

	// Insert two batches: one "old" (received_at backdated to 1h
	// ago via a follow-up UPDATE) and one fresh.
	if err := st.AppendConsoleCastEvents(context.Background(), CastEventBatch{
		CastID: cast.ID,
		Events: []CastEventInput{{TUs: 100, Data: []byte("old-1")}, {TUs: 200, Data: []byte("old-2")}},
	}); err != nil {
		t.Fatal(err)
	}
	// Backdate the rows we just inserted.
	if err := st.db.Exec(
		"UPDATE console_cast_events SET received_at = now() - interval '1 hour' WHERE cast_id = ?",
		cast.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.AppendConsoleCastEvents(context.Background(), CastEventBatch{
		CastID: cast.ID,
		Events: []CastEventInput{{TUs: 1_000, Data: []byte("new-1")}, {TUs: 1_500, Data: []byte("new-2")}},
	}); err != nil {
		t.Fatal(err)
	}

	// 5min window: only the fresh batch is in-window. Floor =
	// the earliest t_us in that batch (1000).
	floor, err := st.SinceFloorByDuration(context.Background(), cast.ID, 5*time.Minute)
	if err != nil {
		t.Fatalf("SinceFloorByDuration(5m): %v", err)
	}
	if floor != 1_000 {
		t.Errorf("5m window floor = %d, want 1000", floor)
	}

	// 1ms window: nothing in-window. Falls back to live-edge:
	// MAX(t_us)+1 = 1501.
	floor, err = st.SinceFloorByDuration(context.Background(), cast.ID, time.Millisecond)
	if err != nil {
		t.Fatalf("SinceFloorByDuration(1ms): %v", err)
	}
	if floor != 1_501 {
		t.Errorf("empty-window floor = %d, want 1501 (live edge)", floor)
	}

	// 2h window: covers the old batch too. Earliest t_us = 100.
	floor, err = st.SinceFloorByDuration(context.Background(), cast.ID, 2*time.Hour)
	if err != nil {
		t.Fatalf("SinceFloorByDuration(2h): %v", err)
	}
	if floor != 100 {
		t.Errorf("2h window floor = %d, want 100", floor)
	}
}

// TestCastResumeOffsetForMachine: the offset reflects MAX(t_us) plus
// the wall-clock gap since the latest event's received_at. We
// backdate received_at to make the gap measurable, then assert the
// returned offset is at least gap+MAX(t_us). The NTP-step floor
// (gap clamped at 0) is exercised by the same path with a future-
// dated received_at.
func TestCastResumeOffsetForMachine(t *testing.T) {
	st := testStore(t)
	mid, cast := mustEnsureCast(t, st)

	// Empty cast: offset is now() - cast_started_at, which is tiny
	// but non-negative. Just assert it's >= 0.
	out, err := st.CastResumeOffsetForMachine(mid)
	if err != nil {
		t.Fatalf("CastResumeOffsetForMachine (empty): %v", err)
	}
	if out.CastID != cast.ID {
		t.Errorf("CastID = %d, want %d", out.CastID, cast.ID)
	}
	if out.PrevMaxUs < 0 {
		t.Errorf("empty-cast PrevMaxUs = %d, want >= 0", out.PrevMaxUs)
	}

	// With events backdated 5s: PrevMaxUs ≈ MAX(t_us) + 5_000_000.
	if err := st.AppendConsoleCastEvents(context.Background(), CastEventBatch{
		CastID: cast.ID,
		Events: []CastEventInput{{TUs: 7_000, Data: []byte("x")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.db.Exec(
		"UPDATE console_cast_events SET received_at = now() - interval '5 seconds' WHERE cast_id = ?",
		cast.ID).Error; err != nil {
		t.Fatal(err)
	}
	out, err = st.CastResumeOffsetForMachine(mid)
	if err != nil {
		t.Fatalf("CastResumeOffsetForMachine (populated): %v", err)
	}
	wantLow := int64(7_000 + 4_500_000) // 5s gap, allow 500ms slop
	wantHigh := int64(7_000 + 6_000_000)
	if out.PrevMaxUs < wantLow || out.PrevMaxUs > wantHigh {
		t.Errorf("populated PrevMaxUs = %d, want in [%d, %d]",
			out.PrevMaxUs, wantLow, wantHigh)
	}

	// Future-dated received_at simulates a backward NTP step on
	// global. Gap should clamp at 0 (no monotonic violation).
	if err := st.db.Exec(
		"UPDATE console_cast_events SET received_at = now() + interval '1 hour' WHERE cast_id = ?",
		cast.ID).Error; err != nil {
		t.Fatal(err)
	}
	out, err = st.CastResumeOffsetForMachine(mid)
	if err != nil {
		t.Fatalf("CastResumeOffsetForMachine (future-dated): %v", err)
	}
	if out.PrevMaxUs != 7_000 {
		t.Errorf("future-dated received_at: PrevMaxUs = %d, want 7000 (gap clamped at 0)",
			out.PrevMaxUs)
	}
}

// TestCastResumeOffsetForMachine_NotFound: a machine without an
// EnsureConsoleCast call returns ErrConsoleCastNotFound, not a
// silent zero offset.
func TestCastResumeOffsetForMachine_NotFound(t *testing.T) {
	st := testStore(t)
	mid := uuid.New()
	if err := st.CreateMachine(&model.Machine{
		ID: mid, NetBoxSerial: "SN-cast-nf", State: model.MachineStateNew,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := st.CastResumeOffsetForMachine(mid)
	if err == nil {
		t.Fatal("expected ErrConsoleCastNotFound, got nil")
	}
}

// TestCastStorageStats: per-machine byte accounting sums event data
// sizes and reports zero (not absence) for cast rows with no events —
// this is the runaway-console tripwire now that casts are kept forever.
func TestCastStorageStats(t *testing.T) {
	st := testStore(t)

	chattyID, chattyCast := mustEnsureCast(t, st)
	quietID, _ := mustEnsureCast(t, st)

	if err := st.AppendConsoleCastEvents(context.Background(), CastEventBatch{
		CastID: chattyCast.ID,
		Events: []CastEventInput{
			{TUs: 1, Data: []byte("0123456789")},       // 10 bytes
			{TUs: 2, Data: []byte("0123456789abcdef")}, // 16 bytes
		},
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := st.CastStorageStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byMachine := map[uuid.UUID]int64{}
	for _, s := range stats {
		byMachine[s.MachineID] = s.Bytes
	}
	if got := byMachine[chattyID]; got != 26 {
		t.Errorf("chatty machine: got %d bytes want 26", got)
	}
	if got, ok := byMachine[quietID]; !ok || got != 0 {
		t.Errorf("quiet machine: got (%d, present=%v) want (0, true) — zero-event casts must report 0, not vanish", got, ok)
	}
}
