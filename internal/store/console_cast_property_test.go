package store

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"
)

// Property tests for the console cast store against Postgres.
//
//   - Resume offset never goes below MAX(t_us) of the cast (the pump's
//     next frame must sort after everything stored), and is
//     non-decreasing across successive calls with no new events.
//   - CastEventsSince(floor, limit) equals the model: events with
//     t_us >= floor sorted by t_us, truncated to limit; paging with
//     floor = last t_us + 1 (what the stream does) reproduces the
//     unpaged list when t_us values are distinct.
//   - LiveEdgeCastFloor = max(t_us)+1 (0 for an empty cast).
//   - Casts are isolated: events appended under one machine's cast never
//     show up under another's.

// clearConsoleCasts empties the cast tables between cases (events go
// with their cast, ON DELETE CASCADE). testStore truncates every table,
// which costs far more per case than deleting the few rows these
// properties write.
func clearConsoleCasts(t *rapid.T, s *Store) {
	if err := s.db.Exec(`DELETE FROM console_casts`).Error; err != nil {
		t.Fatalf("clear console casts: %v", err)
	}
}

func TestConsoleCast_Property_ResumeAndScan(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	rapid.Check(t, func(rt *rapid.T) {
		clearConsoleCasts(rt, s)
		machines := []uuid.UUID{uuid.New(), uuid.New()}
		model := map[uuid.UUID][]int64{}
		castIDs := map[uuid.UUID]int64{}
		for _, m := range machines {
			c, err := s.EnsureConsoleCast(m)
			if err != nil {
				rt.Fatal(err)
			}
			again, err := s.EnsureConsoleCast(m)
			if err != nil || again.ID != c.ID || !again.CastStartedAt.Equal(c.CastStartedAt) {
				rt.Fatalf("EnsureConsoleCast not idempotent: %+v vs %+v (%v)", c, again, err)
			}
			castIDs[m] = c.ID
		}
		used := map[int64]bool{}
		nBatches := rapid.IntRange(0, 4).Draw(rt, "nBatches")
		for b := 0; b < nBatches; b++ {
			m := rapid.SampledFrom(machines).Draw(rt, "machine")
			n := rapid.IntRange(1, 15).Draw(rt, "n")
			var batch []CastEventInput
			for i := 0; i < n; i++ {
				tus := rapid.Int64Range(0, 5_000_000).Draw(rt, "tus")
				if used[tus] {
					continue // distinct t_us: the documented per-cast invariant
				}
				used[tus] = true
				batch = append(batch, CastEventInput{TUs: tus, Data: []byte(fmt.Sprintf("%d", tus))})
				model[m] = append(model[m], tus)
			}
			if err := s.AppendConsoleCastEvents(ctx, CastEventBatch{CastID: castIDs[m], Events: batch}); err != nil {
				rt.Fatal(err)
			}
		}
		for _, m := range machines {
			want := append([]int64(nil), model[m]...)
			sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
			var maxT int64 = -1
			if len(want) > 0 {
				maxT = want[len(want)-1]
			}

			r1, err := s.CastResumeOffsetForMachine(m)
			if err != nil {
				rt.Fatal(err)
			}
			if r1.PrevMaxUs < maxT {
				rt.Fatalf("resume %d below max t_us %d", r1.PrevMaxUs, maxT)
			}
			r2, err := s.CastResumeOffsetForMachine(m)
			if err != nil {
				rt.Fatal(err)
			}
			if r2.PrevMaxUs < r1.PrevMaxUs {
				rt.Fatalf("resume went backwards with no new events: %d then %d", r1.PrevMaxUs, r2.PrevMaxUs)
			}
			edge, err := s.LiveEdgeCastFloor(ctx, castIDs[m])
			if err != nil || edge != maxT+1 {
				rt.Fatalf("live edge %d (%v), want %d", edge, err, maxT+1)
			}

			floor := rapid.Int64Range(-1, 5_000_001).Draw(rt, "floor")
			limit := rapid.IntRange(0, 25).Draw(rt, "limit")
			var exp []int64
			for _, v := range want {
				if v >= floor {
					exp = append(exp, v)
				}
			}
			if limit > 0 && len(exp) > limit {
				exp = exp[:limit]
			}
			rows, err := s.CastEventsSince(ctx, castIDs[m], floor, limit)
			if err != nil {
				rt.Fatal(err)
			}
			got := make([]int64, len(rows))
			for i, r := range rows {
				got[i] = r.TUs
				if string(r.Data) != fmt.Sprintf("%d", r.TUs) || r.CastID != castIDs[m] {
					rt.Fatalf("row %+v has foreign data or cast", r)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(exp) {
				rt.Fatalf("CastEventsSince(floor=%d, limit=%d)\n got  %v\n want %v", floor, limit, got, exp)
			}

			page := rapid.IntRange(1, 7).Draw(rt, "page")
			var paged []int64
			cursor := int64(0)
			for {
				rows, err := s.CastEventsSince(ctx, castIDs[m], cursor, page)
				if err != nil {
					rt.Fatal(err)
				}
				for _, r := range rows {
					paged = append(paged, r.TUs)
					cursor = r.TUs + 1
				}
				if len(rows) < page {
					break
				}
			}
			if fmt.Sprint(paged) != fmt.Sprint(want) {
				rt.Fatalf("paged scan %v != %v", paged, want)
			}
		}
	})
}

// TestConsoleCast_Property_SinceFloorWithinWindow: SinceFloorByDuration with
// a window covering everything returns the minimum t_us of the earliest
// received batch; a zero-length window falls back to the live edge.
func TestConsoleCast_Property_SinceFloorWithinWindow(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	rapid.Check(t, func(rt *rapid.T) {
		clearConsoleCasts(rt, s)
		m := uuid.New()
		c, err := s.EnsureConsoleCast(m)
		if err != nil {
			rt.Fatal(err)
		}
		n := rapid.IntRange(0, 10).Draw(rt, "n")
		base := rapid.Int64Range(0, 1000).Draw(rt, "base")
		var first []int64
		var maxT int64 = -1
		for b := 0; b < n; b++ {
			k := rapid.IntRange(1, 4).Draw(rt, "k")
			var ev []CastEventInput
			for i := 0; i < k; i++ {
				base += rapid.Int64Range(1, 1000).Draw(rt, "dt")
				ev = append(ev, CastEventInput{TUs: base, Data: []byte("x")})
				maxT = base
			}
			if b == 0 {
				for _, e := range ev {
					first = append(first, e.TUs)
				}
			}
			if err := s.AppendConsoleCastEvents(ctx, CastEventBatch{CastID: c.ID, Events: ev}); err != nil {
				rt.Fatal(err)
			}
			time.Sleep(time.Millisecond) // distinct received_at per batch
		}
		got, err := s.SinceFloorByDuration(ctx, c.ID, time.Hour)
		if err != nil {
			rt.Fatal(err)
		}
		want := maxT + 1
		if len(first) > 0 {
			want = first[0]
		}
		if got != want {
			rt.Fatalf("SinceFloorByDuration(1h) = %d, want %d", got, want)
		}
		// A negative window is clamped to zero, and every event was
		// received in an earlier transaction than the query's now(), so
		// the window is empty.
		got0, err := s.SinceFloorByDuration(ctx, c.ID, -time.Minute)
		if err != nil {
			rt.Fatal(err)
		}
		if got0 != maxT+1 {
			rt.Fatalf("SinceFloorByDuration(<0) = %d, want live edge %d", got0, maxT+1)
		}
	})
}
