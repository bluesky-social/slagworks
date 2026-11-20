package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/snapshot"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// TestPreflightGapMsg_LockedMachineIsNotAModelGap pins the refusal wording
// for a locked machine. The lock is one of preflight's gaps, and the refusal
// used to file every gap under "has an incomplete NetBox model", which sent
// the operator to NetBox for a host whose model was complete.
func TestPreflightGapMsg_LockedMachineIsNotAModelGap(t *testing.T) {
	t.Run("lock only", func(t *testing.T) {
		got := preflightGapMsg("deploy", "host-a", []string{snapshot.GapMachineLocked})
		if strings.Contains(got, "incomplete NetBox model") {
			t.Errorf("a refusal for the lock alone blames the NetBox model: %q", got)
		}
		for _, want := range []string{"refusing deploy: host-a is locked", "`slagctl machine unlock host-a`"} {
			if !strings.Contains(got, want) {
				t.Errorf("refusal %q does not contain %q", got, want)
			}
		}
	})
	t.Run("lock and a model gap", func(t *testing.T) {
		gaps := []string{snapshot.GapMachineLocked, "no primary_ip4"}
		got := preflightGapMsg("commission", "host-a", gaps)
		for _, want := range []string{
			"refusing commission: host-a is locked",
			"`slagctl machine unlock host-a`",
			"It also has an incomplete NetBox model (1 issue(s)):\n  - no primary_ip4",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("refusal %q does not contain %q", got, want)
			}
		}
		if len(gaps) != 2 || gaps[0] != snapshot.GapMachineLocked {
			t.Errorf("preflightGapMsg changed the caller's gaps: %q", gaps)
		}
	})
	t.Run("model gaps only", func(t *testing.T) {
		got := preflightGapMsg("deploy", "host-a", []string{"no primary_ip4", "no boot NIC"})
		want := "refusing deploy: host-a has an incomplete NetBox model (2 issue(s)):\n  - no primary_ip4\n  - no boot NIC"
		if got != want {
			t.Errorf("refusal = %q, want %q", got, want)
		}
	})
}

// TestCreateOperation_LockedMachineRefusalSaysLocked drives the same wording
// through POST /machines/:id/deploy: a locked machine in the site snapshot is
// refused with a body that names the lock and the unlock command.
func TestCreateOperation_LockedMachineRefusalSaysLocked(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug:               "site-a",
		NetBoxSite:         "site-a",
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
		Name:   "host-a.site-a",
		Serial: "SN-LOCK",
		Site:   &netbox.NestedRef{ID: 1, Name: "site-a", Slug: "site-a"},
	}
	f := &netboxFixture{
		devices:    []netbox.Device{dev},
		deviceByID: map[int]netbox.Device{devID: dev},
	}
	nb := netbox.NewClient(netbox.ClientConfig{BaseURL: f.server(t).URL, Token: "t"})

	id := uuid.New()
	if err := st.CreateMachine(&model.Machine{
		ID:             id,
		NetBoxSerial:   "SN-LOCK",
		NetBoxDeviceID: &devID,
		State:          model.MachineStateRunning,
		PXEEligible:    true,
		Locked:         true,
	}); err != nil {
		t.Fatalf("seed machine: %v", err)
	}

	s := &Server{
		store:       st,
		invalidator: &fakeInvalidator{},
		netbox:      nb,
		bmcSecrets: &awssecrets.FakeReader{Secrets: map[string]*awssecrets.AdminSecret{
			"site-a": {BySerial: map[string]awssecrets.AdminHost{
				"SN-LOCK": {Kind: awssecrets.KindBMC, Username: "ADMIN", Password: "pw"},
			}},
		}},
		snapshotBuilder: fakePreflightBuilder{snap: model.SiteSnapshot{Machines: []model.MachineSnapshot{{
			ID:          id,
			State:       model.MachineStateRunning,
			Locked:      true,
			PXEEligible: true,
		}}}},
		log:  slog.Default(),
		echo: echo.New(),
	}
	s.echo.POST("/api/v1/machines/:id/deploy", s.deployMachine)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	resp := postJSON(t, srv, "/api/v1/machines/"+id.String()+"/deploy", `{}`)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"refusing deploy: " + id.String() + " is locked",
		"slagctl machine unlock " + id.String(),
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("refusal body %s does not contain %q", body, want)
		}
	}
	if strings.Contains(string(body), id.String()+" has an incomplete NetBox model") {
		t.Errorf("refusal body leads with the NetBox model for a locked machine: %s", body)
	}
	ops, err := st.ListOperations(&id)
	if err != nil {
		t.Fatalf("list ops: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("the refusal persisted %d op(s); want 0", len(ops))
	}
}
