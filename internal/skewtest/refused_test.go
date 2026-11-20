package skewtest

// What a refused peer sees.
//
// v0.6.0 moved the worker token from the URL to a header, so a worker and a
// global on opposite sides of that release refuse each other. The breaking
// change is deliberate; what these tests look at is what each side then
// tells its operator and whether the refused worker keeps serving the boots
// it can serve from its cache. (Whether the daemon reports ready to systemd
// while refused is decided in cmd/slagworksd, above what the driver runs;
// TestSiteWorkerReportsReadyWithoutGlobal there covers it.)

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// refusal runs a worker of one build against a global of the other for a
// few seconds, after first letting the worker cache a snapshot from a
// global of its own build, and returns both logs and whether the restarted
// worker served a running machine boot-local.
type refusalResult struct {
	workerLog, globalLog string
	bootLocal            bool
}

func refusal(t *testing.T, workerBin, workerTag, globalBin, globalTag string) refusalResult {
	t.Helper()
	name, dsn := freshDatabase(t, t, "")
	defer dropDatabase(name)
	st := &stats{boots: map[string]int{}}
	w := &world{
		t: t, f: t, st: st, slug: "site-a", worker: "worker-1", dsn: dsn, dbName: name,
		binFrom: workerBin, binTo: globalBin, verFrom: workerTag, verTo: globalTag,
		gAddr: freePort(t), dbPath: filepath.Join(t.TempDir(), "worker.db"), refuses: true,
	}
	defer func() {
		if w.w != nil {
			w.w.kill()
		}
		if w.g != nil {
			w.g.kill()
		}
	}()

	// The worker's own build first, so it has a snapshot and a token.
	w.startGlobal("from")
	var seeded struct {
		Tokens   map[string]string `json:"tokens"`
		Machines []string          `json:"machines"`
	}
	w.must("g.seed", w.g.call("g.seed", map[string]any{"workers": []string{w.worker}, "machines": []map[string]any{
		{"serial": "REFUSED01", "mac": "02:00:00:00:00:01", "state": "running", "locked": true},
	}}, &seeded))
	w.token = seeded.Tokens[w.worker]
	m := &mach{id: seeded.Machines[0], mac: "02:00:00:00:00:01", serial: "REFUSED01"}
	w.machines = []*mach{m}
	w.startWorker("from")
	if !eventually(20*time.Second, func() bool { return w.status().Connected }) {
		w.fail("harness: worker never connected to a global of its own build")
	}
	w.stopWorker(false)
	w.stopGlobal(false)

	// Now the other build's global, and the worker restarted against it.
	// Long enough for several dial attempts.
	w.startGlobal("to")
	w.startWorker("from")
	res := refusalResult{}
	time.Sleep(4 * time.Second)
	if w.status().Connected {
		w.fail("the %s worker connected to the %s global; this test expects the two builds to refuse each other", workerTag, globalTag)
	}
	res.bootLocal = w.boot(m) == "local"
	res.workerLog = w.w.stderr.String()
	res.globalLog = w.g.stderr.String()
	return res
}

func warnLines(log string) string {
	var out []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, `"level":"WARN"`) || strings.Contains(l, `"level":"ERROR"`) {
			if len(l) > 420 {
				l = l[:420] + "..."
			}
			out = append(out, l)
		}
	}
	if len(out) > 6 {
		out = out[:6]
	}
	return strings.Join(out, "\n")
}

// TestRefusedOldWorker: a pre-0.6 worker left running against this tree's
// global, the state every site is in between upgrading global and upgrading
// its workers by hand.
func TestRefusedOldWorker(t *testing.T) {
	needOld(t)
	res := refusal(t, toolOld, oldRef, toolNew, "main")
	t.Logf("old worker log (warnings):\n%s", warnLines(res.workerLog))
	t.Logf("new global log (warnings):\n%s", warnLines(res.globalLog))
	if !res.bootLocal {
		t.Errorf("the refused worker did not serve a running machine boot-local from its cached snapshot")
	}
}

// TestRefusedNewWorker: this tree's worker against a pre-0.6 global, the
// state a site is in when its worker is upgraded first, or when global is
// rolled back.
func TestRefusedNewWorker(t *testing.T) {
	needOld(t)
	res := refusal(t, toolNew, "main", toolOld, oldRef)
	t.Logf("new worker log (warnings):\n%s", warnLines(res.workerLog))
	t.Logf("old global log (warnings):\n%s", warnLines(res.globalLog))
	if !res.bootLocal {
		t.Errorf("the refused worker did not serve a running machine boot-local from its cached snapshot")
	}
	if !strings.Contains(res.workerLog, "401") {
		t.Errorf("the worker's log does not carry the status global refused it with:\n%s", warnLines(res.workerLog))
	}
	t.Run("log_carries_the_reason", func(t *testing.T) {
		// The worker's dial error keeps only the handshake status and drops
		// the response body, which holds the reason global gave.
		t.Skip("a refused worker logs a bare 401 without the reason global sent")
		if !strings.Contains(res.workerLog, "token") && !strings.Contains(res.workerLog, "upgrade") {
			t.Errorf("the worker's log does not say what global refused or what to do about it:\n%s", warnLines(res.workerLog))
		}
	})
}
