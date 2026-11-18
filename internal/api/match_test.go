package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// matchResultFromSnapshot is the only logic in the match path not delegated to
// the (pg/NetBox-backed) snapshot-build chain — the resolution chain mirrors
// preflightMachine and is exercised by the snapshot-layer tests + CI integration.
// These cover the projection: matched scripts -> MatchResult, the nil-script
// fallback, and the FQDN/Hostname preference.
func TestMatchResultFromSnapshot(t *testing.T) {
	id := uuid.MustParse("00000000-0000-0000-0000-000000000042")

	t.Run("both scripts matched, prefers FQDN", func(t *testing.T) {
		ms := &model.MachineSnapshot{
			ID:            id,
			Hostname:      "kworker-l34-1.pop1",
			FQDN:          "kworker-l34-1.internal.pop1.example.net",
			NetworkScript: &model.SnapshotNetworkScript{Name: "100-pop1-l3-kworker"},
			StorageScript: &model.SnapshotStorageScript{Name: "200-supermicro-tnr-family"},
		}
		res := matchResultFromSnapshot(id, ms)
		if res.Machine != id {
			t.Errorf("Machine = %v, want %v", res.Machine, id)
		}
		if res.Hostname != "kworker-l34-1.internal.pop1.example.net" {
			t.Errorf("Hostname = %q, want the FQDN", res.Hostname)
		}
		if res.NetworkScript == nil || res.NetworkScript.Name != "100-pop1-l3-kworker" {
			t.Errorf("NetworkScript = %+v, want 100-pop1-l3-kworker", res.NetworkScript)
		}
		if res.StorageScript == nil || res.StorageScript.Name != "200-supermicro-tnr-family" {
			t.Errorf("StorageScript = %+v, want 200-supermicro-tnr-family", res.StorageScript)
		}
	})

	t.Run("no scripts matched -> nil MatchedScripts (agent fallback)", func(t *testing.T) {
		ms := &model.MachineSnapshot{ID: id, Hostname: "algo1.pop1"}
		res := matchResultFromSnapshot(id, ms)
		if res.NetworkScript != nil {
			t.Errorf("NetworkScript = %+v, want nil (no match -> agent DHCP fallback)", res.NetworkScript)
		}
		if res.StorageScript != nil {
			t.Errorf("StorageScript = %+v, want nil (no match -> default layout)", res.StorageScript)
		}
	})

	t.Run("no FQDN -> falls back to Hostname", func(t *testing.T) {
		ms := &model.MachineSnapshot{ID: id, Hostname: "leaf-6.pop1"}
		if got := matchResultFromSnapshot(id, ms).Hostname; got != "leaf-6.pop1" {
			t.Errorf("Hostname = %q, want the bare hostname when FQDN is empty", got)
		}
	})

	t.Run("only network matched", func(t *testing.T) {
		ms := &model.MachineSnapshot{
			ID:            id,
			NetworkScript: &model.SnapshotNetworkScript{Name: "150-pop2-newcage-oob-gw"},
		}
		res := matchResultFromSnapshot(id, ms)
		if res.NetworkScript == nil || res.NetworkScript.Name != "150-pop2-newcage-oob-gw" {
			t.Errorf("NetworkScript = %+v, want 150-pop2-newcage-oob-gw", res.NetworkScript)
		}
		if res.StorageScript != nil {
			t.Errorf("StorageScript = %+v, want nil", res.StorageScript)
		}
	})
}
