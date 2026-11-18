package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// adminOnlyServer builds a Server whose BMC credential sources are a
// testpg store, a NetBox stub with one device (serial SN-ADM, pop popa,
// oob_ip 10.0.0.41), and a fake admin secret. It returns the server and
// the id of the machine bound to that device.
func adminOnlyServer(t *testing.T, secret *awssecrets.AdminSecret) (*Server, uuid.UUID) {
	t.Helper()
	st := store.New(testpg.DB(t, store.Migrate))
	devID := 300
	dev := netbox.Device{
		ID: devID, Name: "kworker-a.popa", Serial: "SN-ADM",
		Site:  &netbox.NestedRef{ID: 1, Name: "popa", Slug: "popa"},
		OOBIP: &netbox.NestedIPAddress{ID: 9, Address: "10.0.0.41/24"},
	}
	f := &netboxFixture{devices: []netbox.Device{dev}, deviceByID: map[int]netbox.Device{devID: dev}}
	nb := netbox.NewClient(netbox.ClientConfig{BaseURL: f.server(t).URL, Token: "t"})
	id := uuid.New()
	if err := st.CreateMachine(&model.Machine{
		ID: id, NetBoxSerial: "SN-ADM", NetBoxDeviceID: &devID, State: model.MachineStateRunning,
	}); err != nil {
		t.Fatalf("seed machine: %v", err)
	}
	reader := &awssecrets.FakeReader{Secrets: map[string]*awssecrets.AdminSecret{}}
	if secret != nil {
		reader.Secrets["popa"] = secret
	}
	return &Server{store: st, netbox: nb, bmcSecrets: reader, log: slog.Default(), echo: echo.New()}, id
}

// TestResolveBMCRuntime_AdminSecretOnly pins the single credential source:
// the runtime creds and extra_config come only from the device's
// serial-keyed kind=bmc entry in pop/<pop>/bmc-creds-admin. There is no
// Postgres tier to prefer or fall back to.
func TestResolveBMCRuntime_AdminSecretOnly(t *testing.T) {
	s, id := adminOnlyServer(t, &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{
		"SN-ADM": {Kind: awssecrets.KindBMC, Username: "ADMIN", Password: "admin-pw", ExtraConfig: map[string]any{"ipmi_cipher": float64(3)}},
	}})
	rt, dev, note, err := s.resolveBMCRuntimeWithDevice(context.Background(), id)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if note != "" {
		t.Fatalf("unexpected credNote %q", note)
	}
	if rt.Username != "ADMIN" || rt.Password != "admin-pw" || rt.IpmiCipher() != 3 {
		t.Errorf("runtime = %+v, want the admin entry's cred and extra_config", rt)
	}
	if rt.ExpectedChassisSerial != "SN-ADM" || dev == nil || dev.Serial != "SN-ADM" {
		t.Errorf("serial gate / device: rt=%+v dev=%+v", rt, dev)
	}
}

// TestResolveBMCRuntime_NoAdminEntry_NoteNamesTheFix: without an admin
// entry there is no credential at all, and the note must point the
// operator at the concrete secret and serial key — never at Postgres or a
// nonexistent slagctl command.
func TestResolveBMCRuntime_NoAdminEntry_NoteNamesTheFix(t *testing.T) {
	s, id := adminOnlyServer(t, &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{}})
	rt, _, note, err := s.resolveBMCRuntimeWithDevice(context.Background(), id)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rt.Username != "" || rt.Password != "" {
		t.Errorf("no admin entry must yield no cred, got %+v", rt)
	}
	for _, want := range []string{"pop/popa/bmc-creds-admin", `"SN-ADM"`, "no entry for serial"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q should contain %q", note, want)
		}
	}
	for _, banned := range []string{"Postgres", "set-bmc-creds", "operator credential"} {
		if strings.Contains(note, banned) {
			t.Errorf("note %q must not mention %q", note, banned)
		}
	}
}

// TestLookupBMCAdminHost_Discrimination pins the per-case reasons the
// admin fallback can fail. These strings reach operators verbatim in
// the powerMachine 422 (the "device has no BMC" vs "creds missing"
// distinction), so each case asserts its discriminating substring.
func TestLookupBMCAdminHost_Discrimination(t *testing.T) {
	dev := &netbox.Device{ID: 582, Name: "leaf-6.pop1", Serial: "MT2206X00039"}
	withEntry := func(h awssecrets.AdminHost) *awssecrets.AdminSecret {
		return &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{"MT2206X00039": h}}
	}

	cases := []struct {
		name    string
		dev     *netbox.Device
		secret  *awssecrets.AdminSecret
		fetch   error
		wantSub string
	}{
		{
			name: "secret fetch failed",
			dev:  dev, secret: nil, fetch: errors.New("aws: throttled"),
			wantSub: "secret failed",
		},
		{
			name: "device has no serial",
			dev:  &netbox.Device{ID: 582, Name: "leaf-6.pop1"}, secret: withEntry(awssecrets.AdminHost{}),
			wantSub: "no chassis serial",
		},
		{
			name: "no entry for serial (BMC-less device)",
			dev:  dev, secret: &awssecrets.AdminSecret{BySerial: map[string]awssecrets.AdminHost{}},
			wantSub: `no entry for serial "MT2206X00039"`,
		},
		{
			name: "entry is a pdu cred, not bmc",
			dev:  dev, secret: withEntry(awssecrets.AdminHost{Kind: awssecrets.KindPDU, Username: "apc", Password: "pw"}),
			wantSub: `kind="pdu", not bmc`,
		},
		{
			name: "entry has incomplete credential",
			dev:  dev, secret: withEntry(awssecrets.AdminHost{Kind: awssecrets.KindBMC, Username: "console"}),
			wantSub: "incomplete credential",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, state := lookupBMCAdminHost("pop1", tc.dev, tc.secret, tc.fetch)
			if host != nil {
				t.Fatalf("expected nil host, got %+v", host)
			}
			if !strings.Contains(state, tc.wantSub) {
				t.Errorf("state %q does not contain %q", state, tc.wantSub)
			}
		})
	}

	// Happy path: usable bmc entry returns the host with empty state.
	host, state := lookupBMCAdminHost("pop1", dev,
		withEntry(awssecrets.AdminHost{Kind: awssecrets.KindBMC, Username: "console", Password: "pw"}), nil)
	if host == nil || state != "" {
		t.Fatalf("expected usable host with empty state, got host=%v state=%q", host, state)
	}
}

// TestNoBMCCredMsg names the op kind and carries the credNote — the
// 422 body is the only feedback the slagctl invoker gets, so it must
// say which verb was refused and exactly what was missing — and never
// points at Postgres or the nonexistent `slagctl machine set-bmc-creds`.
func TestNoBMCCredMsg(t *testing.T) {
	msg := noBMCCredMsg("deploy", "the-specific-reason")
	if !strings.Contains(msg, "refusing deploy") {
		t.Errorf("msg %q must name the refused op kind", msg)
	}
	if !strings.Contains(msg, "the-specific-reason") {
		t.Errorf("msg %q must carry the credNote", msg)
	}
	for _, banned := range []string{"set-bmc-creds", "Postgres"} {
		if strings.Contains(msg, banned) {
			t.Errorf("msg %q must not mention %q", msg, banned)
		}
	}
}

// TestBMCCredNote_RecommendsAdminEntry pins the remediation: add or
// correct the host's kind=bmc entry in pop/<pop>/bmc-creds-admin, keyed by
// its NetBox serial. Unknown pop/serial render as placeholders rather than
// blanks.
func TestBMCCredNote_RecommendsAdminEntry(t *testing.T) {
	note := bmcCredNote("pop2", "SN-1", "the-secret-reason")
	for _, want := range []string{"the-secret-reason", "pop/pop2/bmc-creds-admin", `"SN-1"`, "kind=bmc"} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q should contain %q", note, want)
		}
	}
	for _, banned := range []string{"Postgres", "set-bmc-creds"} {
		if strings.Contains(note, banned) {
			t.Errorf("note %q must not mention %q", note, banned)
		}
	}
	generic := bmcCredNote("", "", "no site")
	if !strings.Contains(generic, "pop/<pop>/bmc-creds-admin") || !strings.Contains(generic, "NetBox serial") {
		t.Errorf("unknown pop/serial should render placeholders: %q", generic)
	}
}
