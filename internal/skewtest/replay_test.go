package skewtest

// The outbox replay after a global restart.
//
// While global is down a site worker keeps serving: every DHCP lease it
// grants or renews queues a dhcp_lease observation, and a deploy whose
// installed OS phones home queues a phone_home. When global comes back the
// worker reconnects and sends the whole outbox in one burst, with the op
// updates it held in memory right behind; every handler needs a database
// connection.
//
// TestReplayAfterGlobalRestart builds that burst against a database login
// that may hold only a few connections (a stand-in for whatever bounds new
// connections in a deployment: max_connections, a per-role limit, or the
// rate at which a managed database accepts TLS logins), with global's pool
// bounded below that limit the way slagworksd global bounds its own
// (TestOpenGlobalDB_BurstQueuesWithinConnectionLimit in cmd/slagworksd pins
// that the daemon does). It checks what the outbox contract promises, that
// nothing queued is lost, and that no handler fails for want of a
// connection, which is what loses the messages a worker sends only once.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestReplayAfterGlobalRestart(t *testing.T) {
	const (
		leases    = 150 // a few minutes of renewals at a site with about a hundred hosts
		roleLimit = 12
	)
	for _, tc := range []struct {
		name    string
		maxOpen int
	}{
		{name: "pool_bounded", maxOpen: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, adminDSN := freshDatabase(t, t, "")
			defer dropDatabase(name)
			role := "r_" + name
			if err := pgAdmin.Exec(fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD 'flood' CONNECTION LIMIT %d", role, roleLimit)).Error; err != nil {
				t.Fatalf("harness: create role: %v", err)
			}
			defer func() { _ = pgAdmin.Exec("DROP ROLE IF EXISTS " + role).Error }()
			if err := pgAdmin.Exec(fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", name, role)).Error; err != nil {
				t.Fatalf("harness: database owner: %v", err)
			}
			sqlDB, err := gorm.Open(postgres.Open(adminDSN), &gorm.Config{Logger: logger.Discard})
			if err != nil {
				t.Fatalf("harness: open case database: %v", err)
			}
			defer func() {
				if s, err := sqlDB.DB(); err == nil {
					_ = s.Close()
				}
			}()
			if err := sqlDB.Exec(fmt.Sprintf("ALTER SCHEMA public OWNER TO %s", role)).Error; err != nil {
				t.Fatalf("harness: schema owner: %v", err)
			}
			dsn := strings.NewReplacer("user=postgres", "user="+role, "password=test", "password=flood").Replace(adminDSN)

			st := &stats{boots: map[string]int{}}
			w := &world{
				t: t, f: t, st: st, slug: "site-a", worker: "worker-1", dsn: dsn, dbName: name, sql: sqlDB,
				binFrom: toolNew, binTo: toolNew, verFrom: "aaaaaaa", verTo: "aaaaaaa",
				gAddr: freePort(t), dbPath: filepath.Join(t.TempDir(), "worker.db"),
			}
			defer func() {
				if w.w != nil {
					w.w.kill()
				}
				if w.g != nil {
					w.g.kill()
				}
			}()

			// A fleet with one deploy that has reached awaiting_phone_home.
			w.startGlobal("from")
			var seeded struct {
				Tokens   map[string]string `json:"tokens"`
				Machines []string          `json:"machines"`
			}
			w.must("g.seed", w.g.call("g.seed", map[string]any{"workers": []string{w.worker}, "machines": []map[string]any{
				{"serial": "FLOOD0001", "mac": "02:00:00:00:00:01", "state": "commissioned"},
				{"serial": "FLOOD0002", "mac": "02:00:00:00:00:02", "state": "commissioned"},
			}}, &seeded))
			w.token = seeded.Tokens[w.worker]
			m := &mach{id: seeded.Machines[0], mac: "02:00:00:00:00:01", serial: "FLOOD0001"}
			m2 := &mach{id: seeded.Machines[1], mac: "02:00:00:00:00:02", serial: "FLOOD0002"}
			w.machines = []*mach{m, m2}
			w.startWorker("from")
			if !eventually(20*time.Second, func() bool { return w.status().Connected }) {
				w.fail("harness: worker never connected")
			}
			w.createOp(m, "deploy")
			o := m.cur()
			if o == nil {
				w.fail("harness: deploy op was not created")
			}
			if !eventually(5*time.Second, func() bool { w.checkActuations(); return o.actuated }) {
				w.fail("harness: the executor never power-cycled the machine")
			}
			if code := w.report(m, o, "partitioning", "", false); code != 200 {
				w.fail("harness: phase report got %d", code)
			}
			if code := w.report(m, o, "", "deploy_complete", false); code != 200 {
				w.fail("harness: deploy_complete got %d", code)
			}
			if !eventually(5*time.Second, func() bool {
				g := w.dump().op(o.id)
				return g != nil && g.Phase == "awaiting_phone_home"
			}) {
				w.fail("harness: awaiting_phone_home never reached global")
			}

			// A second deploy, still installing when global goes down.
			w.createOp(m2, "deploy")
			o2 := m2.cur()
			if o2 == nil {
				w.fail("harness: second deploy op was not created")
			}
			if !eventually(5*time.Second, func() bool { w.checkActuations(); return o2.actuated }) {
				w.fail("harness: the executor never power-cycled the second machine")
			}
			if code := w.report(m2, o2, "partitioning", "", false); code != 200 {
				w.fail("harness: phase report got %d", code)
			}

			// Global goes down. The worker keeps serving.
			w.stopGlobal(false)
			// The second install finishes meanwhile: its phases are op
			// updates, which the worker holds in memory and sends once,
			// unacknowledged, right behind the observations.
			for _, phase := range []string{"rsyncing_os", "installing bootloader", "finalizing"} {
				if code := w.report(m2, o2, phase, "", false); code != 200 {
					w.fail("the worker answered %d to a phase report while global was down", code)
				}
			}
			if code := w.report(m, o, "", "phone_home", false); code != 200 {
				w.fail("the worker answered %d to the phone_home of a deploy awaiting it while global was down", code)
			}
			for i := 0; i < leases; i++ {
				w.must("w.dhcp_lease", w.w.call("w.dhcp_lease", map[string]any{
					"machine_id": "00000000-0000-0000-0000-000000000000", "event": "allocate",
					"mac": fmt.Sprintf("02:00:00:01:%02x:%02x", i/256, i%256),
					"ip":  fmt.Sprintf("198.51.100.%d", i+1),
				}, nil))
			}
			if got := outboxRows(t, w.dbPath); got < leases+1 {
				w.fail("harness: outbox holds %d rows, want at least %d", got, leases+1)
			}

			// Global comes back; the worker replays.
			p := startProc(t, toolNew, "global-flood")
			w.must("g.start", p.call("g.start", map[string]any{
				"dsn": dsn, "addr": w.gAddr, "slug": w.slug, "version": "aaaaaaa", "macs": w.macs(),
				"max_open_conns": tc.maxOpen, "max_idle_conns": tc.maxOpen,
			}, nil))
			w.g, w.gDown = p, false
			start := time.Now()
			if !eventually(120*time.Second, func() bool { return outboxRows(t, w.dbPath) == 0 }) {
				w.fail("the outbox still holds %d unacknowledged observations 120s after global came back", outboxRows(t, w.dbPath))
			}
			drained := time.Since(start)

			// Nothing queued was lost.
			var open int64
			if err := sqlDB.Raw(`SELECT count(*) FROM dhcp_lease_ledger WHERE released_at IS NULL AND mac LIKE '02:00:00:01:%'`).Scan(&open).Error; err != nil {
				t.Fatalf("harness: count ledger: %v", err)
			}
			if open != leases {
				var total int64
				var sample string
				_ = sqlDB.Raw(`SELECT count(*) FROM dhcp_lease_ledger`).Scan(&total).Error
				_ = sqlDB.Raw(`SELECT COALESCE((SELECT to_jsonb(t)::text FROM dhcp_lease_ledger t LIMIT 1), '')`).Scan(&sample).Error
				t.Logf("ledger rows in all: %d; one of them: %s", total, sample)
				w.fail("the lease ledger holds %d of the %d leases the worker reported while global was down", open, leases)
			}
			if !eventually(10*time.Second, func() bool {
				g := w.dump().op(o.id)
				return g != nil && g.Status == "completed"
			}) {
				g := w.dump().op(o.id)
				w.fail("the deploy whose phone-home the worker accepted while global was down is %s/%q at global, want completed", g.Status, g.Phase)
			}

			// What the burst cost.
			time.Sleep(500 * time.Millisecond) // let the op updates behind the observations be handled
			glog := w.g.stderr.String()
			obsFailed := strings.Count(glog, `"msg":"observation handler failed"`)
			updFailed := strings.Count(glog, `"msg":"op_update handler failed"`)
			phase2 := w.dump().op(o2.id).Phase
			connFailed := 0
			for _, line := range strings.Split(glog, "\n") {
				if strings.Contains(line, "handler failed") || strings.Contains(line, "touch failed") || strings.Contains(line, "refusing connection") {
					if strings.Contains(line, "failed to connect to") || strings.Contains(line, "too many connections") {
						connFailed++
					}
				}
			}
			t.Logf("replay of %d observations against a %d-connection login, pool max_open=%d: drained in %s; observation handler failures=%d, op_update handler failures=%d, handler runs that could not get a connection=%d; the installing deploy's phase at global is %q (the agent last reported %q)",
				leases+1, roleLimit, tc.maxOpen, drained.Round(time.Millisecond), obsFailed, updFailed, connFailed, phase2, "finalizing")
			if phase2 != "finalizing" {
				w.fail("the op updates the worker queued while global was down were dropped: the installing deploy is at phase %q at global, the agent's last accepted report said %q", phase2, "finalizing")
			}
			if connFailed > 0 {
				w.fail("%d handler runs failed because global could not open a database connection: the replay burst opened more connections than the database accepts", connFailed)
			}
		})
	}
}
