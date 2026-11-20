package worker

// Property tests for LocalDB persistence: round trips across a process
// restart (close the SQLite handle, reopen the file) for the op layer
// (ops, machine-state overlay and phone-home pins, including
// credential-bearing params and zero/empty values), the machine cache, the
// kv cursors, and the DHCP lease table against an independent map model
// with an explicit clock predicate. Plus corrupt/truncated-file handling:
// OpenLocalDB and every read path must not panic.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func reopenLocalDB(t interface {
	Fatalf(string, ...any)
}, db *LocalDB) *LocalDB {
	sqlDB, err := db.db.DB()
	if err != nil {
		t.Fatalf("extract sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ndb, err := OpenLocalDB(db.Path())
	if err != nil {
		t.Fatalf("reopen %s: %v", db.Path(), err)
	}
	ndb.SetLogger(quietLog)
	return ndb
}

func closeQuiet(db *LocalDB) {
	if sqlDB, err := db.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// openPropDB opens one LocalDB for a whole property. Opening (and
// migrating) a fresh SQLite file per generated case costs far more than the
// case, so the properties whose every case fully overwrites the state it
// reads share one file. The returned pointer follows the property's own
// restarts: reopenLocalDB closes the handle it is given.
func openPropDB(t *testing.T) *LocalDB {
	t.Helper()
	db, err := OpenLocalDB(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetLogger(quietLog)
	return db
}

type caseDirs struct {
	root string
	n    atomic.Int64
}

func (c *caseDirs) next(t *rapid.T) string {
	d := filepath.Join(c.root, fmt.Sprintf("c%d", c.n.Add(1)))
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return d
}

// --- generators ---

func genTimePtr(t *rapid.T, label string) *time.Time {
	if rapid.Bool().Draw(t, label+"Nil") {
		return nil
	}
	v := genTime(t, label)
	return &v
}

func genTime(t *rapid.T, label string) time.Time {
	switch rapid.IntRange(0, 3).Draw(t, label+"Kind") {
	case 0:
		return time.Time{}
	case 1:
		return time.Unix(rapid.Int64Range(0, 4102444800).Draw(t, label+"Sec"), rapid.Int64Range(0, 999999999).Draw(t, label+"Ns")).UTC()
	case 2:
		z := time.FixedZone("z", rapid.IntRange(-12, 14).Draw(t, label+"Off")*3600)
		return time.Unix(rapid.Int64Range(0, 4102444800).Draw(t, label+"Sec"), 0).In(z)
	default:
		return time.Date(2026, 11, 18, 1, 2, 3, 4, time.UTC)
	}
}

func genSecret(t *rapid.T, label string) string {
	return rapid.StringMatching(`[ -~\x{00e9}\x{2603}"\\<>&]{0,20}`).Draw(t, label)
}

func genParams(t *rapid.T) json.RawMessage {
	switch rapid.IntRange(0, 4).Draw(t, "paramsKind") {
	case 0:
		return nil
	case 1:
		return json.RawMessage(`{}`)
	case 2:
		return json.RawMessage(`null`)
	default:
		p := map[string]any{
			"destructive": rapid.Bool().Draw(t, "destructive"),
			"bmc_runtime": map[string]any{
				"host":     "10.0.0.1",
				"username": genSecret(t, "bmcUser"),
				"password": genSecret(t, "bmcPass"),
			},
			"tailscale_runtime": map[string]any{"auth_key": genSecret(t, "tsKey")},
		}
		if rapid.Bool().Draw(t, "withPDU") {
			p["pdu_runtime"] = map[string]any{"outlets": []any{map[string]any{"outlet": 3, "password": genSecret(t, "pduPass")}}}
		}
		b, _ := json.Marshal(p)
		return b
	}
}

func genOperation(t *rapid.T, machines []uuid.UUID) model.Operation {
	op := model.Operation{
		ID:             uuid.New(),
		MachineID:      rapid.SampledFrom(machines).Draw(t, "opMachine"),
		Type:           rapid.SampledFrom([]model.OperationType{model.OpDeploy, model.OpCommission, model.OpPowerOn, model.OpBMCReconcile}).Draw(t, "opType"),
		Status:         rapid.SampledFrom([]model.OperationStatus{model.OpPending, model.OpClaimed, model.OpRunning}).Draw(t, "opStatus"),
		Phase:          rapid.SampledFrom([]string{"", "pxe_booting", "awaiting_phone_home", "partitioning"}).Draw(t, "opPhase"),
		PhaseEnteredAt: genTime(t, "phaseAt"),
		ClaimedBy:      rapid.SampledFrom([]string{"", "worker-a"}).Draw(t, "claimedBy"),
		ClaimedAt:      genTimePtr(t, "claimedAt"),
		CreatedAt:      genTime(t, "createdAt"),
		CompletedAt:    genTimePtr(t, "completedAt"),
		Params:         genParams(t),
		Version:        rapid.IntRange(0, math.MaxInt32).Draw(t, "version"),
	}
	if rapid.Bool().Draw(t, "zeroTrace") {
		op.TraceID = uuid.Nil
	} else {
		op.TraceID = uuid.New()
	}
	if rapid.Bool().Draw(t, "withResult") {
		op.Result = model.OpResult{Error: genSecret(t, "resErr"), Phase: "x"}.Marshal()
	}
	return op
}

func opsEquivalent(a, b model.Operation) string {
	switch {
	case a.ID != b.ID, a.MachineID != b.MachineID, a.TraceID != b.TraceID:
		return "ids"
	case a.Type != b.Type, a.Status != b.Status, a.Phase != b.Phase, a.ClaimedBy != b.ClaimedBy, a.Version != b.Version:
		return "scalar fields"
	case !a.PhaseEnteredAt.Equal(b.PhaseEnteredAt), !a.CreatedAt.Equal(b.CreatedAt):
		return "times"
	case (a.ClaimedAt == nil) != (b.ClaimedAt == nil), a.ClaimedAt != nil && !a.ClaimedAt.Equal(*b.ClaimedAt):
		return "claimed_at"
	case (a.CompletedAt == nil) != (b.CompletedAt == nil), a.CompletedAt != nil && !a.CompletedAt.Equal(*b.CompletedAt):
		return "completed_at"
	case !jsonEquivalent(a.Params, b.Params):
		return fmt.Sprintf("params %q vs %q", a.Params, b.Params)
	case !jsonEquivalent(a.Result, b.Result):
		return "result"
	}
	return ""
}

// TestLocalDBOpState_RoundTrip_Prop: SaveOpState -> restart -> LoadOpState
// returns the same op set, overlay and phone-home pins, and NewCache
// rehydrates every op (untrusted) with params intact.
func TestLocalDBOpState_RoundTrip_Prop(t *testing.T) {
	db := openPropDB(t)
	defer func() { closeQuiet(db) }()
	rapid.Check(t, func(rt *rapid.T) {
		machines := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
		n := rapid.IntRange(0, 6).Draw(rt, "nOps")
		var ops []model.Operation
		for i := 0; i < n; i++ {
			ops = append(ops, genOperation(rt, machines))
		}
		overlay := map[string]model.MachineState{}
		for _, m := range machines {
			if rapid.Bool().Draw(rt, "hasOverlay") {
				overlay[m.String()] = rapid.SampledFrom([]model.MachineState{model.MachineStateDeploying, model.MachineStateRunning, model.MachineStateCommissioning, ""}).Draw(rt, "overlayState")
			}
		}
		// The phone-home pins: IDs of some of the saved deploy ops.
		var phoneHomed []string
		for _, op := range ops {
			if op.Type == model.OpDeploy && rapid.Bool().Draw(rt, "phoneHomed") {
				phoneHomed = append(phoneHomed, op.ID.String())
			}
		}
		// Overwrite history: an earlier save must be fully replaced.
		if rapid.Bool().Draw(rt, "priorSave") {
			prior := genOperation(rt, machines)
			db.SaveOpState(PersistedOpState{
				Ops:        []model.Operation{prior},
				Overlay:    map[string]model.MachineState{machines[0].String(): model.MachineStateFailed},
				PhoneHomed: []string{prior.ID.String()},
			})
		}
		db.SaveOpState(PersistedOpState{Ops: ops, Overlay: overlay, PhoneHomed: phoneHomed})
		db = reopenLocalDB(rt, db)

		st := db.LoadOpState()
		if st == nil {
			rt.Fatalf("LoadOpState returned nil after SaveOpState")
		}
		if len(st.Ops) != len(ops) {
			rt.Fatalf("ops: got %d want %d", len(st.Ops), len(ops))
		}
		for i := range ops {
			if d := opsEquivalent(st.Ops[i], ops[i]); d != "" {
				rt.Fatalf("op %d differs (%s): got %+v want %+v", i, d, st.Ops[i], ops[i])
			}
		}
		if len(st.Overlay) != len(overlay) {
			rt.Fatalf("overlay: got %v want %v", st.Overlay, overlay)
		}
		for k, v := range overlay {
			if st.Overlay[k] != v {
				rt.Fatalf("overlay[%s]: got %q want %q", k, st.Overlay[k], v)
			}
		}

		gotPH := append([]string(nil), st.PhoneHomed...)
		wantPH := append([]string(nil), phoneHomed...)
		sort.Strings(gotPH)
		sort.Strings(wantPH)
		if fmt.Sprint(gotPH) != fmt.Sprint(wantPH) {
			rt.Fatalf("phone-homed pins: got %v want %v", gotPH, wantPH)
		}

		c := NewCache(db)
		for _, op := range ops {
			got := c.GetOperation(op.ID.String())
			if got == nil {
				rt.Fatalf("NewCache did not rehydrate op %s", op.ID)
			}
			if d := opsEquivalent(*got, op); d != "" {
				rt.Fatalf("rehydrated op %s differs (%s)", op.ID, d)
			}
		}
	})
}

// TestLocalDBKV_RoundTrip_Prop: cursor/identity/instance survive restart,
// including int64 extremes and empty strings.
func TestLocalDBKV_RoundTrip_Prop(t *testing.T) {
	db := openPropDB(t)
	defer func() { closeQuiet(db) }()
	rapid.Check(t, func(rt *rapid.T) {
		cursor := rapid.OneOf(rapid.Just(int64(math.MinInt64)), rapid.Just(int64(math.MaxInt64)), rapid.Int64()).Draw(rt, "cursor")
		id := rapid.StringMatching(`[a-z0-9-]{1,36}`).Draw(rt, "id")
		fqdn := rapid.StringMatching(`[a-z0-9.\-]{0,40}`).Draw(rt, "fqdn")
		inst := rapid.String().Draw(rt, "inst")
		db.SetSyncCursor(cursor)
		db.SetWorkerIdentity(id, fqdn)
		db.SetInstanceID(inst)
		db = reopenLocalDB(rt, db)
		if got := db.GetSyncCursor(); got != cursor {
			rt.Fatalf("cursor: got %d want %d", got, cursor)
		}
		if gid, gf := db.GetWorkerIdentity(); gid != id || gf != fqdn {
			rt.Fatalf("identity: got (%q,%q) want (%q,%q)", gid, gf, id, fqdn)
		}
		if got := db.GetInstanceID(); got != inst {
			rt.Fatalf("instance: got %q want %q", got, inst)
		}
		db.ResetSyncState()
		if db.GetSyncCursor() != 0 || db.GetInstanceID() != "" {
			rt.Fatalf("ResetSyncState left state behind")
		}
		if gid, _ := db.GetWorkerIdentity(); gid != id {
			rt.Fatalf("ResetSyncState cleared the worker identity")
		}
	})
}

// TestLocalDBMachineCache_RoundTrip_Prop: SaveSnapshot -> restart ->
// LoadMachineCache returns every machine with credentials and zero values
// intact, and a later snapshot fully replaces an earlier one.
func TestLocalDBMachineCache_RoundTrip_Prop(t *testing.T) {
	db := openPropDB(t)
	defer func() { closeQuiet(db) }()
	rapid.Check(t, func(rt *rapid.T) {
		gen := func(label string) *model.SiteSnapshot {
			snap := &model.SiteSnapshot{SeqNum: rapid.Int64().Draw(rt, label+"seq")}
			n := rapid.IntRange(0, 4).Draw(rt, label+"n")
			for i := 0; i < n; i++ {
				m := model.MachineSnapshot{
					ID:          uuid.New(),
					Hostname:    rapid.StringMatching(`[a-z0-9.-]{0,12}`).Draw(rt, label+"host"),
					State:       rapid.SampledFrom([]model.MachineState{"", model.MachineStateNew, model.MachineStateRunning}).Draw(rt, label+"state"),
					BMCUser:     genSecret(rt, label+"bmcUser"),
					BMCPass:     genSecret(rt, label+"bmcPass"),
					Locked:      rapid.Bool().Draw(rt, label+"locked"),
					PXEEligible: rapid.Bool().Draw(rt, label+"pxe"),
				}
				if rapid.Bool().Draw(rt, label+"nbid") {
					v := rapid.IntRange(0, 1<<30).Draw(rt, label+"nbidv")
					m.NetBoxDeviceID = &v
				}
				snap.Machines = append(snap.Machines, m)
			}
			return snap
		}
		if rapid.Bool().Draw(rt, "prior") {
			db.SaveSnapshot(gen("p"))
		}
		snap := gen("s")
		db.SaveSnapshot(snap)
		db = reopenLocalDB(rt, db)
		got := db.LoadMachineCache()
		if len(got) != len(snap.Machines) {
			rt.Fatalf("machine cache: got %d want %d", len(got), len(snap.Machines))
		}
		for _, m := range snap.Machines {
			g, ok := got[m.ID]
			if !ok {
				rt.Fatalf("machine %s missing", m.ID)
			}
			wb, _ := json.Marshal(m)
			gb, _ := json.Marshal(g)
			if !bytes.Equal(wb, gb) {
				rt.Fatalf("machine %s differs:\n got %s\nwant %s", m.ID, gb, wb)
			}
		}
		meta := db.LoadSnapshotMeta()
		if meta == nil || meta.SeqNum != snap.SeqNum || len(meta.Machines) != 0 {
			rt.Fatalf("snapshot meta: got %+v want seq %d with no machines", meta, snap.SeqNum)
		}
	})
}

// --- DHCP leases vs a map model ---

type leaseModel struct {
	ip     string
	expiry time.Time
}

// leaseZone picks the zone an expiry is written in: the process's own (the
// production shape, time.Now().Add) or another UTC offset for the same
// instant, as a DST change does to one zone.
func leaseZone(t *rapid.T) *time.Location {
	return rapid.SampledFrom([]*time.Location{
		time.Local, time.UTC,
		time.FixedZone("m10", -10*3600), time.FixedZone("p530", 5*3600+1800), time.FixedZone("p14", 14*3600),
	}).Draw(t, "zone")
}

// TestLocalDBLeases_Model_Prop: the lease table against a map through save,
// delete, purge, load and restart.
func TestLocalDBLeases_Model_Prop(t *testing.T) {
	db := openPropDB(t)
	defer func() { closeQuiet(db) }()
	macs := []string{"aa:bb:cc:00:00:01", "aa:bb:cc:00:00:02", "aa:bb:cc:00:00:03", "02:00:00:00:00:0a"}
	rapid.Check(t, func(rt *rapid.T) {
		db.PurgeDHCPLeases()
		store := db.NewLeaseStore()
		mdl := map[string]leaseModel{}
		var trace []string
		check := func(rt *rapid.T) {
			now := time.Now()
			want := map[string]leaseModel{}
			for mac, l := range mdl {
				// Clock predicate. Generated expiries are at least 10
				// minutes from now, so the verdict cannot flip while the
				// case runs.
				if l.expiry.After(now) {
					want[mac] = l
				}
			}
			got := db.LoadDHCPLeases()
			gotM := map[string]DHCPLease{}
			for _, l := range got {
				gotM[l.MAC.String()] = l
			}
			for mac, w := range want {
				g, ok := gotM[mac]
				if !ok {
					rt.Fatalf("live lease %s (expires %s, %v from now) missing from LoadDHCPLeases\ntrace: %v", mac, w.expiry, w.expiry.Sub(now).Round(time.Minute), trace)
				}
				if !g.IP.Equal(net.ParseIP(w.ip)) || !g.ExpiresAt.Equal(w.expiry) {
					rt.Fatalf("lease %s: got %s@%s want %s@%s", mac, g.IP, g.ExpiresAt, w.ip, w.expiry)
				}
			}
			for mac, g := range gotM {
				if _, ok := want[mac]; !ok {
					rt.Fatalf("LoadDHCPLeases returned lease %s (expires %s) the model says is expired or deleted\ntrace: %v", mac, g.ExpiresAt, trace)
				}
			}
			if n := db.CountActiveDHCPLeases(); n != int64(len(want)) {
				rt.Fatalf("CountActiveDHCPLeases = %d, want %d\ntrace: %v", n, len(want), trace)
			}
			// LoadDHCPLeases prunes expired rows: the model drops them too.
			for mac, l := range mdl {
				if !l.expiry.After(now) {
					delete(mdl, mac)
				}
			}
		}
		actions := map[string]func(*rapid.T){
			"save": func(rt *rapid.T) {
				mac := rapid.SampledFrom(macs).Draw(rt, "mac")
				ip := rapid.SampledFrom([]string{"10.1.0.5", "10.1.0.6", "fd00::7", "192.168.255.254"}).Draw(rt, "ip")
				mins := rapid.IntRange(10, 600).Draw(rt, "mins")
				if rapid.Bool().Draw(rt, "expired") {
					mins = -mins
				}
				exp := time.Now().Add(time.Duration(mins) * time.Minute).In(leaseZone(rt))
				hw, _ := net.ParseMAC(mac)
				store.SaveLease(hw, net.ParseIP(ip), exp)
				mdl[mac] = leaseModel{ip: ip, expiry: exp}
				trace = append(trace, fmt.Sprintf("save %s %s %+dm zone=%s", mac, ip, mins, exp.Location()))
			},
			"delete": func(rt *rapid.T) {
				mac := rapid.SampledFrom(macs).Draw(rt, "mac")
				hw, _ := net.ParseMAC(mac)
				store.DeleteLease(hw)
				delete(mdl, mac)
				trace = append(trace, "delete "+mac)
			},
			"purge": func(rt *rapid.T) {
				db.PurgeDHCPLeases()
				mdl = map[string]leaseModel{}
				trace = append(trace, "purge")
			},
			"restart": func(rt *rapid.T) {
				db = reopenLocalDB(rt, db)
				store = db.NewLeaseStore()
				trace = append(trace, "restart")
			},
			"load": check,
		}
		// Repeat picks uniformly: the extra keys keep saves and loads
		// frequent next to purges and restarts (a restart reopens SQLite).
		actions["save2"], actions["save3"], actions["load2"] = actions["save"], actions["save"], check
		rt.Repeat(actions)
		check(rt)
	})
}

// --- corrupt / truncated files ---

type corruptStats struct {
	cases, openErr, openOK, silentLoss, panics atomic.Int64
}

// TestLocalDBCorruption_Prop writes a populated DB, closes it, damages the
// file (truncate, overwrite a byte range, zero the header, or replace it
// with garbage), and reopens. Property: neither OpenLocalDB nor any read
// path panics.
//
// Not asserted, only counted and logged: when the reopen succeeds, fewer
// outbox rows can come back than were persisted. SQLite accepts some
// damaged files as valid smaller databases, and ListPendingOutbox ignores
// the error of its table read, so a damaged outbox table reads as empty
// with nothing logged.
func TestLocalDBCorruption_Prop(t *testing.T) {
	dirs := &caseDirs{root: t.TempDir()}
	st := &corruptStats{}
	var silent []string
	rapid.Check(t, func(rt *rapid.T) {
		st.cases.Add(1)
		path := filepath.Join(dirs.next(rt), "worker.db")
		db, err := OpenLocalDB(path)
		if err != nil {
			rt.Fatalf("open: %v", err)
		}
		db.SetLogger(quietLog)
		nRows := rapid.IntRange(1, 40).Draw(rt, "nRows")
		for i := 0; i < nRows; i++ {
			pad := bytes.Repeat([]byte{'p'}, rapid.IntRange(0, 2000).Draw(rt, "pad"))
			data, _ := json.Marshal(map[string]string{"pad": string(pad)})
			if _, err := db.AppendOutbox(model.Observation{MachineID: uuid.New(), Event: "commission_report", Data: data}); err != nil {
				rt.Fatalf("append: %v", err)
			}
		}
		db.SaveOpState(PersistedOpState{Ops: []model.Operation{{ID: uuid.New(), Type: model.OpDeploy, Status: model.OpClaimed}}})
		closeQuiet(db)
		// A clean close checkpoints the WAL into the main file. Remove any
		// leftover so the damaged main file is all the reopen sees.
		_ = os.Remove(path + "-wal")
		_ = os.Remove(path + "-shm")

		raw, err := os.ReadFile(path)
		if err != nil {
			rt.Fatalf("read: %v", err)
		}
		mode := rapid.SampledFrom([]string{"truncate", "overwrite", "zeroHeader", "garbage"}).Draw(rt, "mode")
		switch mode {
		case "truncate":
			raw = raw[:rapid.IntRange(0, len(raw)-1).Draw(rt, "truncAt")]
		case "overwrite":
			off := rapid.IntRange(100, len(raw)-1).Draw(rt, "off")
			n := rapid.IntRange(1, 512).Draw(rt, "n")
			for i := off; i < off+n && i < len(raw); i++ {
				raw[i] = byte(rapid.IntRange(0, 255).Draw(rt, "b"))
			}
		case "zeroHeader":
			for i := 0; i < 100 && i < len(raw); i++ {
				raw[i] = 0
			}
		case "garbage":
			raw = []byte(rapid.String().Draw(rt, "garbage"))
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			rt.Fatalf("write: %v", err)
		}

		var reopened *LocalDB
		func() {
			defer func() {
				if p := recover(); p != nil {
					st.panics.Add(1)
					rt.Fatalf("OpenLocalDB panicked on %s-damaged file: %v", mode, p)
				}
			}()
			reopened, err = OpenLocalDB(path)
		}()
		if err != nil {
			st.openErr.Add(1)
			return
		}
		st.openOK.Add(1)
		reopened.SetLogger(quietLog)
		defer closeQuiet(reopened)
		var pending []PendingOutbox
		func() {
			defer func() {
				if p := recover(); p != nil {
					st.panics.Add(1)
					rt.Fatalf("read path panicked on %s-damaged file: %v", mode, p)
				}
			}()
			pending = reopened.ListPendingOutbox()
			_ = reopened.LoadOpState()
			_ = reopened.LoadMachineCache()
			_ = reopened.LoadSnapshotMeta()
			_ = reopened.LoadDHCPLeases()
			_ = reopened.GetSyncCursor()
			_, _ = reopened.GetWorkerIdentity()
			_ = NewCache(reopened)
		}()
		if len(pending) < nRows {
			// Classify: a zero-length file is a valid empty DB;
			// otherwise ask SQLite directly whether the table is readable.
			var n int64
			qerr := reopened.db.Raw("SELECT count(*) FROM outbox").Scan(&n).Error
			var ic string
			_ = reopened.db.Raw("PRAGMA integrity_check").Scan(&ic).Error
			kind := "decoded-fewer"
			switch {
			case len(raw) == 0:
				kind = "empty-file"
			case qerr != nil:
				kind = "query-error-swallowed"
			}
			if kind != "empty-file" {
				st.silentLoss.Add(1)
			}
			silent = append(silent, fmt.Sprintf("%s/%s: %d of %d rows returned, count(*)=%d err=%v integrity=%.60q",
				mode, kind, len(pending), nRows, n, qerr, ic))
		}
	})
	t.Logf("corruption coverage: cases=%d openErr=%d openOK=%d silentLoss=%d panics=%d",
		st.cases.Load(), st.openErr.Load(), st.openOK.Load(), st.silentLoss.Load(), st.panics.Load())
	sort.Strings(silent)
	if len(silent) > 0 {
		for _, l := range silent {
			t.Logf("short outbox read: %s", l)
		}
	}
}
