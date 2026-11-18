package model

import (
	"testing"

	"github.com/google/uuid"
)

//go:fix inline

// TestContentHash_OOBSlicesOrderInvariant: the same OOB dynamic slices in
// different order (NetBox/API return order isn't guaranteed) must hash
// identically, so a no-op rebuild doesn't churn the snapshot seq.
func TestContentHash_OOBSlicesOrderInvariant(t *testing.T) {
	a := SiteSnapshot{
		OOB: OOBConfig{
			Subnet: "10.254.240.0/22",
			DynamicSlices: []OOBDynamicSlice{
				{Start: "10.254.241.100", End: "10.254.241.149", Owner: "10.254.240.11"},
				{Start: "10.254.241.150", End: "10.254.241.199", Owner: "10.254.240.12"},
			},
		},
	}
	b := SiteSnapshot{
		OOB: OOBConfig{
			Subnet: "10.254.240.0/22",
			DynamicSlices: []OOBDynamicSlice{ // reversed
				{Start: "10.254.241.150", End: "10.254.241.199", Owner: "10.254.240.12"},
				{Start: "10.254.241.100", End: "10.254.241.149", Owner: "10.254.240.11"},
			},
		},
	}
	ha, err := a.ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	hb, err := b.ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("OOB slice order changed the hash: %s vs %s", ha, hb)
	}
	if a.OOB.DynamicSlices[0].Owner != "10.254.240.11" {
		t.Fatal("ContentHash mutated the caller's OOB.DynamicSlices order")
	}
}

func TestSiteSnapshotHashDeterministic(t *testing.T) {
	s := SiteSnapshot{
		SeqNum: 42,
		Site:   SiteInfo{Name: "pop3", Slug: "pop3"},
		Machines: []MachineSnapshot{
			{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), NetBoxDeviceID: new(10), Hostname: "a", State: MachineStateNew},
			{ID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), NetBoxDeviceID: new(11), Hostname: "b", State: MachineStateRunning},
		},
	}
	h1, err := s.ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	h2, err := s.ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %s vs %s", h1, h2)
	}
}

func TestSiteSnapshotHashIgnoresSeqNum(t *testing.T) {
	s1 := SiteSnapshot{SeqNum: 1, Site: SiteInfo{Slug: "pop3"}}
	s2 := SiteSnapshot{SeqNum: 999, Site: SiteInfo{Slug: "pop3"}}
	h1, _ := s1.ContentHash()
	h2, _ := s2.ContentHash()
	if h1 != h2 {
		t.Fatalf("hash should ignore SeqNum, got %s vs %s", h1, h2)
	}
}

func TestSiteSnapshotHashOrderInsensitive(t *testing.T) {
	m1 := MachineSnapshot{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Hostname: "a"}
	m2 := MachineSnapshot{ID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Hostname: "b"}
	s1 := SiteSnapshot{Machines: []MachineSnapshot{m1, m2}}
	s2 := SiteSnapshot{Machines: []MachineSnapshot{m2, m1}}
	h1, _ := s1.ContentHash()
	h2, _ := s2.ContentHash()
	if h1 != h2 {
		t.Fatalf("hash must be order-insensitive, got %s vs %s", h1, h2)
	}
}

func TestSiteSnapshotHashChangesOnField(t *testing.T) {
	s1 := SiteSnapshot{Site: SiteInfo{Slug: "pop3"}, Machines: []MachineSnapshot{{Hostname: "a"}}}
	s2 := SiteSnapshot{Site: SiteInfo{Slug: "pop3"}, Machines: []MachineSnapshot{{Hostname: "b"}}}
	h1, _ := s1.ContentHash()
	h2, _ := s2.ContentHash()
	if h1 == h2 {
		t.Fatal("hash should differ when hostname differs")
	}
}

// TestSiteSnapshotHashStableWithTieKeys guards against sort.Slice being
// unstable: two interfaces sharing a MAC (valid with bonded NICs) or two
// images sharing a Name must still produce a deterministic hash. The
// hash must be identical no matter what order the slices arrived in.
func TestSiteSnapshotHashStableWithTieKeys(t *testing.T) {
	mkSnap := func(ifaces []SnapshotInterface) SiteSnapshot {
		return SiteSnapshot{
			Site: SiteInfo{Slug: "pop3"},
			Machines: []MachineSnapshot{
				{
					ID:         uuid.MustParse("00000000-0000-0000-0000-000000000001"),
					Interfaces: ifaces,
				},
			},
		}
	}
	a := SnapshotInterface{Name: "eno1", MAC: "aa:bb:cc:dd:ee:01", IP: "10.0.0.1/24"}
	b := SnapshotInterface{Name: "bond0", MAC: "aa:bb:cc:dd:ee:01", IP: "10.0.0.2/24"}

	h1, err := mkSnap([]SnapshotInterface{a, b}).ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	h2, err := mkSnap([]SnapshotInterface{b, a}).ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash flapped on tied-MAC interfaces: %s vs %s", h1, h2)
	}
}

// TestSiteSnapshotHashDoesNotMutateReceiver guards against the naive
// "copied := s" approach: the struct copy is shallow, and sort.Slice on
// copied.Machines would reorder the caller's slice backing array too.
// Concurrent callers of ContentHash() on snapshots that share backing
// arrays would race. ContentHash must sort on independent copies.
func TestSiteSnapshotHashDoesNotMutateReceiver(t *testing.T) {
	m1 := MachineSnapshot{ID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Hostname: "b"}
	m2 := MachineSnapshot{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Hostname: "a"}
	// Insertion order is b-then-a (reverse UUID order). If ContentHash
	// mutates the caller, after the call the slice will be a-then-b.
	s := SiteSnapshot{
		Site:     SiteInfo{Slug: "pop3"},
		Machines: []MachineSnapshot{m1, m2},
		SSHKeys:  []string{"z", "a"},
	}
	if _, err := s.ContentHash(); err != nil {
		t.Fatal(err)
	}
	if s.Machines[0].Hostname != "b" || s.Machines[1].Hostname != "a" {
		t.Fatalf("ContentHash mutated caller Machines order: %+v", s.Machines)
	}
	if s.SSHKeys[0] != "z" || s.SSHKeys[1] != "a" {
		t.Fatalf("ContentHash mutated caller SSHKeys order: %+v", s.SSHKeys)
	}
}
