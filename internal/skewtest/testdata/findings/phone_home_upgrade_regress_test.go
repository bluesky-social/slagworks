package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/commission"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// TestPhoneHomeQueuedByAPre06WorkerCompletesItsDeploy replays, against this
// tree's observation handler, the one record a v0.5.0 site worker keeps of a
// deploy whose installed OS phoned home while global was unreachable.
//
// Upgrading across v0.6.0 makes that the normal case: global is upgraded
// first and from then on refuses every v0.5.0 worker (the token moved to a
// header), so each worker runs disconnected until an operator upgrades it
// by hand. A deploy in flight keeps going on the worker alone: the agent
// installs, the host reboots, its phone-home gets a 2xx. v0.5.0 records
// that as an in-memory op update (lost when the package upgrade restarts
// the worker) and as a durable phone_home observation that names the
// machine but not the operation. After the upgrade the new worker replays
// that observation, global acknowledges it without touching the operation,
// the installed OS never phones home again, and the phone-home timeout
// fails a healthy host.
func TestPhoneHomeQueuedByAPre06WorkerCompletesItsDeploy(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	if err := st.CreateSiteConfig(&model.SiteConfig{Slug: "site-a", NetBoxSite: "site-a",
		EphemeralPoolStart: "192.0.2.10", EphemeralPoolEnd: "192.0.2.20"}); err != nil {
		t.Fatalf("site config: %v", err)
	}
	m := &model.Machine{NetBoxSerial: "SER-PH-1", State: model.MachineStateCommissioned, PXEEligible: true}
	if err := st.CreateMachine(m); err != nil {
		t.Fatalf("machine: %v", err)
	}
	// The deploy as global last saw it before the upgrade: claimed by the
	// site's worker and latched at awaiting_phone_home.
	op, err := st.CreateMachineOp(m.ID, model.OpDeploy, json.RawMessage(`{"destructive":true}`), model.MachineStateCommissioned)
	if err != nil {
		t.Fatalf("create deploy: %v", err)
	}
	if _, err := st.ClaimOperation(op.ID, "worker-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.SetOperationPhase(op.ID, "awaiting_phone_home"); err != nil {
		t.Fatalf("phase: %v", err)
	}

	// The outbox row, exactly as v0.5.0's Cache.RecordPhoneHome wrote it:
	// machine, event and timestamp; no operation ID. The hub stamps the
	// site and worker from the connection.
	obs := model.Observation{
		MachineID: m.ID,
		Event:     "phone_home",
		Timestamp: time.Now().Add(-10 * time.Minute),
		SiteSlug:  "site-a",
		WorkerID:  "worker-1",
	}
	h := commission.New(st, nil, nil)
	if err := h.HandleObservation(context.Background(), obs); err != nil {
		t.Fatalf("HandleObservation: %v", err)
	}

	got, err := st.GetOperation(op.ID)
	if err != nil {
		t.Fatalf("read op: %v", err)
	}
	if got.Status != model.OpCompleted {
		t.Fatalf("after the phone_home a v0.5.0 worker queued for this machine was replayed and acknowledged, the deploy is %s at phase %q; want completed. Nothing else will complete it: the host phoned home once and got its 2xx, so the phone-home timeout fails it.",
			got.Status, got.Phase)
	}
	gm, err := st.GetMachine(m.ID)
	if err != nil {
		t.Fatalf("read machine: %v", err)
	}
	if gm.State != model.MachineStateRunning || !gm.Locked {
		t.Fatalf("machine is %s (locked=%v), want running and locked", gm.State, gm.Locked)
	}
}
