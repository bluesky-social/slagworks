package httpboot

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/worker"
)

// The MAC-keyed endpoints accept either hex case in a MAC, but the worker
// cache indexes machines by the lower-case form the snapshot builder stores
// (internal/snapshot/builder.go). These tests run the real worker.Cache
// behind the real mux, so a MAC that reaches the provider un-normalized
// misses the index and the request is treated as coming from an unknown
// machine.

const caseTestMAC = "52:54:00:aa:bb:01"

var (
	caseTestMachineID = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	caseTestDeployOp  = uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
)

// newCaseTestServer returns a worker.Cache holding one machine whose NIC is
// stored as caseTestMAC, and the httpboot mux serving from it.
func newCaseTestServer(t *testing.T) (*worker.Cache, http.Handler) {
	t.Helper()
	c := worker.NewCache(nil)
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum: 1,
		Site:   model.SiteInfo{Slug: "t", Name: "t"},
		Machines: []model.MachineSnapshot{{
			ID:           caseTestMachineID,
			State:        model.MachineStateCommissioned,
			PXEEligible:  true,
			KernelParams: testKernelParams,
			SystemUUID:   "22222222-2222-4222-8222-222222222222",
			Interfaces: []model.SnapshotInterface{
				{Name: "eth0", MAC: caseTestMAC, Type: "physical"},
			},
			StorageScript: &model.SnapshotStorageScript{Content: "#!/bin/sh\necho storage\n"},
		}},
	})
	srv := NewServer(":0", ServerConfig{
		ImagesDir:    t.TempDir(),
		DefaultImage: "noble-pxe",
		DeployMode:   true,
		Provider:     c,
	})
	return c, srv.buildMux()
}

func TestMACKeyedEndpoints_UpperCaseMACResolvesSameMachine(t *testing.T) {
	upper := strings.ToUpper(caseTestMAC)

	t.Run("boot ipxe", func(t *testing.T) {
		c, mux := newCaseTestServer(t)
		c.ApplyOpDelta(model.Operation{
			ID: caseTestDeployOp, MachineID: caseTestMachineID,
			Type: model.OpDeploy, Status: model.OpClaimed, Version: 1,
		}, model.MachineStateDeploying)

		want := "slagworks.machine=" + caseTestMachineID.String()
		for _, mac := range []string{caseTestMAC, upper} {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/boot/ipxe?mac="+mac, nil))
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
				t.Errorf("GET /boot/ipxe?mac=%s: got %d, want 200 with the deploy script (%s); body:\n%s",
					mac, w.Code, want, w.Body.String())
			}
		}
	})

	t.Run("boot storage-script", func(t *testing.T) {
		_, mux := newCaseTestServer(t)
		for _, mac := range []string{caseTestMAC, upper} {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/boot/storage-script?mac="+mac, nil))
			if w.Code != http.StatusOK || w.Body.String() != "#!/bin/sh\necho storage\n" {
				t.Errorf("GET /boot/storage-script?mac=%s: got %d %q, want 200 with the machine's script",
					mac, w.Code, w.Body.String())
			}
		}
	})

	t.Run("inventory", func(t *testing.T) {
		c, mux := newCaseTestServer(t)
		body := `{"nics":[{"mac":"` + upper + `"}]}`
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/inventory", strings.NewReader(body)))
		if w.Code != http.StatusOK {
			t.Fatalf("POST /api/v1/inventory: got %d, want 200: %s", w.Code, w.Body.String())
		}
		obs, _ := c.DrainOutbox()
		var found bool
		for _, o := range obs {
			if o.Obs.Event != "inventory_import" {
				continue
			}
			found = true
			if o.Obs.MachineID != caseTestMachineID {
				t.Errorf("inventory_import machine_id = %s, want %s (NIC MAC %s not matched locally)",
					o.Obs.MachineID, caseTestMachineID, upper)
			}
		}
		if !found {
			t.Fatal("no inventory_import observation queued")
		}
	})
}
