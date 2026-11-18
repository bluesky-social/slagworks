package worker

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestReservedIPForMAC_OOBReservation: a MAC present only in the snapshot's
// OOB reservation set (a BMC/PDU/switch-mgmt port, NOT a slagworks machine)
// resolves via ReservedIPForMAC. This is the core of the dnsmasq takeover.
func TestReservedIPForMAC_OOBReservation(t *testing.T) {
	c := NewCache(nil)
	c.ApplySnapshot(&model.SiteSnapshot{
		Reservations: []model.Reservation{
			{MAC: "28:29:86:ad:67:e5", IP: "10.254.240.152"},
		},
	})
	got := c.ReservedIPForMAC("28:29:86:ad:67:e5")
	if got == nil || got.String() != "10.254.240.152" {
		t.Fatalf("OOB reservation: got %v, want 10.254.240.152", got)
	}
	if c.ReservedIPForMAC("ff:ff:ff:ff:ff:ff") != nil {
		t.Fatal("unknown MAC should return nil")
	}
}

// TestReservedIPForMAC_MachineWinsOverOOB: when a MAC appears in BOTH the
// machine-interface set (ipByMAC) and the OOB reservation set with different
// IPs, the machine path wins (it reflects an operator-initiated allocation).
func TestReservedIPForMAC_MachineWinsOverOOB(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	mac := "aa:bb:cc:00:00:01"
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{
			{
				ID:    id,
				State: model.MachineStateRunning,
				Interfaces: []model.SnapshotInterface{
					{Name: "eno1", MAC: mac, Type: "physical", IP: "10.0.0.42/24"},
				},
			},
		},
		Reservations: []model.Reservation{
			{MAC: mac, IP: "10.254.240.99"}, // different IP, must lose
		},
	})
	got := c.ReservedIPForMAC(mac)
	if got == nil || got.String() != "10.0.0.42" {
		t.Fatalf("machine ipByMAC must win: got %v, want 10.0.0.42", got)
	}
}

// TestIsOOBManagedNonMachine: the DHCP BootEligible negative gate. True only
// for a MAC that has an OOB reservation AND is not a slagworks machine
// (a BMC/PDU/switch-mgmt port). False for a machine MAC, an unknown MAC, and
// a machine MAC that also happens to be reserved.
func TestIsOOBManagedNonMachine(t *testing.T) {
	c := NewCache(nil)
	id := uuid.New()
	machineMAC := "aa:bb:cc:00:00:01"
	bmcMAC := "28:29:86:ad:67:e5" // reserved, not a machine
	c.ApplySnapshot(&model.SiteSnapshot{
		Machines: []model.MachineSnapshot{
			{
				ID:    id,
				State: model.MachineStateRunning,
				Interfaces: []model.SnapshotInterface{
					{Name: "eno1", MAC: machineMAC, Type: "physical", IP: "10.0.0.42/24"},
				},
			},
		},
		Reservations: []model.Reservation{
			{MAC: bmcMAC, IP: "10.254.240.152"},
		},
	})

	if !c.IsOOBManagedNonMachine(bmcMAC) {
		t.Error("a reserved non-machine (BMC) MAC should be OOB-managed-non-machine (deny bootfile)")
	}
	if c.IsOOBManagedNonMachine(machineMAC) {
		t.Error("a slagworks machine MAC must NOT be denied a bootfile")
	}
	if c.IsOOBManagedNonMachine("ff:ff:ff:ff:ff:ff") {
		t.Error("an unknown MAC (auto-commission path) must NOT be denied a bootfile")
	}
}

// TestReservedIPForMAC_FullReplaceClearsOOB: ApplySnapshot full-replaces the
// reservation set, so a MAC dropped from a later snapshot stops resolving.
func TestReservedIPForMAC_FullReplaceClearsOOB(t *testing.T) {
	c := NewCache(nil)
	mac := "28:29:86:ad:67:e5"
	c.ApplySnapshot(&model.SiteSnapshot{
		Reservations: []model.Reservation{{MAC: mac, IP: "10.254.240.152"}},
	})
	if c.ReservedIPForMAC(mac) == nil {
		t.Fatal("precondition: reservation should resolve")
	}
	c.ApplySnapshot(&model.SiteSnapshot{Reservations: nil})
	if c.ReservedIPForMAC(mac) != nil {
		t.Fatal("reservation should be cleared after full-replace with empty set")
	}
}
