// Command skewtool is the test driver behind internal/skewtest: one process
// that runs either a global (store, migrations, WebSocket hub and the
// observation and op-update handlers) or a site worker (worker.db, cache,
// upstream connection, executor and the HTTP boot/report surface), and takes
// commands as JSON lines on stdin.
//
// It deliberately uses only the part of the internal API that v0.5.0 and
// later releases share, so the same source compiles against an older
// checkout of the repository. The harness builds it twice, once per release,
// and runs the two builds against the same Postgres database and the same
// worker.db: that is what makes an upgrade, a rollback, or a mixed fleet an
// executable scenario instead of a reading exercise.
//
// It is test machinery. Nothing in a release build imports it.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/bluesky-social/slagworks/internal/commission"
	"github.com/bluesky-social/slagworks/internal/httpboot"
	"github.com/bluesky-social/slagworks/internal/hubhandlers"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/worker"
	"github.com/bluesky-social/slagworks/internal/ws"
)

type request struct {
	Cmd  string          `json:"cmd"`
	Args json.RawMessage `json:"args"`
}

type response struct {
	OK   bool   `json:"ok"`
	Err  string `json:"err,omitempty"`
	Data any    `json:"data,omitempty"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 64<<20)
	out := json.NewEncoder(os.Stdout)
	t := &tool{}
	for in.Scan() {
		var req request
		if err := json.Unmarshal(in.Bytes(), &req); err != nil {
			_ = out.Encode(response{Err: "bad request line: " + err.Error()})
			continue
		}
		data, err := t.dispatch(req)
		if err != nil {
			_ = out.Encode(response{Err: err.Error(), Data: data})
			continue
		}
		_ = out.Encode(response{OK: true, Data: data})
	}
}

type tool struct {
	g *global
	w *site
}

func (t *tool) dispatch(req request) (any, error) {
	switch req.Cmd {
	case "ping":
		return "pong", nil
	case "g.start":
		var a globalStartArgs
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return nil, err
		}
		g, applied, err := startGlobal(a)
		if err != nil {
			return nil, err
		}
		t.g = g
		return map[string]any{"schema_before": applied}, nil
	case "g.seed":
		var a seedArgs
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return nil, err
		}
		return t.g.seed(a)
	case "g.create_op":
		var a createOpArgs
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return nil, err
		}
		return t.g.createOp(a)
	case "g.abort":
		var a struct {
			MachineID uuid.UUID `json:"machine_id"`
		}
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return nil, err
		}
		return t.g.abort(a.MachineID)
	case "g.sweep":
		var a sweepArgs
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return nil, err
		}
		return t.g.sweep(a)
	case "g.rebuild":
		return nil, t.g.rebuildAndPush()
	case "g.fail_claims":
		var a struct {
			On bool `json:"on"`
		}
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return nil, err
		}
		t.g.failClaims.Store(a.On)
		return nil, nil
	case "g.seed_extra":
		return nil, t.g.seedExtra()
	case "g.dump":
		return t.g.dump()
	case "g.stop":
		t.g.stop()
		t.g = nil
		return nil, nil
	case "w.start":
		var a siteStartArgs
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return nil, err
		}
		w, info, err := startSite(a)
		if err != nil {
			return nil, err
		}
		t.w = w
		return info, nil
	case "w.status":
		var a statusArgs
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return nil, err
		}
		return t.w.status(a), nil
	case "w.disconnect":
		t.w.ws.ForceDisconnect()
		return nil, nil
	case "w.dhcp_lease":
		var a struct {
			MachineID uuid.UUID `json:"machine_id"`
			Event     string    `json:"event"`
			MAC       string    `json:"mac"`
			IP        string    `json:"ip"`
		}
		if err := json.Unmarshal(req.Args, &a); err != nil {
			return nil, err
		}
		t.w.cache.RecordDHCPLease(a.MachineID, a.Event, a.MAC, a.IP, "dynamic", "ephemeral", time.Now().Add(time.Hour))
		return nil, nil
	case "w.stop":
		t.w.stop()
		t.w = nil
		return nil, nil
	}
	return nil, fmt.Errorf("unknown command %q", req.Cmd)
}

// ---------------------------------------------------------------- global

type globalStartArgs struct {
	DSN     string            `json:"dsn"`
	Addr    string            `json:"addr"`
	Slug    string            `json:"slug"`
	Version string            `json:"version"`
	MACs    map[string]string `json:"macs"`
	// MaxOpenConns and MaxIdleConns, when non-zero, bound the pool. Zero
	// leaves database/sql's defaults, which is what slagworksd global runs
	// with.
	MaxOpenConns int `json:"max_open_conns"`
	MaxIdleConns int `json:"max_idle_conns"`
}

type global struct {
	slug string
	st   *store.Store
	// failClaims makes every claim_op fail the way it does while global
	// cannot reach its database: the op was created and published, and the
	// worker holds it pending and keeps retrying the claim.
	failClaims atomic.Bool
	pool       interface{ Close() error }
	hub        *ws.Hub
	srv        *http.Server
	log        *slog.Logger

	mu   sync.Mutex
	macs map[string]string

	rebuildMu sync.Mutex
}

// flakyClaimer is the store with a switch on ClaimOperation.
type flakyClaimer struct {
	*store.Store
	fail *atomic.Bool
}

func (f flakyClaimer) ClaimOperation(opID uuid.UUID, workerID string) (*model.Operation, error) {
	if f.fail.Load() {
		return nil, errors.New("skewtool: injected claim failure (database unavailable)")
	}
	return f.Store.ClaimOperation(opID, workerID)
}

type invalidatorFunc func(string)

func (f invalidatorFunc) Invalidate(slug string) { f(slug) }

func startGlobal(a globalStartArgs) (*global, int64, error) {
	cfg, err := pgx.ParseConfig(a.DSN)
	if err != nil {
		return nil, 0, fmt.Errorf("parse dsn: %w", err)
	}
	pool := stdlib.OpenDB(*cfg)
	if a.MaxOpenConns > 0 {
		pool.SetMaxOpenConns(a.MaxOpenConns)
	}
	if a.MaxIdleConns > 0 {
		pool.SetMaxIdleConns(a.MaxIdleConns)
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		TranslateError: true,
	})
	if err != nil {
		_ = pool.Close()
		return nil, 0, fmt.Errorf("open database: %w", err)
	}
	// The goose version before this binary migrates, so the harness can see
	// what Migrate changed (0 on an empty database).
	var before int64
	_ = db.Raw(`SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&before).Error
	if err := store.Migrate(db); err != nil {
		_ = pool.Close()
		return nil, before, fmt.Errorf("migrate: %w", err)
	}
	st := store.New(db)
	log := slog.Default().With("role", "global", "version", a.Version)
	g := &global{slug: a.Slug, st: st, pool: pool, log: log, macs: a.MACs}
	if g.macs == nil {
		g.macs = map[string]string{}
	}

	inv := invalidatorFunc(func(string) {
		go func() {
			if err := g.rebuildAndPush(); err != nil {
				log.Warn("rebuild after invalidate failed", "err", err)
			}
		}()
	})
	comm := commission.New(st, nil, inv)
	comm.SetLogger(log)
	opUpd := hubhandlers.NewOpUpdate(st)
	opUpd.SetLogger(log)
	opUpd.SetInvalidator(func() { inv.Invalidate(a.Slug) })
	hub := ws.NewHub(ws.HubConfig{
		Auth:            st,
		Rebuilder:       g,
		ObsHandler:      comm,
		OpUpdateHandler: opUpd,
		OpClaimer:       flakyClaimer{Store: st, fail: &g.failClaims},
		Invalidator:     inv,
		ActiveOps:       st,
		Logger:          log,
		Version:         a.Version,
	})
	opUpd.SetPublisher(hub)
	comm.SetPublisher(hub)
	g.hub = hub

	// An empty Addr migrates and opens the store without serving workers.
	if a.Addr == "" {
		return g, before, nil
	}
	ln, err := net.Listen("tcp", a.Addr)
	if err != nil {
		_ = pool.Close()
		return nil, before, fmt.Errorf("listen %s: %w", a.Addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/ws/site", hub)
	g.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = g.srv.Serve(ln) }()
	return g, before, nil
}

func (g *global) stop() {
	g.hub.Close()
	if g.srv != nil {
		_ = g.srv.Close()
	}
	_ = g.pool.Close()
}

// seedExtra writes one row into the tables no scenario step touches, so a
// migration has something to lose in each of them.
func (g *global) seedExtra() error {
	if err := g.st.CreateImage(&model.Image{Name: "noble-pxe", Version: "24.04", Arch: "amd64",
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
		URL:    "https://images.example.test/noble-pxe.tar"}); err != nil {
		return fmt.Errorf("create image: %w", err)
	}
	if err := g.st.CreateSSHKey(&model.SSHKey{PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIskewtest operator@example.test", Comment: "skewtest"}); err != nil {
		return fmt.Errorf("create ssh key: %w", err)
	}
	if err := g.st.CreateStorageScript(&model.StorageScript{Name: "skew-storage", Priority: 10, Content: "#!/usr/bin/env python3\nprint('{}')\n"}); err != nil {
		return fmt.Errorf("create storage script: %w", err)
	}
	if err := g.st.CreateNetworkScript(&model.NetworkScript{Name: "skew-network", Priority: 10, Content: "#!/usr/bin/env python3\nprint('{}')\n"}); err != nil {
		return fmt.Errorf("create network script: %w", err)
	}
	if err := g.st.RecordDHCPLeaseEvent(context.Background(), g.slug, "seed-worker", "allocate",
		"02:00:00:00:ff:01", "192.0.2.11", "dynamic", "ephemeral", nil, time.Now().Add(time.Hour)); err != nil {
		return fmt.Errorf("record dhcp lease event: %w", err)
	}
	return nil
}

// RebuildAndWait implements ws.Rebuilder the way the snapshot dispatcher
// does for a hello against a cold hub: build, bump the SeqNum when the
// content changed, return the snapshot.
func (g *global) RebuildAndWait(_ context.Context, slug string) (model.SiteSnapshot, error) {
	snap, _, err := g.build()
	return snap, err
}

func (g *global) build() (model.SiteSnapshot, bool, error) {
	g.rebuildMu.Lock()
	defer g.rebuildMu.Unlock()
	machines, err := g.st.ListMachines(nil)
	if err != nil {
		return model.SiteSnapshot{}, false, fmt.Errorf("list machines: %w", err)
	}
	sort.Slice(machines, func(i, j int) bool { return machines[i].ID.String() < machines[j].ID.String() })
	snap := model.SiteSnapshot{Site: model.SiteInfo{Slug: g.slug}}
	g.mu.Lock()
	for _, m := range machines {
		ms := model.MachineSnapshot{
			ID:             m.ID,
			NetBoxDeviceID: m.NetBoxDeviceID,
			NetBoxSerial:   m.NetBoxSerial,
			Hostname:       "host-" + m.NetBoxSerial,
			State:          m.State,
			Locked:         m.Locked,
			PXEEligible:    m.PXEEligible,
			KernelParams:   "console=ttyS0,115200",
			// The executor refuses a lifecycle op on a machine with no
			// power transport; the fake controller never dials this.
			BMCIP: "192.0.2.200",
			// A machine with no system UUID reads as never commissioned and
			// is refused the deploy boot.
			SystemUUID: m.ID.String(),
		}
		if mac := g.macs[m.ID.String()]; mac != "" {
			ms.Interfaces = []model.SnapshotInterface{{Name: "eth0", MAC: mac, Type: "physical"}}
		}
		snap.Machines = append(snap.Machines, ms)
	}
	g.mu.Unlock()
	hash, err := snap.ContentHash()
	if err != nil {
		return snap, false, fmt.Errorf("content hash: %w", err)
	}
	cfg, err := g.st.GetSiteConfig(g.slug)
	if err != nil {
		return snap, false, fmt.Errorf("site config %s: %w", g.slug, err)
	}
	if cfg.LastSnapshotHash == hash {
		snap.SeqNum = cfg.LastSnapshotSeq
		return snap, false, nil
	}
	seq, err := g.st.BumpSiteSnapshotMeta(g.slug, hash)
	if err != nil {
		return snap, false, fmt.Errorf("bump snapshot meta: %w", err)
	}
	snap.SeqNum = seq
	return snap, true, nil
}

func (g *global) rebuildAndPush() error {
	snap, changed, err := g.build()
	if err != nil {
		return err
	}
	if changed {
		g.hub.Push(g.slug, snap)
	}
	return nil
}

type seedArgs struct {
	Workers  []string `json:"workers"`
	Machines []struct {
		Serial string `json:"serial"`
		MAC    string `json:"mac"`
		State  string `json:"state"`
		Locked bool   `json:"locked"`
	} `json:"machines"`
}

func (g *global) seed(a seedArgs) (any, error) {
	if _, err := g.st.GetSiteConfig(g.slug); err != nil {
		if err := g.st.CreateSiteConfig(&model.SiteConfig{
			Slug: g.slug, NetBoxSite: g.slug,
			EphemeralPoolStart: "192.0.2.10", EphemeralPoolEnd: "192.0.2.50",
		}); err != nil {
			return nil, fmt.Errorf("create site config: %w", err)
		}
	}
	tokens := map[string]string{}
	for _, id := range a.Workers {
		_, tok, err := g.st.RegisterSiteWorker(g.slug, id, id+".example.test")
		if err != nil {
			return nil, fmt.Errorf("register worker %s: %w", id, err)
		}
		tokens[id] = tok
	}
	ids := []string{}
	for i, m := range a.Machines {
		dev := 1000 + i
		row := &model.Machine{NetBoxDeviceID: &dev, NetBoxSerial: m.Serial, State: model.MachineState(m.State), PXEEligible: true, Locked: m.Locked}
		if err := g.st.CreateMachine(row); err != nil {
			return nil, fmt.Errorf("create machine %s: %w", m.Serial, err)
		}
		g.mu.Lock()
		g.macs[row.ID.String()] = m.MAC
		g.mu.Unlock()
		ids = append(ids, row.ID.String())
	}
	return map[string]any{"tokens": tokens, "machines": ids}, g.rebuildAndPush()
}

type createOpArgs struct {
	MachineID uuid.UUID `json:"machine_id"`
	Type      string    `json:"type"`
	Unlock    bool      `json:"unlock"`
}

func (g *global) createOp(a createOpArgs) (any, error) {
	if a.Unlock {
		if err := g.st.SetMachineLocked(a.MachineID, false); err != nil {
			return nil, fmt.Errorf("unlock: %w", err)
		}
	}
	// Params in the shape production deploys carry (a BMC runtime block and
	// the destructive flag), with a recognisable secret so a harness can
	// look for it where it must not be.
	params := json.RawMessage(`{"destructive":true,"bmc_runtime":{"username":"admin","password":"skew-secret","extra_config":{}}}`)
	var op *model.Operation
	var err error
	switch model.OperationType(a.Type) {
	case model.OpDeploy:
		op, err = g.st.CreateMachineOp(a.MachineID, model.OpDeploy, params,
			model.MachineStateCommissioned, model.MachineStateFailed, model.MachineStateRunning)
	case model.OpCommission:
		op, err = g.st.CreateMachineOp(a.MachineID, model.OpCommission, params,
			model.MachineStateNew, model.MachineStateFailed, model.MachineStateCommissioned)
	default:
		op, err = g.st.CreatePowerOp(a.MachineID, model.OperationType(a.Type), params)
	}
	if err != nil {
		return nil, err
	}
	g.hub.PublishOpDelta(*op, "")
	return map[string]any{"op_id": op.ID.String()}, nil
}

func (g *global) publishOutcomes(outs []store.OpOutcome) (ids []string, durable bool) {
	for _, o := range outs {
		g.hub.PublishOpRemoved(o.Op, o.MachineState)
		ids = append(ids, o.Op.ID.String())
		if o.MachineState != "" {
			durable = true
		}
	}
	return ids, durable
}

func (g *global) abort(machineID uuid.UUID) (any, error) {
	_, outs, err := g.st.AbortMachine(machineID)
	ids, durable := g.publishOutcomes(outs)
	if durable {
		if rerr := g.rebuildAndPush(); rerr != nil {
			g.log.Warn("rebuild after abort failed", "err", rerr)
		}
	}
	return map[string]any{"failed": ids}, err
}

// sweepArgs are the four expiry timeouts in milliseconds; a negative value
// skips that sweep. The order is api.Server.expireOperations'.
type sweepArgs struct {
	PendingMS   int64 `json:"pending_ms"`
	StuckMS     int64 `json:"stuck_ms"`
	OrphanMS    int64 `json:"orphan_ms"`
	PhoneHomeMS int64 `json:"phone_home_ms"`
}

func (g *global) sweep(a sweepArgs) (any, error) {
	res := map[string][]string{}
	var errs []error
	durable := false
	run := func(label string, ms int64, f func(time.Duration) ([]store.OpOutcome, error)) {
		if ms < 0 {
			return
		}
		outs, err := f(time.Duration(ms) * time.Millisecond)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", label, err))
		}
		ids, d := g.publishOutcomes(outs)
		res[label] = ids
		durable = durable || d
	}
	run("pending", a.PendingMS, g.st.ExpirePendingOperations)
	run("stuck", a.StuckMS, g.st.ExpireStuckOperations)
	run("orphaned_claim", a.OrphanMS, g.st.ExpireOrphanedClaims)
	run("phone_home", a.PhoneHomeMS, g.st.ExpirePhoneHomeOperations)
	if durable {
		if err := g.rebuildAndPush(); err != nil {
			errs = append(errs, fmt.Errorf("rebuild: %w", err))
		}
	}
	return res, errors.Join(errs...)
}

type opView struct {
	ID        string `json:"id"`
	MachineID string `json:"machine_id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Phase     string `json:"phase"`
	ClaimedBy string `json:"claimed_by"`
	Result    string `json:"result"`
	Params    string `json:"params"`
	Version   int    `json:"version"`
}

type machineView struct {
	ID     string `json:"id"`
	Serial string `json:"serial"`
	State  string `json:"state"`
	Locked bool   `json:"locked"`
}

func (g *global) dump() (any, error) {
	ops, err := g.st.ListOperations(nil)
	if err != nil {
		return nil, fmt.Errorf("list operations: %w", err)
	}
	ov := []opView{}
	for _, op := range ops {
		ov = append(ov, opView{
			ID: op.ID.String(), MachineID: op.MachineID.String(), Type: string(op.Type),
			Status: string(op.Status), Phase: op.Phase, ClaimedBy: op.ClaimedBy,
			Result: string(op.Result), Params: string(op.Params), Version: op.Version,
		})
	}
	sort.Slice(ov, func(i, j int) bool { return ov[i].ID < ov[j].ID })
	machines, err := g.st.ListMachines(nil)
	if err != nil {
		return nil, fmt.Errorf("list machines: %w", err)
	}
	mv := []machineView{}
	for _, m := range machines {
		mv = append(mv, machineView{ID: m.ID.String(), Serial: m.NetBoxSerial, State: string(m.State), Locked: m.Locked})
	}
	sort.Slice(mv, func(i, j int) bool { return mv[i].ID < mv[j].ID })
	out := map[string]any{"ops": ov, "machines": mv}
	if cfg, err := g.st.GetSiteConfig(g.slug); err == nil {
		out["seq"] = cfg.LastSnapshotSeq
	} else {
		out["site_config_err"] = err.Error()
	}
	if id, err := g.st.GetInstanceID(); err == nil {
		out["instance_id"] = id
	} else {
		out["instance_id_err"] = err.Error()
	}
	workers, err := g.st.ListSiteWorkers(g.slug)
	if err != nil {
		return nil, fmt.Errorf("list site workers: %w", err)
	}
	wv := []string{}
	for _, w := range workers {
		wv = append(wv, w.WorkerID)
	}
	sort.Strings(wv)
	out["workers"] = wv
	return out, nil
}

// ---------------------------------------------------------------- site

type siteStartArgs struct {
	DB       string `json:"db"`
	URL      string `json:"url"`
	Token    string `json:"token"`
	WorkerID string `json:"worker_id"`
	Serial   string `json:"serial"`
	HTTPAddr string `json:"http_addr"`
	Version  string `json:"version"`
	// NoExecutor leaves the op executor off, as on a standby worker.
	NoExecutor bool `json:"no_executor"`
}

type site struct {
	cache  *worker.Cache
	ws     *worker.WSConn
	cancel context.CancelFunc
	http   *http.Server
	power  *fakePower
	done   chan struct{}
}

func startSite(a siteStartArgs) (*site, any, error) {
	localDB, err := worker.OpenLocalDB(a.DB)
	if err != nil {
		return nil, nil, fmt.Errorf("open local db: %w", err)
	}
	log := slog.Default().With("role", "site", "version", a.Version)
	localDB.SetLogger(log)
	cache := worker.NewCacheWithSerial(localDB, a.Serial)
	info := map[string]any{
		"rehydrated_seq":      cache.LastSeqNum(),
		"rehydrated_machines": cache.MachineCount(),
		"rehydrated_outbox":   cache.OutboxDepth(),
	}
	conn := worker.NewWSConn(worker.WSConnConfig{
		GlobalURL: a.URL,
		Token:     a.Token,
		WorkerID:  a.WorkerID,
		FQDN:      a.WorkerID + ".example.test",
		Version:   a.Version,
		Serial:    a.Serial,
	}, cache, localDB, nil)
	conn.SetLogger(log)
	ctx, cancel := context.WithCancel(context.Background())
	s := &site{cache: cache, ws: conn, cancel: cancel, power: &fakePower{calls: map[string]map[string]int{}}, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		_ = conn.Run(ctx)
	}()
	if !a.NoExecutor {
		ex := worker.NewExecutor(worker.ExecutorConfig{
			Cache:    cache,
			Factory:  s.power.factory,
			Claimer:  conn,
			WorkerID: a.WorkerID,
			Interval: 40 * time.Millisecond,
		})
		ex.SetLogger(log)
		go func() { _ = ex.Run(ctx) }()
	}

	ln, err := net.Listen("tcp", a.HTTPAddr)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("listen %s: %w", a.HTTPAddr, err)
	}
	hs := httpboot.NewServer(a.HTTPAddr, httpboot.ServerConfig{
		DefaultImage: "noble-pxe",
		HTTPPort:     ln.Addr().(*net.TCPAddr).Port,
		DeployMode:   true,
		Provider:     cache,
	})
	hs.SetLogger(log)
	s.http = &http.Server{Handler: hs.Build(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.http.Serve(ln) }()
	return s, info, nil
}

func (s *site) stop() {
	_ = s.http.Close()
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
}

type statusArgs struct {
	Ops      []string `json:"ops"`
	Machines []string `json:"machines"`
}

func (s *site) status(a statusArgs) any {
	ops := map[string]any{}
	for _, id := range a.Ops {
		if op := s.cache.GetOperation(id); op != nil {
			ops[id] = map[string]any{"status": string(op.Status), "phase": op.Phase, "version": op.Version}
		}
	}
	states := map[string]string{}
	for _, id := range a.Machines {
		if u, err := uuid.Parse(id); err == nil {
			states[id] = s.cache.MachineState(u)
		}
	}
	return map[string]any{
		"connected":  s.ws.Connected(),
		"outbox":     s.cache.OutboxDepth(),
		"seq":        s.cache.LastSeqNum(),
		"ops":        ops,
		"states":     states,
		"actuations": s.power.snapshot(),
	}
}

// fakePower is a BMC that accepts every call and counts the writes per
// machine, so the harness can tell when the executor actuated a host.
type fakePower struct {
	mu    sync.Mutex
	calls map[string]map[string]int
}

func (f *fakePower) factory(_ context.Context, m *model.MachineSnapshot, _ model.BMCRuntime, _ *model.PDURuntime, _ *model.AMTRuntime) (power.Controller, error) {
	return &fakeController{f: f, machine: m.ID.String()}, nil
}

func (f *fakePower) record(machine, call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls[machine] == nil {
		f.calls[machine] = map[string]int{}
	}
	f.calls[machine][call]++
}

func (f *fakePower) snapshot() map[string]map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]map[string]int{}
	for m, c := range f.calls {
		out[m] = map[string]int{}
		for k, v := range c {
			out[m][k] = v
		}
	}
	return out
}

type fakeController struct {
	f       *fakePower
	machine string
}

func (c *fakeController) PowerOn(context.Context) error {
	c.f.record(c.machine, "power_on")
	return nil
}
func (c *fakeController) PowerOff(context.Context) error {
	c.f.record(c.machine, "power_off")
	return nil
}
func (c *fakeController) PowerCycle(context.Context) error {
	c.f.record(c.machine, "power_cycle")
	return nil
}
func (c *fakeController) PowerStatus(context.Context) (power.State, error) { return power.StateOn, nil }
func (c *fakeController) Transport() string                                { return "fake" }
func (c *fakeController) SetBootDevice(_ context.Context, d power.BootDevice) error {
	c.f.record(c.machine, "set_boot_"+string(d))
	return nil
}
func (c *fakeController) ChassisSerial(context.Context) (string, error) { return "", nil }
func (c *fakeController) Close(context.Context) error                   { return nil }
