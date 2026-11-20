package logstore_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// Differential property test: logstore.Query against a slice model of
// the rows inserted, over small pools of machines/ops/sources/levels so
// filters overlap. The model restates the documented filter semantics
// independently: machine OR op when both are set, otherwise equality;
// machine_name on the payload; sources IN; min level and above, in any
// case (an unknown level is an error, not a missing filter); ts >= since
// OR global_received_at >= since;
// id > last_seen; id ASC; LIMIT. Pagination is exercised by re-querying
// with LastSeenID = last id returned until a short page, and must
// reproduce the unpaged result.

type modelRow struct {
	id       int64
	received time.Time // global_received_at, read back from the table
	env      slogkit.Envelope
}

var levelRank = map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}

// modelQuery returns the ids the filter selects, and false when Query
// must refuse the filter.
func modelQuery(rows []modelRow, f logstore.QueryFilter) ([]int64, bool) {
	minR, known := levelRank[strings.ToLower(f.MinLevel)]
	if f.MinLevel != "" && !known {
		return nil, false
	}
	var out []int64
	for _, r := range rows {
		e := r.env
		if f.TraceID != uuid.Nil && e.TraceID != f.TraceID {
			continue
		}
		switch {
		case f.MachineID != uuid.Nil && f.OpID != uuid.Nil:
			if e.MachineID != f.MachineID && e.OpID != f.OpID {
				continue
			}
		case f.MachineID != uuid.Nil:
			if e.MachineID != f.MachineID {
				continue
			}
		case f.OpID != uuid.Nil:
			if e.OpID != f.OpID {
				continue
			}
		}
		if f.MachineName != "" && e.MachineName != f.MachineName {
			continue
		}
		if len(f.Sources) > 0 {
			ok := false
			for _, s := range f.Sources {
				if string(e.Source) == s {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		if f.MinLevel != "" && levelRank[string(e.Level)] < minR {
			continue
		}
		if !f.Since.IsZero() && e.TS.Before(f.Since) && r.received.Before(f.Since) {
			continue
		}
		if r.id <= f.LastSeenID {
			continue
		}
		out = append(out, r.id)
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, true
}

func TestQuery_Property_MatchesModel(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := logstore.New(db)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)
	machines := []uuid.UUID{uuid.Nil, uuid.New(), uuid.New()}
	ops := []uuid.UUID{uuid.Nil, uuid.New(), uuid.New()}
	names := []string{"", "alpha", "beta"}
	sources := []slogkit.Source{"agent", "site", "global"}
	levels := []slogkit.Level{"debug", "info", "warn", "error"}

	rapid.Check(t, func(rt *rapid.T) {
		clearEnvelopes(rt, db)
		var rows []modelRow
		nBatches := rapid.IntRange(1, 4).Draw(rt, "nBatches")
		for b := 0; b < nBatches; b++ {
			n := rapid.IntRange(1, 15).Draw(rt, "batchLen")
			batch := make([]slogkit.Envelope, n)
			for i := range batch {
				batch[i] = slogkit.Envelope{
					TS:          base.Add(time.Duration(rapid.IntRange(-5, 5).Draw(rt, "tsOff")) * time.Minute),
					Level:       rapid.SampledFrom(levels).Draw(rt, "level"),
					Source:      rapid.SampledFrom(sources).Draw(rt, "source"),
					Msg:         fmt.Sprintf("m%d-%d", b, i),
					MachineID:   rapid.SampledFrom(machines).Draw(rt, "machine"),
					MachineName: rapid.SampledFrom(names).Draw(rt, "name"),
					OpID:        rapid.SampledFrom(ops).Draw(rt, "op"),
				}
			}
			if err := st.InsertBatch(ctx, batch); err != nil {
				rt.Fatalf("insert: %v", err)
			}
			for _, e := range batch {
				rows = append(rows, modelRow{env: e})
			}
		}
		// Ids are a BIGSERIAL with one writer, so id order is insertion
		// order. global_received_at is the database's now() at insert, so
		// the model takes it from the table rather than guessing it.
		var stored []struct {
			ID               int64
			Msg              string
			GlobalReceivedAt time.Time
		}
		if err := db.Raw(`SELECT id, msg, global_received_at FROM log_envelopes ORDER BY id`).Scan(&stored).Error; err != nil {
			rt.Fatalf("read stored rows: %v", err)
		}
		if len(stored) != len(rows) {
			rt.Fatalf("table has %d rows, inserted %d", len(stored), len(rows))
		}
		for i := range rows {
			if stored[i].Msg != rows[i].env.Msg {
				rt.Fatalf("id order != insertion order: id %d has %q want %q", stored[i].ID, stored[i].Msg, rows[i].env.Msg)
			}
			rows[i].id = stored[i].ID
			rows[i].received = stored[i].GlobalReceivedAt
		}

		for q := 0; q < 4; q++ {
			f := logstore.QueryFilter{
				MachineID:   rapid.SampledFrom(machines).Draw(rt, "fMachine"),
				OpID:        rapid.SampledFrom(ops).Draw(rt, "fOp"),
				MachineName: rapid.SampledFrom(names).Draw(rt, "fName"),
				MinLevel:    rapid.SampledFrom([]string{"", "debug", "info", "warn", "error", "WARN", "Info", "bogus", "warning"}).Draw(rt, "fLevel"),
			}
			if rapid.Bool().Draw(rt, "fSrc") {
				f.Sources = []string{string(rapid.SampledFrom(sources).Draw(rt, "fSource1"))}
				if rapid.Bool().Draw(rt, "fSrc2") {
					f.Sources = append(f.Sources, string(rapid.SampledFrom(sources).Draw(rt, "fSource2")))
				}
			}
			if rapid.Bool().Draw(rt, "fSince") {
				f.Since = base.Add(time.Duration(rapid.IntRange(-6, 6).Draw(rt, "sinceOff")) * time.Minute)
			}
			want, valid := modelQuery(rows, f)
			if vErr := logstore.ValidateMinLevel(f.MinLevel); (vErr == nil) != valid {
				rt.Fatalf("ValidateMinLevel(%q) = %v, model says valid=%v", f.MinLevel, vErr, valid)
			}

			got, err := st.Query(ctx, f)
			if !valid {
				if err == nil {
					rt.Fatalf("filter %+v: unknown min level accepted, returned %v", f, ids(got))
				}
				continue
			}
			if err != nil {
				rt.Fatalf("query: %v", err)
			}
			if fmt.Sprint(ids(got)) != fmt.Sprint(want) {
				rt.Fatalf("filter %+v\n got  %v\n want %v", f, ids(got), want)
			}

			// Paged replay must reproduce the unpaged result.
			page := rapid.IntRange(1, 7).Draw(rt, "page")
			var paged []int64
			pf := f
			pf.Limit = page
			for {
				res, err := st.Query(ctx, pf)
				if err != nil {
					rt.Fatalf("paged query: %v", err)
				}
				paged = append(paged, ids(res)...)
				if len(res) < page {
					break
				}
				pf.LastSeenID = res[len(res)-1].ID
			}
			if fmt.Sprint(paged) != fmt.Sprint(want) {
				rt.Fatalf("paged (size %d) %v != unpaged %v", page, paged, want)
			}
		}

		// Round trip: every stored payload decodes to the inserted envelope.
		all, err := st.Query(ctx, logstore.QueryFilter{})
		if err != nil {
			rt.Fatal(err)
		}
		if len(all) != len(rows) {
			rt.Fatalf("unfiltered query returned %d rows, inserted %d", len(all), len(rows))
		}
		for i, r := range all {
			w := rows[i].env
			g := r.Envelope
			if !g.TS.Equal(w.TS) || g.Level != w.Level || g.Source != w.Source || g.Msg != w.Msg ||
				g.MachineID != w.MachineID || g.OpID != w.OpID || g.MachineName != w.MachineName {
				rt.Fatalf("round trip id %d:\n got  %+v\n want %+v", r.ID, g, w)
			}
		}
	})
}

// clearEnvelopes empties log_envelopes between cases. testpg.DB
// truncates every table, which costs far more per case than deleting
// the few rows of the one table these properties write.
func clearEnvelopes(t *rapid.T, db *gorm.DB) {
	if err := db.Exec(`DELETE FROM log_envelopes`).Error; err != nil {
		t.Fatalf("clear log_envelopes: %v", err)
	}
}

func ids(rs []logstore.Result) []int64 {
	out := make([]int64, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// TestRetention_Property_DeletesExactlyPolicy: DeleteOlderThan per source
// removes exactly the rows of that source with ts < cutoff and nothing
// else (recomputation oracle over the inserted rows).
func TestRetention_Property_DeletesExactlyPolicy(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := logstore.New(db)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Microsecond)
	sources := []slogkit.Source{"agent", "site", "global", "sol"}
	rapid.Check(t, func(rt *rapid.T) {
		clearEnvelopes(rt, db)
		n := rapid.IntRange(0, 30).Draw(rt, "n")
		batch := make([]slogkit.Envelope, n)
		for i := range batch {
			batch[i] = slogkit.Envelope{
				TS:     base.Add(time.Duration(rapid.IntRange(-100, 0).Draw(rt, "ageDays")) * 24 * time.Hour),
				Level:  "info",
				Source: rapid.SampledFrom(sources).Draw(rt, "src"),
				Msg:    fmt.Sprintf("r%d", i),
			}
		}
		if err := st.InsertBatch(ctx, batch); err != nil {
			rt.Fatal(err)
		}
		src := string(rapid.SampledFrom(append(sources, "")).Draw(rt, "delSrc"))
		cutoff := base.Add(time.Duration(rapid.IntRange(-100, 1).Draw(rt, "cutDays")) * 24 * time.Hour)
		var wantDeleted int64
		keep := map[string]bool{}
		for _, e := range batch {
			if (src == "" || string(e.Source) == src) && e.TS.Before(cutoff) {
				wantDeleted++
			} else {
				keep[e.Msg] = true
			}
		}
		got, err := st.DeleteOlderThan(ctx, cutoff, src)
		if err != nil {
			rt.Fatal(err)
		}
		if got != wantDeleted {
			rt.Fatalf("deleted %d, want %d", got, wantDeleted)
		}
		rest, err := st.Query(ctx, logstore.QueryFilter{})
		if err != nil {
			rt.Fatal(err)
		}
		if len(rest) != len(keep) {
			rt.Fatalf("%d rows remain, want %d", len(rest), len(keep))
		}
		for _, r := range rest {
			if !keep[r.Envelope.Msg] {
				rt.Fatalf("row %q should have been deleted", r.Envelope.Msg)
			}
		}
	})
}
