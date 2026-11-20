// External test package (logstore_test, not logstore): logstore is now
// imported by internal/store (for the D6 op-lifecycle InsertTx seam), so an
// in-package test that imports store would form a cycle. The external package
// imports both freely and exercises logstore through its exported API.
package logstore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/logstore"
	"github.com/bluesky-social/slagworks/internal/slogkit"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

func newTestStore(t *testing.T) *logstore.Store {
	t.Helper()
	db := testpg.DB(t, store.Migrate)
	return logstore.New(db)
}

func TestMain(m *testing.M) {
	code := m.Run()
	testpg.Shutdown()
	os.Exit(code)
}

func TestInsertBatchAndQuery(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	trace := uuid.New()
	machine := uuid.New()
	op := uuid.New()
	now := time.Now().UTC().Round(time.Microsecond)

	envs := []slogkit.Envelope{
		{
			TS: now, Level: slogkit.LevelInfo, Source: slogkit.SourceAgent,
			Msg: "hello", MachineID: machine, OpID: op, TraceID: trace,
		},
		{
			TS: now.Add(time.Millisecond), Level: slogkit.LevelWarn, Source: slogkit.SourceSite,
			Msg: "phase changed", MachineID: machine, OpID: op, TraceID: trace, Phase: "rsyncing_os",
		},
		{
			TS: now.Add(2 * time.Millisecond), Level: slogkit.LevelInfo, Source: slogkit.SourceGlobal,
			Msg: "[ 3.451] systemd[1]: Started rsync", MachineID: machine, TraceID: trace,
		},
		// An unrelated trace — must not appear in trace-filtered queries.
		{
			TS: now, Level: slogkit.LevelInfo, Source: slogkit.SourceAgent,
			Msg: "noise", TraceID: uuid.New(),
		},
	}
	if err := st.InsertBatch(ctx, envs); err != nil {
		t.Fatalf("insert: %v", err)
	}

	t.Run("filter by trace_id", func(t *testing.T) {
		got, err := st.Query(ctx, logstore.QueryFilter{TraceID: trace})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Errorf("trace filter: got %d envelopes, want 3", len(got))
		}
		for _, r := range got {
			if r.Envelope.TraceID != trace {
				t.Errorf("envelope leaked across trace: %v", r.Envelope.TraceID)
			}
		}
	})

	t.Run("filter by source", func(t *testing.T) {
		got, err := st.Query(ctx, logstore.QueryFilter{Sources: []string{"site"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Envelope.Source != slogkit.SourceSite {
			t.Errorf("source filter mismatch: %+v", got)
		}
	})

	t.Run("min level filters out info+debug", func(t *testing.T) {
		got, err := st.Query(ctx, logstore.QueryFilter{TraceID: trace, MinLevel: "warn"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Envelope.Level != slogkit.LevelWarn {
			t.Errorf("min_level=warn returned: %+v", got)
		}
	})

	t.Run("unknown min level is an error, not every level", func(t *testing.T) {
		got, err := st.Query(ctx, logstore.QueryFilter{TraceID: trace, MinLevel: "warning"})
		if err == nil {
			t.Fatalf("min_level=warning: got %d envelopes and no error, want an error", len(got))
		}
	})

	t.Run("last_seen_id resumes past prior id", func(t *testing.T) {
		all, err := st.Query(ctx, logstore.QueryFilter{TraceID: trace})
		if err != nil {
			t.Fatal(err)
		}
		if len(all) < 2 {
			t.Fatalf("expected the 3 seeded trace rows, got %d", len(all))
		}
		got, err := st.Query(ctx, logstore.QueryFilter{TraceID: trace, LastSeenID: all[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range got {
			if r.ID <= all[0].ID {
				t.Errorf("last_seen_id resume returned id %d <= cursor %d", r.ID, all[0].ID)
			}
		}
	})

	t.Run("delete older than", func(t *testing.T) {
		// Insert one extra in the past.
		old := slogkit.Envelope{
			TS: time.Now().Add(-48 * time.Hour), Level: slogkit.LevelInfo,
			Source: slogkit.SourceAgent, Msg: "ancient",
		}
		if err := st.InsertBatch(ctx, []slogkit.Envelope{old}); err != nil {
			t.Fatalf("insert ancient: %v", err)
		}
		n, err := st.DeleteOlderThan(ctx, time.Now().Add(-24*time.Hour), "")
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if n < 1 {
			t.Errorf("expected at least 1 deleted, got %d", n)
		}
	})
}

// TestQuery_MachineOrOpScope covers the OR semantics used by `operation watch`
// / `deploy -w`: with BOTH machine_id and op_id set, the filter is
// `machine_id = M OR op_id = O`. The operator wants ALL recent activity for the
// target machine while a deploy runs — its own op lines, its pre-op infra lines
// (DHCP/TFTP carry machine_id but no op_id), AND a concurrent/conflicting
// op on the same machine. The caller bounds noise with `since` (not exercised
// here). A bare op or bare machine filter reduces to a single equality.
func TestQuery_MachineOrOpScope(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := logstore.New(db)
	ctx := context.Background()

	machine := uuid.New()
	op := uuid.New()
	concurrentOp := uuid.New() // a different op, but on the SAME machine
	otherMachine := uuid.New()
	now := time.Now().UTC().Round(time.Microsecond)

	envs := []slogkit.Envelope{
		{TS: now, Level: slogkit.LevelInfo, Source: slogkit.SourceAgent,
			Msg: "agent exec", MachineID: machine, OpID: op},
		// pre-op infra: this machine, NO op_id (DHCP DISCOVER shape).
		{TS: now.Add(1 * time.Millisecond), Level: slogkit.LevelInfo, Source: slogkit.SourceSite,
			Msg: "DHCP Offer", MachineID: machine},
		// a CONCURRENT op on the same machine — should surface (machine_id match).
		{TS: now.Add(2 * time.Millisecond), Level: slogkit.LevelWarn, Source: slogkit.SourceGlobal,
			Msg: "concurrent op claimed", MachineID: machine, OpID: concurrentOp},
		// unrelated machine, no op — must NOT appear.
		{TS: now.Add(3 * time.Millisecond), Level: slogkit.LevelInfo, Source: slogkit.SourceSite,
			Msg: "DHCP Offer other", MachineID: otherMachine},
	}
	if err := st.InsertBatch(ctx, envs); err != nil {
		t.Fatalf("insert: %v", err)
	}

	t.Run("machine+op OR scope surfaces op, infra, and concurrent-op lines", func(t *testing.T) {
		got, err := st.Query(ctx, logstore.QueryFilter{MachineID: machine, OpID: op})
		if err != nil {
			t.Fatal(err)
		}
		msgs := map[string]bool{}
		for _, r := range got {
			msgs[r.Envelope.Msg] = true
		}
		if !msgs["agent exec"] {
			t.Error("the op's own line missing")
		}
		if !msgs["DHCP Offer"] {
			t.Error("this machine's NULL-op DHCP line should surface")
		}
		if !msgs["concurrent op claimed"] {
			t.Error("a concurrent op on the same machine should surface (the point of OR)")
		}
		if msgs["DHCP Offer other"] {
			t.Error("an unrelated machine's line leaked in")
		}
	})

	t.Run("bare op scope stays strict (only that op's rows)", func(t *testing.T) {
		got, err := st.Query(ctx, logstore.QueryFilter{OpID: op})
		if err != nil {
			t.Fatal(err)
		}
		var msgs []string
		for _, r := range got {
			msgs = append(msgs, r.Envelope.Msg)
		}
		if len(got) != 1 || got[0].Envelope.Msg != "agent exec" {
			t.Errorf("bare op filter should return only the op-tagged line, got %v", msgs)
		}
	})

	t.Run("bare machine scope returns all of that machine's rows", func(t *testing.T) {
		got, err := st.Query(ctx, logstore.QueryFilter{MachineID: machine})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 { // agent exec + DHCP Offer + concurrent op claimed
			t.Errorf("bare machine filter: got %d rows, want 3", len(got))
		}
	})

	t.Run("since still bounds the OR (parenthesization holds)", func(t *testing.T) {
		// An OLD line for this machine, well before `since` by the
		// emitter's clock and by global's. The machine_id = M match must
		// NOT let it bypass the since-bound — that's the
		// unparenthesized-OR footgun (roast HIGH). It must be excluded.
		oldLine := slogkit.Envelope{
			TS: now.Add(-time.Hour), Level: slogkit.LevelInfo, Source: slogkit.SourceSite,
			Msg: "ancient DHCP", MachineID: machine,
		}
		if err := st.InsertBatch(ctx, []slogkit.Envelope{oldLine}); err != nil {
			t.Fatalf("insert old: %v", err)
		}
		if err := db.Exec("UPDATE log_envelopes SET global_received_at = ts WHERE msg = ?", oldLine.Msg).Error; err != nil {
			t.Fatalf("backdate old: %v", err)
		}
		got, err := st.Query(ctx, logstore.QueryFilter{
			MachineID: machine, OpID: op, Since: now.Add(-time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range got {
			if r.Envelope.Msg == "ancient DHCP" {
				t.Error("since-bound bypassed: an old machine line leaked past the time floor (unparenthesized OR)")
			}
		}
	})
}

// TestQuery_SinceAdmitsWhatEitherClockPlacesInTheWindow: Since passes a row
// whose emitter ts or whose global_received_at is at or after it, on every
// query. An emitter whose clock runs behind stamps ts before the window on
// lines global receives inside it; a global whose clock lags receives
// lines late that their emitter stamped inside it. Only a row both clocks
// place before Since is outside the window. The bound must not depend on
// which query of a stream is asking (a paging cursor would step over what
// an earlier, narrower query hid), so QueryFilter has no way to ask for
// one clock alone.
func TestQuery_SinceAdmitsWhatEitherClockPlacesInTheWindow(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := logstore.New(db)
	ctx := context.Background()
	machine := uuid.New()
	now := time.Now().UTC().Round(time.Microsecond)
	since := now.Add(-10 * time.Minute)

	rows := []struct {
		msg          string
		ts, received time.Time
	}{
		{"both clocks in the window", since.Add(time.Minute), since.Add(time.Minute)},
		{"emitter clock behind", since.Add(-time.Hour), since.Add(2 * time.Minute)},
		{"global clock behind", since.Add(3 * time.Minute), since.Add(-time.Hour)},
		{"before the window on both clocks", since.Add(-time.Hour), since.Add(-time.Hour)},
	}
	for _, r := range rows {
		env := slogkit.Envelope{TS: r.ts, Level: slogkit.LevelInfo, Source: slogkit.SourceAgent, Msg: r.msg, MachineID: machine}
		if err := st.InsertBatch(ctx, []slogkit.Envelope{env}); err != nil {
			t.Fatalf("insert %q: %v", r.msg, err)
		}
		if err := db.Exec("UPDATE log_envelopes SET global_received_at = ? WHERE machine_id = ? AND msg = ?", r.received, machine, r.msg).Error; err != nil {
			t.Fatalf("set global_received_at for %q: %v", r.msg, err)
		}
	}

	got, err := st.Query(ctx, logstore.QueryFilter{MachineID: machine, Since: since})
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, r := range got {
		msgs = append(msgs, r.Envelope.Msg)
	}
	want := []string{"both clocks in the window", "emitter clock behind", "global clock behind"}
	if len(msgs) != len(want) {
		t.Fatalf("Query with Since returned %q, want %q in id order", msgs, want)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Fatalf("Query with Since returned %q, want %q in id order", msgs, want)
		}
	}
}

// TestRunRetention_PolicyScopedBySource pins the B6 retention contract:
// each source is swept against its own window, sources absent from the
// policy are never touched, and the first sweep fires immediately.
func TestRunRetention_PolicyScopedBySource(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	traces := map[string]uuid.UUID{
		"agent-old":   uuid.New(), // agent, past 30d — swept
		"agent-fresh": uuid.New(), // agent, recent — kept
		"global-mid":  uuid.New(), // global, inside 90d — kept
		"site-old":    uuid.New(), // site, past 30d, NOT in policy — kept
	}
	insert := func(key string, source slogkit.Source, age time.Duration) {
		t.Helper()
		env := slogkit.Envelope{
			TS: time.Now().Add(-age), Level: slogkit.LevelInfo,
			Source: source, Msg: key, TraceID: traces[key],
		}
		if err := st.InsertBatch(ctx, []slogkit.Envelope{env}); err != nil {
			t.Fatal(err)
		}
	}
	insert("agent-old", slogkit.SourceAgent, 40*24*time.Hour)
	insert("agent-fresh", slogkit.SourceAgent, time.Hour)
	insert("global-mid", slogkit.SourceGlobal, 40*24*time.Hour)
	insert("site-old", slogkit.SourceSite, 40*24*time.Hour)

	policy := logstore.RetentionPolicy{
		"agent":  30 * 24 * time.Hour,
		"global": 90 * 24 * time.Hour,
	}
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		st.RunRetention(rctx, policy, time.Hour, nil)
	}()
	// The first sweep is immediate; poll for its effect then stop.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := st.Query(ctx, logstore.QueryFilter{TraceID: traces["agent-old"]})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent-old not swept within 5s")
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	<-done

	for _, keep := range []string{"agent-fresh", "global-mid", "site-old"} {
		got, err := st.Query(ctx, logstore.QueryFilter{TraceID: traces[keep]})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("%s: got %d rows, want 1 (must not be swept)", keep, len(got))
		}
	}
}
