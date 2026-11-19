package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// These tests exercise the shared op-creation tail (Server.createOperation)
// through all three of its callers — deployMachine, commissionMachine,
// powerMachine — against a real store. They pin the invariants the extraction
// must preserve: the 201 op-row shape per type, the all-sites invalidation, the
// preflight-rejection wiring, and the handler-local guards that deliberately
// did NOT move into the shared driver.

// fakePreflightBuilder is a SnapshotBuilder whose BuildSnapshot returns a
// canned snapshot. A snapshot with no Machines makes preflightMachine report
// the terminal "machine not in any site snapshot" gap, which is the cleanest
// way to drive the !pf.OK rejection branch without modeling every field.
type fakePreflightBuilder struct {
	snap model.SiteSnapshot
	err  error
}

func (f fakePreflightBuilder) BuildSnapshot(context.Context, string) (model.SiteSnapshot, error) {
	return f.snap, f.err
}

// opFlowServer builds a Server backed by a real (testpg) store, a
// fakeInvalidator, and no NetBox. With NetBox unwired, resolveBMCRuntime
// short-circuits to an empty (credNote-free) runtime and preflight is a no-op
// pass, so these fixtures isolate the create+invalidate tail from the
// NetBox/secret resolution machinery (which has its own tests).
func opFlowServer(t *testing.T) (*Server, *store.Store, *fakeInvalidator, *fakeOpPublisher) {
	t.Helper()
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	inv := &fakeInvalidator{}
	pub := &fakeOpPublisher{}
	// One site_config so invalidateAllSites has a slug to fan out to.
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug:               "pop-a",
		NetBoxSite:         "pop-a",
		EphemeralPoolStart: "10.0.0.10",
		EphemeralPoolEnd:   "10.0.0.250",
	}); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	s := &Server{store: st, invalidator: inv, opPublisher: pub, log: slog.Default(), echo: echo.New()}
	s.echo.POST("/api/v1/machines/:id/deploy", s.deployMachine)
	s.echo.POST("/api/v1/machines/:id/commission", s.commissionMachine)
	s.echo.POST("/api/v1/machines/:id/power", s.powerMachine)
	s.echo.POST("/api/v1/machines/:id/abort", s.abortMachine)
	return s, st, inv, pub
}

func seedMachine(t *testing.T, st *store.Store, state model.MachineState) uuid.UUID {
	t.Helper()
	id := uuid.New()
	m := &model.Machine{
		ID:           id,
		NetBoxSerial: "SN-" + id.String()[:8],
		State:        state,
		PXEEligible:  true,
	}
	if err := st.CreateMachine(m); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	return id
}

func postJSON(t *testing.T, srv *httptest.Server, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// TestCreateOperation_HappyPathPerType is the core happy-path proof: each of
// the three verbs creates the correctly-typed op row, returns 201, and fires
// the all-sites invalidation exactly once.
func TestCreateOperation_HappyPathPerType(t *testing.T) {
	s, st, inv, pub := opFlowServer(t)
	// deploy refuses with 424 when zero SSH keys are registered, so seed one.
	if err := st.CreateSSHKey(&model.SSHKey{PublicKey: "ssh-ed25519 AAAAC3Nz test"}); err != nil {
		t.Fatalf("seed ssh key: %v", err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	cases := []struct {
		name     string
		state    model.MachineState
		path     string
		body     string
		wantType model.OperationType
	}{
		{"deploy", model.MachineStateCommissioned, "deploy", `{"destructive":true}`, model.OpDeploy},
		{"commission", model.MachineStateNew, "commission", `{}`, model.OpCommission},
		{"power cycle", model.MachineStateRunning, "power", `{"action":"cycle"}`, model.OpPowerCycle},
		{"power on", model.MachineStateRunning, "power", `{"action":"on"}`, model.OpPowerOn},
		{"power off", model.MachineStateRunning, "power", `{"action":"off"}`, model.OpPowerOff},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := seedMachine(t, st, tc.state)
			beforeInv := len(inv.seen())
			beforeDelta := pub.deltaCount()

			resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+"/"+tc.path, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("status = %d, want 201", resp.StatusCode)
			}
			var op model.Operation
			if err := json.NewDecoder(resp.Body).Decode(&op); err != nil {
				t.Fatalf("decode op: %v", err)
			}
			if op.Type != tc.wantType {
				t.Errorf("op.Type = %q, want %q", op.Type, tc.wantType)
			}
			if op.MachineID != id {
				t.Errorf("op.MachineID = %v, want %v", op.MachineID, id)
			}
			if op.Status != model.OpPending {
				t.Errorf("op.Status = %q, want pending", op.Status)
			}
			// The op must persist and carry the merged bmc_runtime block
			// (proof the transport-resolution step ran and its params reached
			// the store, not just an empty create).
			got, err := st.GetOperation(op.ID)
			if err != nil {
				t.Fatalf("persisted op not found: %v", err)
			}
			var params map[string]json.RawMessage
			if err := json.Unmarshal(got.Params, &params); err != nil {
				t.Fatalf("op.Params not a JSON object: %v (%s)", err, got.Params)
			}
			if _, ok := params["bmc_runtime"]; !ok {
				t.Errorf("op.Params missing bmc_runtime block: %s", got.Params)
			}
			// Tier 2: op-create publishes a PENDING op_delta on the op
			// stream (no machine_state) and does NOT invalidate identity —
			// a pending op changed no machine state.
			if got := pub.deltaCount(); got != beforeDelta+1 {
				t.Errorf("op_delta publishes = %d, want %d (one per op create)", got-beforeDelta, 1)
			}
			if got := len(inv.seen()); got != beforeInv {
				t.Errorf("op create must NOT invalidate identity, got %d new invalidations", got-beforeInv)
			}
		})
	}
}

// TestAbortMachine_PublishesOpRemovedAndInvalidatesOnDurable pins the
// Tier-2 abort behavior: aborting a machine with an active claimed deploy
// fails the op (durable → failed), publishes an op_removed per affected op
// carrying that durable state, and fires one identity rebuild (durable
// machines.state write). The op leaving via op_removed is what drops it on
// the worker's stream without a stale-op zombie.
func TestAbortMachine_PublishesOpRemovedAndInvalidatesOnDurable(t *testing.T) {
	s, st, inv, pub := opFlowServer(t)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	id := seedMachine(t, st, model.MachineStateCommissioned)
	op, err := st.CreateMachineOp(id, model.OpDeploy, nil, model.MachineStateCommissioned)
	if err != nil {
		t.Fatalf("create deploy op: %v", err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	beforeInv := len(inv.seen())
	beforeRemoved := pub.removedCount()

	resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+"/abort", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("abort status = %d, want 200", resp.StatusCode)
	}

	if got := pub.removedCount(); got != beforeRemoved+1 {
		t.Errorf("abort op_removed publishes = %d, want 1", got-beforeRemoved)
	}
	// The claimed deploy failed to a durable `failed` state, so one identity
	// rebuild must fire.
	if got := len(inv.seen()); got != beforeInv+1 {
		t.Errorf("durable abort must invalidate once, got %d", got-beforeInv)
	}
}

// TestAbortMachine_PublishesOpRemovedWhenTheAbortErrorsAfterFailingTheOp
// pins abort's publish contract when the abort fails partway. AbortMachine
// fails the active op in its own transaction and then re-reads the machine;
// if that read errors (a dropped connection), the op is already failed in
// the database. A retried abort finds no active op and publishes nothing, so
// the erroring call itself must publish op_removed and fire the identity
// rebuild before returning the error. Otherwise the worker keeps the op as
// active, and an active op is what authorizes it to serve a deploy boot.
func TestAbortMachine_PublishesOpRemovedWhenTheAbortErrorsAfterFailingTheOp(t *testing.T) {
	s, st, inv, pub := opFlowServer(t)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	id := seedMachine(t, st, model.MachineStateCommissioned)
	op, err := st.CreateMachineOp(id, model.OpDeploy, nil, model.MachineStateCommissioned)
	if err != nil {
		t.Fatalf("create deploy op: %v", err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Fault: once the op row has been updated (the fail), every read of the
	// machines table errors. For a deploy op the only such read left is
	// AbortMachine's trailing re-read. gorm callbacks live on the shared
	// testpg handle, so they are removed before the assertions.
	db := st.DB()
	var opUpdated atomic.Bool
	if err := db.Callback().Update().After("gorm:update").Register("test:op-updated", func(tx *gorm.DB) {
		if tx.Statement.Table == "operations" && tx.Error == nil && tx.RowsAffected > 0 {
			opUpdated.Store(true)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().Before("gorm:query").Register("test:fail-machine-read", func(tx *gorm.DB) {
		if tx.Statement.Table == "machines" && opUpdated.Load() {
			_ = tx.AddError(errors.New("injected fault"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	removeFault := func() {
		_ = db.Callback().Update().Remove("test:op-updated")
		_ = db.Callback().Query().Remove("test:fail-machine-read")
	}
	t.Cleanup(removeFault)

	resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+"/abort", `{}`)
	_ = resp.Body.Close()
	removeFault()

	// Setup checks: the abort reported the error, yet the op is failed.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("setup: abort status = %d, want 500 from the injected read fault", resp.StatusCode)
	}
	if got, err := st.GetOperation(op.ID); err != nil || got.Status != model.OpFailed {
		t.Fatalf("setup: op = %+v, err %v; want failed by the abort", got, err)
	}

	pub.mu.Lock()
	removed := append([]model.Operation(nil), pub.removed...)
	removedSt := append([]model.MachineState(nil), pub.removeSt...)
	pub.mu.Unlock()
	if len(removed) != 1 || removed[0].ID != op.ID {
		t.Fatalf("op %s is failed in the database, but the abort published op_removed for %v; want exactly that op (the worker keeps it as an active op)",
			op.ID, opIDs(removed))
	}
	if removedSt[0] != model.MachineStateFailed {
		t.Errorf("op_removed machine state = %q, want failed", removedSt[0])
	}
	if got := len(inv.seen()); got != 1 {
		t.Errorf("identity rebuilds = %d, want 1 (the fail wrote durable state)", got)
	}
}

// TestCreateOperation_PreflightRejection proves the preflight wiring the tail
// owns: when the NetBox model has a gap, deploy is refused with 422 and NO op
// row is persisted. Uses a NetBox fixture + a fake SnapshotBuilder returning a
// machine-less snapshot (the terminal preflight gap). Commission takes the same
// preflight branch (only the op-type differs), so exercising deploy is enough
// to pin the shared behavior.
func TestCreateOperation_PreflightRejection(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug:               "pop-pf",
		NetBoxSite:         "pop-pf",
		EphemeralPoolStart: "10.0.0.10",
		EphemeralPoolEnd:   "10.0.0.250",
	}); err != nil {
		t.Fatalf("seed site_config: %v", err)
	}
	if err := st.CreateSSHKey(&model.SSHKey{PublicKey: "ssh-ed25519 AAAAC3Nz test"}); err != nil {
		t.Fatalf("seed ssh key: %v", err)
	}

	devID := 100
	dev := netbox.Device{
		ID:     devID,
		Name:   "kworker-x.pop-pf",
		Serial: "SN-PF",
		Site:   &netbox.NestedRef{ID: 1, Name: "pop-pf", Slug: "pop-pf"},
	}
	f := &netboxFixture{
		devices:    []netbox.Device{dev},
		deviceByID: map[int]netbox.Device{devID: dev},
	}
	nb := netbox.NewClient(netbox.ClientConfig{BaseURL: f.server(t).URL, Token: "t"})

	id := uuid.New()
	if err := st.CreateMachine(&model.Machine{
		ID:             id,
		NetBoxSerial:   "SN-PF",
		NetBoxDeviceID: &devID,
		State:          model.MachineStateCommissioned,
		PXEEligible:    true,
	}); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	inv := &fakeInvalidator{}
	s := &Server{
		store:       st,
		invalidator: inv,
		netbox:      nb,
		// A kind=bmc admin entry so resolveBMCRuntime's cred gate passes
		// (empty credNote) and execution reaches preflight — the branch
		// under test.
		bmcSecrets: &awssecrets.FakeReader{Secrets: map[string]*awssecrets.AdminSecret{
			"pop-pf": {BySerial: map[string]awssecrets.AdminHost{
				"SN-PF": {Kind: awssecrets.KindBMC, Username: "ADMIN", Password: "pw"},
			}},
		}},
		snapshotBuilder: fakePreflightBuilder{snap: model.SiteSnapshot{}}, // no Machines -> gap
		log:             slog.Default(),
		echo:            echo.New(),
	}
	s.echo.POST("/api/v1/machines/:id/deploy", s.deployMachine)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+"/deploy", `{"destructive":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (preflight gap)", resp.StatusCode)
	}
	// No op row must exist: preflight rejects BEFORE the store create.
	ops, err := st.ListOperations(&id)
	if err != nil {
		t.Fatalf("list ops: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("preflight rejection persisted %d op(s); want 0", len(ops))
	}
	if got := inv.seen(); len(got) != 0 {
		t.Errorf("preflight rejection fired %d invalidation(s); want 0", len(got))
	}
}

// TestDeployGuards_StayHandlerLocal pins the deploy-only guards that the
// refactor deliberately kept in deployMachine rather than folding into the
// shared tail: the no-SSH-key refusal (424) and the wipe_all_disks-without-
// destructive contradiction (400). Both must reject before any op is created.
func TestDeployGuards_StayHandlerLocal(t *testing.T) {
	t.Run("wipe_all_disks requires destructive -> 400", func(t *testing.T) {
		s, st, _, _ := opFlowServer(t)
		// Seed a key so we're past the SSH gate and truly testing the wipe gate.
		if err := st.CreateSSHKey(&model.SSHKey{PublicKey: "ssh-ed25519 AAAAC3Nz test"}); err != nil {
			t.Fatalf("seed ssh key: %v", err)
		}
		srv := httptest.NewServer(s.echo)
		defer srv.Close()
		id := seedMachine(t, st, model.MachineStateCommissioned)
		resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+"/deploy",
			`{"wipe_all_disks":true,"destructive":false}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("no ssh keys -> 424", func(t *testing.T) {
		s, st, _, _ := opFlowServer(t) // no SSH keys seeded
		srv := httptest.NewServer(s.echo)
		defer srv.Close()
		id := seedMachine(t, st, model.MachineStateCommissioned)
		resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+"/deploy", `{"destructive":true}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusFailedDependency {
			t.Fatalf("status = %d, want 424", resp.StatusCode)
		}
	})
}

// TestCreateOperation_UnknownMachine documents the deliberately DIVERGENT
// unknown-machine status across the handlers, which the refactor preserves:
//   - power resolves + GetMachine's the machine IN THE HANDLER, so a missing
//     machine is a clean 404;
//   - deploy/commission (with NetBox unwired) skip the resolve entirely and let
//     the store's create reject the missing machine as a 409.
//
// These codes are load-bearing (slagctl/operators branch on them); the test
// locks them so a future "normalize the error" edit fails loudly.
func TestCreateOperation_UnknownMachine(t *testing.T) {
	s, st, _, _ := opFlowServer(t)
	if err := st.CreateSSHKey(&model.SSHKey{PublicKey: "ssh-ed25519 AAAAC3Nz test"}); err != nil {
		t.Fatalf("seed ssh key: %v", err)
	}
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	missing := uuid.New().String()
	cases := []struct {
		name string
		path string
		body string
		want int
	}{
		{"power -> 404", "power", `{"action":"cycle"}`, http.StatusNotFound},
		{"deploy -> 409", "deploy", `{"destructive":true}`, http.StatusConflict},
		{"commission -> 409", "commission", `{}`, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postJSON(t, srv, "/api/v1/machines/"+missing+"/"+tc.path, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}
