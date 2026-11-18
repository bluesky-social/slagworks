//go:build integration

package qemutest

import (
	"net"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Machine is the test author's view of a machine: one struct that
// drives both the snapshot entry FakeHub serves AND the VMConfig the
// QEMU subprocess boots from. A single source of truth prevents MAC
// drift between hub state and VM wire.
type Machine struct {
	ID             uuid.UUID
	MAC            string
	NetBoxDeviceID *int
	State          model.MachineState
	PXEEligible    bool
	StorageScript  *model.SnapshotStorageScript
	NetworkScript  *model.SnapshotNetworkScript
	SystemUUID     string
	BMCIP          string
	// BMCUser/BMCPass populate the snapshot's BMC cred, which production
	// global stamps from the device's bmc-creds-admin entry. Ops carry
	// the same cred in their bmc_runtime envelope.
	BMCUser string
	BMCPass string
	// KernelParams populates MachineSnapshot.KernelParams on the
	// snapshot served to site workers. Mirrors the production source
	// (NetBox device-type custom field). Empty = unset; "none" = no
	// SOL probe; anything else = console=... cmdline string.
	KernelParams string
	// Additional NICs beyond the primary MAC. The primary MAC is
	// always a "physical" interface; these are optional extras.
	ExtraNICs []model.SnapshotInterface
}

// ToSnapshot converts a Machine into a MachineSnapshot suitable for
// inclusion in a SiteSnapshot.
func (m Machine) ToSnapshot() model.MachineSnapshot {
	ifaces := []model.SnapshotInterface{
		{
			Name: "eth0",
			MAC:  m.MAC,
			Type: "physical",
		},
	}
	ifaces = append(ifaces, m.ExtraNICs...)

	// Default KernelParams to the QEMU-friendly ttyS1 string when the
	// test didn't set one explicitly. Without this, every existing
	// matrix/commission test would have to be updated to set a
	// KernelParams that matches the QEMU vm.go ttyS1 stdio routing.
	// Tests that want to exercise the empty / "none" / mismatch paths
	// set KernelParams explicitly and override this default.
	kp := m.KernelParams
	if kp == "" {
		kp = "console=ttyS1,115200n8"
	}
	ms := model.MachineSnapshot{
		ID:             m.ID,
		NetBoxDeviceID: m.NetBoxDeviceID,
		State:          m.State,
		PXEEligible:    m.PXEEligible,
		Interfaces:     ifaces,
		StorageScript:  m.StorageScript,
		NetworkScript:  m.NetworkScript,
		SystemUUID:     m.SystemUUID,
		BMCIP:          m.BMCIP,
		BMCUser:        m.BMCUser,
		BMCPass:        m.BMCPass,
		KernelParams:   kp,
	}
	return ms
}

// SnapshotBuilder builds SiteSnapshots that the FakeHub serves.
// Mutable fluent builder so tests don't have to construct the whole
// nested struct literal.
//
// Operations left SiteSnapshot in Tier 2 Phase 2: AddOperation now
// records ops that Build() registers with the FakeHub as the slug's
// active-op set, which the hub serves on the hello op_resync and Push
// fans as op_deltas. The test-facing API is unchanged.
type SnapshotBuilder struct {
	hub  *FakeHub
	slug string
	snap model.SiteSnapshot
	ops  []model.Operation
}

// NewSnapshot starts a SnapshotBuilder for the given slug.
// Defaults to sane values (SeqNum=1, ephemeral pool in the
// back half of the /24 allocated to the network).
func (e *Env) NewSnapshot(slug string, network *Network) *SnapshotBuilder {
	start, end := ephemeralPoolRange(network.Subnet)
	return &SnapshotBuilder{
		hub:  e.Hub,
		slug: slug,
		snap: model.SiteSnapshot{
			SeqNum: 1,
			Site: model.SiteInfo{
				Slug: slug,
				Name: slug,
			},
			DHCP: model.DHCPConfig{
				Subnet:             network.SubnetStr,
				Gateway:            network.Gateway,
				EphemeralPoolStart: start,
				EphemeralPoolEnd:   end,
			},
		},
	}
}

// Seq overrides SeqNum for push-ordering tests.
func (b *SnapshotBuilder) Seq(n int64) *SnapshotBuilder {
	b.snap.SeqNum = n
	return b
}

// AddMachine adds a machine entry.
func (b *SnapshotBuilder) AddMachine(m Machine) *SnapshotBuilder {
	b.snap.Machines = append(b.snap.Machines, m.ToSnapshot())
	return b
}

// AddOperation adds an operation to the slug's active-op set. Since ops
// left the snapshot (Tier 2), Build() registers these with the FakeHub so
// they're served on the hello op_resync and fanned as op_deltas by Push.
func (b *SnapshotBuilder) AddOperation(op model.Operation) *SnapshotBuilder {
	b.ops = append(b.ops, op)
	return b
}

// Build returns the assembled snapshot AND records the builder's ops as
// the slug's active-op set on the FakeHub, so a subsequent env.Hub.Push
// fans both the identity snapshot and the op stream. Build must be called
// before Push (tests already do: Build() then Push(slug, snap)).
func (b *SnapshotBuilder) Build() model.SiteSnapshot {
	if b.hub != nil {
		b.hub.SetActiveOps(b.slug, b.ops)
	}
	return b.snap
}

// ephemeralPoolRange returns the upper half of the subnet as the
// ephemeral DHCP pool. For 198.18.0.0/24 that's .129-.254.
func ephemeralPoolRange(subnet *net.IPNet) (start, end string) {
	base := subnet.IP.To4()
	lo := make(net.IP, 4)
	hi := make(net.IP, 4)
	copy(lo, base)
	copy(hi, base)
	lo[3] = 129
	hi[3] = 254
	return lo.String(), hi.String()
}

// NewOperation is a small helper for constructing a pending op.
func NewOperation(machineID uuid.UUID, opType model.OperationType) model.Operation {
	now := time.Now()
	return model.Operation{
		ID:        uuid.New(),
		MachineID: machineID,
		TraceID:   uuid.New(),
		Type:      opType,
		Status:    model.OpPending,
		CreatedAt: now,
	}
}
