package worker

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// partitionGlobal is a minimal global for one site whose worker it can
// start refusing, which is how a worker experiences a lost uplink, a
// rotated token, or a global on the other side of a breaking release.
type partitionGlobal struct {
	snap   model.SiteSnapshot
	refuse *atomic.Bool
}

func (g partitionGlobal) GetSiteWorkerByTokenHash(hash string) (*model.SiteWorker, error) {
	if g.refuse.Load() || hash != store.HashWorkerToken("tok-b") {
		return nil, store.ErrSiteWorkerNotFound
	}
	return &model.SiteWorker{SiteSlug: "pop1", WorkerID: "worker-b"}, nil
}

func (g partitionGlobal) TouchSiteWorker(slug, workerID, _ string, _ *string) (*model.SiteWorker, error) {
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID}, nil
}

func (g partitionGlobal) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

func (g partitionGlobal) RebuildAndWait(context.Context, string) (model.SiteSnapshot, error) {
	return g.snap, nil
}

// TestCache_WorkerCutOffFromGlobalStopsServingTheInstallerForAnOpItOnlyHeardAbout
//
// A site worker serves boots from its cache when global is unreachable, and
// its cache includes every operation of the site, also the ones a partner
// worker claimed and is driving. Nothing tells a cut-off worker that such
// an operation ended: global's op_removed (the deploy completed through the
// partner, the operator aborted it, a timeout failed it) goes to connected
// workers only. A restarted worker treats the operations it reloads as
// untrusted for exactly this reason ("global may have failed/completed an
// op during the outage") and serves BootLocal until it has resynced. A
// worker that stays up must come to the same conclusion once it has been
// without global for a while and has seen nothing of the operation itself:
// otherwise the next PXE boot of that machine that reaches it runs the
// installer, with the op's --destructive flag, over a host global has as
// running and locked.
//
// SLAGWORKS_OP_TRUST_WINDOW is the knob this test expects the fix to read:
// how long after losing global a worker still authorizes an installer boot
// for an operation it has no local activity for.
func TestCache_WorkerCutOffFromGlobalStopsServingTheInstallerForAnOpItOnlyHeardAbout(t *testing.T) {
	t.Setenv("SLAGWORKS_OP_TRUST_WINDOW", "200ms")

	const mac = "02:00:00:00:00:01"
	machineID := uuid.New()
	snap := model.SiteSnapshot{SeqNum: 9, Site: model.SiteInfo{Slug: "pop1"}, Machines: []model.MachineSnapshot{{
		ID: machineID, Hostname: "host-1", State: model.MachineStateCommissioned, PXEEligible: true,
		SystemUUID: machineID.String(), KernelParams: "console=ttyS0,115200",
		Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: mac, Type: "physical"}},
	}}}
	refuse := &atomic.Bool{}
	g := partitionGlobal{snap: snap, refuse: refuse}
	hub := ws.NewHub(ws.HubConfig{Auth: g, Rebuilder: g})
	srv := httptest.NewServer(hub)
	defer srv.Close()
	defer hub.Close()

	localDB, err := OpenLocalDB(filepath.Join(t.TempDir(), "worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	cache := NewCache(localDB)
	conn := NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: "tok-b", WorkerID: "worker-b"}, cache, localDB, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = conn.Run(ctx) }()
	defer func() { cancel(); <-done }()
	sctx, scancel := context.WithTimeout(ctx, 5*time.Second)
	defer scancel()
	if err := conn.WaitForSync(sctx); err != nil {
		t.Fatalf("worker never synced: %v", err)
	}

	// A deploy the partner worker claimed and is driving reaches this
	// worker on the op stream. While connected it serves the installer for
	// it, as the boot-action table says.
	claimedAt := time.Now()
	op := model.Operation{
		ID: uuid.New(), MachineID: machineID, TraceID: uuid.New(), Type: model.OpDeploy,
		Status: model.OpClaimed, ClaimedBy: "worker-a", ClaimedAt: &claimedAt, Phase: "rsyncing_os", Version: 4,
		Params: json.RawMessage(`{"destructive":true}`),
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// The hub drops op messages for a conn until its hello resync is on the
	// wire; publish until the worker has the op.
	waitFor("the worker to hold the partner's deploy", func() bool {
		hub.PublishOpDelta(op, model.MachineStateDeploying)
		return cache.BootActionForMAC(mac) == model.BootDeploy
	})

	// The worker loses global and stays cut off.
	refuse.Store(true)
	conn.ForceDisconnect()
	waitFor("the worker to notice it lost global", func() bool { return !conn.Connected() })

	// Global completes the deploy through the partner. The cut-off worker
	// cannot hear it.
	op.Status, op.Version = model.OpCompleted, 9
	hub.PublishOpRemoved(op, model.MachineStateRunning)

	// Well past the trust window, the installed host reboots and this
	// worker answers its DHCP.
	time.Sleep(time.Second)
	if conn.Connected() {
		t.Fatal("harness: the worker reconnected")
	}
	// The site daemon's DHCP OnACK hook (cmd/slagworksd/site.go) records
	// the firmware's DHCP exchange as a phase of the machine's booting op
	// just before the boot script is fetched. The worker's own answer to
	// the host is not evidence that the operation is still wanted.
	if op := cache.ActiveBootingOpForMachine(machineID); op != nil {
		cache.UpdateOpPhase(op.ID.String(), "dhcp_ack")
	}
	if got := cache.BootActionForMAC(mac); got != model.BootLocal {
		t.Fatalf("a worker cut off from global for five trust windows serves boot action %v to a machine whose deploy it only knows from the op stream; want BootLocal (%v): global completed that deploy through the partner worker, and the installer would wipe a running host",
			got, model.BootLocal)
	}
}
