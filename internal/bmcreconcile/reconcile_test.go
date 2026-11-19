package bmcreconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestParseUserList covers the ipmitool output parser. Layouts used
// in the wild vary across vendors; keep adding fixtures as we hit
// new ones.
func TestParseUserList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []User
	}{
		{
			name: "supermicro 4-user",
			in: `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    true       true       OPERATOR
4   monitor          true    false      true       USER
`,
			want: []User{
				{Slot: 1, Name: "", Privilege: PrivAdmin, Enabled: true},
				{Slot: 2, Name: "ADMIN", Privilege: PrivAdmin, Enabled: true},
				{Slot: 3, Name: "slagworks", Privilege: PrivOperator, Enabled: true},
				{Slot: 4, Name: "monitor", Privilege: PrivUser, Enabled: true},
			},
		},
		{
			name: "no-access slot",
			in: `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      NO ACCESS
2   ADMIN            true    true       true       ADMINISTRATOR
`,
			want: []User{
				{Slot: 1, Name: "", Privilege: 0, Enabled: false},
				{Slot: 2, Name: "ADMIN", Privilege: PrivAdmin, Enabled: true},
			},
		},
		{
			// The bytes ipmitool itself writes into the pipe, TAB header
			// included. Names up to IPMI's 16-byte maximum come through
			// whole: HPE ships "Administrator" at slot 2.
			name: "ipmitool raw output, TAB header",
			in: ipmitoolUserListHeader +
				ipmitoolUserRow(1, "", "true", "false", "false", "ADMINISTRATOR") +
				ipmitoolUserRow(2, "Administrator", "true", "true", "true", "ADMINISTRATOR") +
				ipmitoolUserRow(3, "monitor", "true", "true", "true", "USER") +
				ipmitoolUserRow(4, "ipmi-exporter-ro", "true", "true", "true", "OPERATOR") +
				ipmitoolUserRow(5, "", "true", "false", "false", "NO ACCESS"),
			want: []User{
				{Slot: 1, Name: "", Privilege: PrivAdmin, Enabled: true},
				{Slot: 2, Name: "Administrator", Privilege: PrivAdmin, Enabled: true},
				{Slot: 3, Name: "monitor", Privilege: PrivUser, Enabled: true},
				{Slot: 4, Name: "ipmi-exporter-ro", Privilege: PrivOperator, Enabled: true},
				{Slot: 5, Name: "", Privilege: 0, Enabled: false},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUserList(tc.in)
			if err != nil {
				t.Fatalf("parseUserList: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len mismatch: got %d, want %d (%+v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("row %d: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ipmitoolUserListHeader is the header row ipmitool writes for `user list`,
// byte for byte. It is a C string literal (lib/ipmi_user.c,
// dump_user_access) holding two TABs; a terminal expands them to 8-column
// stops, which puts each heading over its data column. The space-separated
// headers elsewhere in this file are that terminal rendering.
const ipmitoolUserListHeader = "ID  Name\t     Callin  Link Auth\tIPMI Msg   Channel Priv Limit\n"

// ipmitoolUserRow renders one `user list` data row as ipmitool does:
// printf("%-4d%-17s%-8s%-11s%-11s%-s\n"), space-padded with no TABs.
func ipmitoolUserRow(slot int, name, callin, linkAuth, ipmiMsg, priv string) string {
	return fmt.Sprintf("%-4d%-17s%-8s%-11s%-11s%-s\n", slot, name, callin, linkAuth, ipmiMsg, priv)
}

// TestReconcile_RawIpmitoolOutput_InBandRedeployIsNoOp runs an in-band
// redeploy against the bytes real ipmitool writes (ipmitoolUserListHeader).
// The monitor account is already provisioned, so provision-once means no
// write of any kind but the boot order, and the slot-2 gate must accept the
// bmc-creds-admin account at slot 2 whatever its length.
func TestReconcile_RawIpmitoolOutput_InBandRedeployIsNoOp(t *testing.T) {
	for _, admin := range []string{"ADMIN", "Administrator"} {
		t.Run(admin, func(t *testing.T) {
			fake := newFakeIPMITool(t)
			table := ipmitoolUserListHeader +
				ipmitoolUserRow(1, "", "true", "false", "false", "ADMINISTRATOR") +
				ipmitoolUserRow(2, admin, "true", "true", "true", "ADMINISTRATOR") +
				ipmitoolUserRow(3, "monitor", "true", "true", "true", "USER")
			fake.queueOutput("user list 1", table)
			fake.queueOutput("chassis bootparam set bootflag force_pxe options=PEF,timestamp,reset_persistence", "")
			fake.queueOutput("user list 1", table)

			res, err := Reconcile(context.Background(), Options{
				IPMITool:  fake.path,
				Desired:   []DesiredUser{{Name: MonitorUser, Privilege: PrivUser}},
				AdminUser: admin,
			})
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if len(res.CreatedOrUpdated) != 0 {
				t.Errorf("redeploy rewrote a provisioned account's password: %+v", res.CreatedOrUpdated)
			}
			for _, c := range fake.calls() {
				if !strings.Contains(c.Args, "user list 1") && !strings.Contains(c.Args, "bootparam") {
					t.Errorf("redeploy of a compliant BMC issued %q", c.Args)
				}
			}
		})
	}
}

// TestReconcile_FullProvisioning_FreshBMC drives the end-to-end happy
// path: BMC has only the vendor ADMIN at slot 2; reconcile must add
// slagworks + monitor to slots 3+4, set persistent PXE boot. No
// leftovers to disable.
func TestReconcile_FullProvisioning_FreshBMC(t *testing.T) {
	fake := newFakeIPMITool(t)

	// Initial user list: 4-slot BMC with only slot 2 (ADMIN) populated.
	fake.queueOutput("user list 1", `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3                    true    false      false      NO ACCESS
4                    true    false      false      NO ACCESS
`)
	// Setter sequence for slots 3 and 4, then boot order.
	for _, cmd := range []string{
		"user priv 3 15 1",
		"user set name 3 slagworks",
		"user set password 3 *",
		"user priv 3 3 1",
		"channel setaccess 1 3 callin=on ipmi=on link=on privilege=3",
		"user enable 3",
		"user priv 4 15 1",
		"user set name 4 monitor",
		"user set password 4 *",
		"user priv 4 2 1",
		"channel setaccess 1 4 callin=on ipmi=on link=on privilege=2",
		"user enable 4",
		"chassis bootparam set bootflag force_pxe options=PEF,timestamp,reset_persistence",
	} {
		fake.queueOutput(cmd, "")
	}
	// Post-reconcile re-read shows the new users.
	fake.queueOutput("user list 1", `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    true       true       OPERATOR
4   monitor          true    true       true       USER
`)

	res, err := Reconcile(context.Background(), Options{
		IPMITool:      fake.path,
		IPMIInterface: "open",
		Desired: []DesiredUser{
			{Name: "slagworks", Privilege: PrivOperator},
			{Name: "monitor", Privilege: PrivUser},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(res.CreatedOrUpdated) != 2 {
		t.Fatalf("expected 2 provisioned users, got %d: %+v", len(res.CreatedOrUpdated), res.CreatedOrUpdated)
	}
	byName := map[string]ProvisionedUser{}
	for _, u := range res.CreatedOrUpdated {
		byName[u.Name] = u
	}
	if u, ok := byName["slagworks"]; !ok {
		t.Error("slagworks not in CreatedOrUpdated")
	} else if u.Privilege != PrivOperator || u.Slot != 3 || u.Password == "" {
		t.Errorf("slagworks: %+v", u)
	}
	if u, ok := byName["monitor"]; !ok {
		t.Error("monitor not in CreatedOrUpdated")
	} else if u.Privilege != PrivUser || u.Slot != 4 || u.Password == "" {
		t.Errorf("monitor: %+v", u)
	}
	if !res.BootOrderPersisted {
		t.Error("BootOrderPersisted: expected true")
	}
	if len(res.LeftoversDisabled) != 0 {
		t.Errorf("LeftoversDisabled: expected none, got %v", res.LeftoversDisabled)
	}
}

// TestReconcile_AlreadyProvisioned_NoOp covers the subsequent-deploy
// happy path: BMC already has slagworks + monitor with the right
// privileges. Reconcile should make no setter calls except the
// boot-order write (always fired; idempotent).
func TestReconcile_AlreadyProvisioned_NoOp(t *testing.T) {
	fake := newFakeIPMITool(t)
	provisioned := `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    true       true       OPERATOR
4   monitor          true    true       true       USER
`
	fake.queueOutput("user list 1", provisioned)
	fake.queueOutput("chassis bootparam set bootflag force_pxe options=PEF,timestamp,reset_persistence", "")
	fake.queueOutput("user list 1", provisioned)

	res, err := Reconcile(context.Background(), Options{
		IPMITool: fake.path,
		Desired: []DesiredUser{
			{Name: "slagworks", Privilege: PrivOperator},
			{Name: "monitor", Privilege: PrivUser},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.CreatedOrUpdated) != 0 {
		t.Errorf("expected no writes, got %+v", res.CreatedOrUpdated)
	}
	if len(res.LeftoversDisabled) != 0 {
		t.Errorf("expected no leftover disables, got %v", res.LeftoversDisabled)
	}
	if !res.BootOrderPersisted {
		t.Error("BootOrderPersisted should be true even when users were already correct")
	}
}

// TestReconcile_WrongPriv_FixedInPlace_NoPasswordRewrite is the
// provision-once guarantee: an EXISTING user with the wrong privilege gets
// its privilege/access/enabled state corrected WITHOUT rewriting its password,
// and is NOT reported in CreatedOrUpdated (the cred slagworksd holds is
// unchanged, so there is nothing new to phone home). slagworks exists at
// OPERATOR; Desired is ADMINISTRATOR — reconcile must bump priv in place.
//
// The assertion is structural: the fake fails loudly on any unqueued command,
// and we deliberately do NOT queue a `user set password 3` — so if reconcile
// regenerates/rewrites the password (the old every-deploy-rotates behavior),
// the fake errors and this test fails.
func TestReconcile_WrongPriv_FixedInPlace_NoPasswordRewrite(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    true       true       OPERATOR
4   monitor          true    true       true       USER
`)
	// ONLY priv/access/enable for slot 3 (no `user set password`), then boot order.
	for _, cmd := range []string{
		"user priv 3 4 1",
		"channel setaccess 1 3 callin=on ipmi=on link=on privilege=4",
		"user enable 3",
		"chassis bootparam set bootflag force_pxe options=PEF,timestamp,reset_persistence",
	} {
		fake.queueOutput(cmd, "")
	}
	fake.queueOutput("user list 1", `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    true       true       ADMINISTRATOR
4   monitor          true    true       true       USER
`)

	res, err := Reconcile(context.Background(), Options{
		IPMITool: fake.path,
		Desired: []DesiredUser{
			{Name: "slagworks", Privilege: PrivAdmin}, // was OPERATOR on the BMC
			{Name: "monitor", Privilege: PrivUser},    // already correct
		},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// No cred reported: the password was untouched, so nothing to phone home.
	if len(res.CreatedOrUpdated) != 0 {
		t.Errorf("expected NO CreatedOrUpdated (password untouched), got %+v", res.CreatedOrUpdated)
	}
	if len(res.LeftoversDisabled) != 0 {
		t.Errorf("expected no leftovers, got %v", res.LeftoversDisabled)
	}
}

// TestReconcile_PartialCreate_DisabledUser_GetsFreshPassword guards the
// partial-create recovery path: create does NO ACCESS -> set-name ->
// set-password -> priv -> ENABLE, so a crash after set-name (but before the
// password write) leaves the slot NAMED-BUT-DISABLED with no usable
// password. A reconcile retry MUST treat such a slot as needing (re)provisioning — write a fresh password and
// report the cred — NOT silently enable it at the right privilege with an
// unknown password (which would strand the account, with slagworksd holding no
// valid cred). slagworks exists at slot 3 but is DISABLED.
func TestReconcile_PartialCreate_DisabledUser_GetsFreshPassword(t *testing.T) {
	fake := newFakeIPMITool(t)
	// slagworks present but DISABLED (NO ACCESS) — the partial-create remnant.
	fake.queueOutput("user list 1", `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    false      false      NO ACCESS
4   monitor          true    true       true       USER
`)
	// MUST include a `user set password 3` (fresh provision), then priv/enable.
	// NOTE: no `user set name 3` — the name is already present.
	for _, cmd := range []string{
		"user set password 3 *",
		"user priv 3 4 1",
		"channel setaccess 1 3 callin=on ipmi=on link=on privilege=4",
		"user enable 3",
		"chassis bootparam set bootflag force_pxe options=PEF,timestamp,reset_persistence",
	} {
		fake.queueOutput(cmd, "")
	}
	fake.queueOutput("user list 1", `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    true       true       ADMINISTRATOR
4   monitor          true    true       true       USER
`)

	res, err := Reconcile(context.Background(), Options{
		IPMITool: fake.path,
		Desired: []DesiredUser{
			{Name: "slagworks", Privilege: PrivAdmin},
			{Name: "monitor", Privilege: PrivUser},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The disabled slagworks slot must be re-provisioned with a fresh cred.
	if len(res.CreatedOrUpdated) != 1 {
		t.Fatalf("expected 1 provisioned (the disabled slagworks), got %+v", res.CreatedOrUpdated)
	}
	u := res.CreatedOrUpdated[0]
	if u.Name != "slagworks" || u.Slot != 3 || u.Password == "" || u.Privilege != PrivAdmin {
		t.Errorf("re-provisioned slagworks: %+v (want name=slagworks slot=3 priv=ADMIN non-empty pw)", u)
	}
}

// TestReconcile_PartialCreate_ReusedSlot_GetsFreshPassword: deploy 1 creates
// the monitor in a free slot that still lists a real privilege limit, as a
// slot the leftover pass disabled and cleared does (that pass leaves the
// limit and the old password in place), and stops after the name write
// because the BMC refuses the password. `user list` has no enable column, so
// reconcile reads a slot as disabled only when it lists NO ACCESS, and only
// a disabled remnant gets a fresh password. Were the remnant to list the old
// limit, deploy 2 would read an enabled monitor and only fix its privilege
// and enable it, putting the previous occupant's password live under the
// monitor name, or at USER do nothing at all; either way it reports no cred.
// After deploy 2 the monitor must be enabled at USER with a password one of
// the deploys reported.
func TestReconcile_PartialCreate_ReusedSlot_GetsFreshPassword(t *testing.T) {
	for _, tc := range []struct {
		name         string
		leftoverPriv PrivLevel
	}{
		{"never-used slot", privNoAccess},
		{"cleared leftover at OPERATOR", PrivOperator},
		{"cleared leftover at USER", PrivUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bmc := newStatefulBMC(t, map[int]bmcSlot{
				1: {Priv: privNoAccess},
				2: {Name: "ADMIN", Priv: PrivAdmin, Password: "Adm1n!pw", Enabled: true},
				3: {Priv: tc.leftoverPriv, Password: "Old!leftover1"},
			})
			opt := Options{IPMITool: bmc.path, Desired: []DesiredUser{{Name: MonitorUser, Privilege: PrivUser}}}

			bmc.refusePasswordWrites(true)
			res1, err := Reconcile(context.Background(), opt)
			if err == nil {
				t.Fatal("deploy 1: expected the refused password write to fail the reconcile")
			}
			if got := bmc.slot(3).Name; got != MonitorUser {
				t.Fatalf("deploy 1 should have named slot 3 before the password write; slot 3 name = %q", got)
			}

			bmc.refusePasswordWrites(false)
			res2, err := Reconcile(context.Background(), opt)
			if err != nil {
				t.Fatalf("deploy 2: %v", err)
			}

			var reported []string
			for _, res := range []*Result{res1, res2} {
				if res == nil {
					continue
				}
				for _, u := range res.CreatedOrUpdated {
					reported = append(reported, u.Password)
				}
			}
			got := bmc.slot(3)
			if got.Name != MonitorUser || !got.Enabled || got.Priv != PrivUser {
				t.Fatalf("after deploy 2 slot 3 is %+v; want monitor enabled at USER", got)
			}
			if !slices.Contains(reported, got.Password) {
				t.Fatalf("the monitor at slot 3 holds password %q, which no deploy reported (reported: %q)", got.Password, reported)
			}
		})
	}
}

// TestReconcile_Leftovers_Disabled covers the stale-MAAS scenario:
// BMC has an unexpected user at slot 5. Reconcile must disable it
// and clear the name. The vendor ADMIN at slot 2 is preserved because
// it's the AWS-managed recovery cred.
func TestReconcile_Leftovers_Disabled(t *testing.T) {
	fake := newFakeIPMITool(t)
	withMaas := `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    true       true       OPERATOR
4   monitor          true    true       true       USER
5   maas             true    true       true       OPERATOR
`
	fake.queueOutput("user list 1", withMaas)
	// Leftover-cleanup sequence for slot 5 (maas), then boot order.
	for _, cmd := range []string{
		"user disable 5",
		"user set name 5 ",
		"chassis bootparam set bootflag force_pxe options=PEF,timestamp,reset_persistence",
	} {
		fake.queueOutput(cmd, "")
	}
	fake.queueOutput("user list 1", `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    true       true       OPERATOR
4   monitor          true    true       true       USER
5                    true    false      false      NO ACCESS
`)

	res, err := Reconcile(context.Background(), Options{
		IPMITool: fake.path,
		Desired: []DesiredUser{
			{Name: "slagworks", Privilege: PrivOperator},
			{Name: "monitor", Privilege: PrivUser},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.CreatedOrUpdated) != 0 {
		t.Errorf("expected no provision writes, got %+v", res.CreatedOrUpdated)
	}
	if len(res.LeftoversDisabled) != 1 || res.LeftoversDisabled[0] != "maas" {
		t.Errorf("LeftoversDisabled: got %v, want [maas]", res.LeftoversDisabled)
	}
}

// TestReconcile_LeftoverNameClearRefused_StillDisablesAll covers Supermicro
// BMCs, which reject `user set name <slot> ""`: the leftover is already
// disabled, so the failed clear is recorded and the remaining leftovers are
// still disabled instead of aborting the pass.
func TestReconcile_LeftoverNameClearRefused_StillDisablesAll(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      Unknown (0x00)
2   ADMIN            false   false      true       ADMINISTRATOR
3   fwupd            true    false      false      Unknown (0x00)
4   maas             true    true       true       ADMINISTRATOR
5   monitor          true    true       true       USER
`)
	fake.queueOutput("user disable 3", "")
	fake.queueFailure("user set name 3 ", "Set User Name command failed (user 3, name ): Invalid data field in request", 1)
	fake.queueOutput("user disable 4", "")
	fake.queueFailure("user set name 4 ", "Set User Name command failed (user 4, name ): Invalid data field in request", 1)
	fake.queueOutput("user list 1", `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      Unknown (0x00)
2   ADMIN            false   false      true       ADMINISTRATOR
3   fwupd            true    false      false      Unknown (0x00)
4   maas             true    true       true       ADMINISTRATOR
5   monitor          true    true       true       USER
`)

	res, err := Reconcile(context.Background(), Options{
		IPMITool:      fake.path,
		Desired:       []DesiredUser{{Name: "monitor", Privilege: PrivUser}},
		SkipBootOrder: true,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := strings.Join(res.LeftoversDisabled, ","); got != "fwupd,maas" {
		t.Errorf("LeftoversDisabled = %q, want fwupd,maas", got)
	}
	if got := strings.Join(res.NamesNotCleared, ","); got != "fwupd,maas" {
		t.Errorf("NamesNotCleared = %q, want fwupd,maas", got)
	}
}

// TestReconcile_NoEmptySlot_TakesOverLeftover: every non-reserved slot
// holds a stray named account (maas, olduser), so there is no empty slot
// for monitor. Reconcile must still converge: monitor takes over the first
// leftover's slot and the other leftover is disabled as usual. The taken
// slot is disabled and set to NO ACCESS before it is renamed, so a run
// that dies before the password write leaves a slot `user list` shows as
// disabled (re-provisioned next run), never one that reads as an enabled
// `monitor` still carrying the stray's password. Reconcile used to refuse
// with "need 1 free slots, found 0" before any write, on every run,
// leaving both strays enabled.
func TestReconcile_NoEmptySlot_TakesOverLeftover(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "maas", "ADMINISTRATOR"},
		[3]string{"4", "olduser", "OPERATOR"},
	))
	want := []string{
		"user list 1",
		"user disable 3",
		"user priv 3 15 1",
		"user set name 3 monitor",
		"user set password 3 *",
		"user priv 3 2 1",
		"channel setaccess 1 3 callin=on ipmi=on link=on privilege=2",
		"user enable 3",
		"user disable 4",
		"user set name 4 ",
		"user list 1",
	}
	for _, cmd := range want[1 : len(want)-1] {
		fake.queueOutput(cmd, "")
	}
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "monitor", "USER"},
		[3]string{"4", "", ""},
	))

	opt := RemoteOptions(LANTarget{Host: "192.0.2.10", Username: "ADMIN", Password: "adminpw!"}, "")
	opt.IPMITool = fake.path
	res, err := Reconcile(context.Background(), opt)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.CreatedOrUpdated) != 1 || res.CreatedOrUpdated[0].Name != MonitorUser || res.CreatedOrUpdated[0].Slot != 3 ||
		len(res.CreatedOrUpdated[0].Password) != passwordLength {
		t.Fatalf("expected monitor provisioned at slot 3 with a generated password, got %+v", res.CreatedOrUpdated)
	}
	if !slices.Equal(res.LeftoversDisabled, []string{"maas", "olduser"}) {
		t.Errorf("LeftoversDisabled = %v, want [maas olduser]", res.LeftoversDisabled)
	}
	var got []string
	for _, c := range fake.calls() {
		cmd := strings.TrimPrefix(c.Args, "-I lanplus -H 192.0.2.10 -U ADMIN -C 17 -E ")
		if strings.HasPrefix(cmd, "user set password 3 ") {
			cmd = "user set password 3 *"
		}
		got = append(got, cmd)
	}
	if !slices.Equal(got, want) {
		t.Errorf("ipmitool calls:\n got %q\nwant %q", got, want)
	}
}

// TestReconcile_TakeoverSparesDesiredAccounts: only leftovers are taken
// over. A desired account that needs no write (slagworks, already
// compliant at slot 3) is never a candidate, even though it comes first;
// monitor takes the maas slot. With no leftover to take, reconcile refuses
// before any write and names the account it has no slot for.
func TestReconcile_TakeoverSparesDesiredAccounts(t *testing.T) {
	desired := []DesiredUser{
		{Name: "slagworks", Privilege: PrivOperator},
		{Name: "monitor", Privilege: PrivUser},
	}

	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "slagworks", "OPERATOR"},
		[3]string{"4", "maas", "ADMINISTRATOR"},
	))
	for _, cmd := range []string{
		"user disable 4",
		"user priv 4 15 1",
		"user set name 4 monitor",
		"user set password 4 *",
		"user priv 4 2 1",
		"channel setaccess 1 4 callin=on ipmi=on link=on privilege=2",
		"user enable 4",
	} {
		fake.queueOutput(cmd, "")
	}
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "slagworks", "OPERATOR"},
		[3]string{"4", "monitor", "USER"},
	))
	res, err := Reconcile(context.Background(), Options{IPMITool: fake.path, Desired: desired, SkipBootOrder: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.CreatedOrUpdated) != 1 || res.CreatedOrUpdated[0].Slot != 4 {
		t.Errorf("expected monitor provisioned at slot 4 (the maas slot), got %+v", res.CreatedOrUpdated)
	}
	if !slices.Equal(res.LeftoversDisabled, []string{"maas"}) {
		t.Errorf("LeftoversDisabled = %v, want [maas]", res.LeftoversDisabled)
	}
	for _, c := range fake.calls() {
		for _, slot3 := range []string{"user disable 3", "user set name 3", "user set password 3", "user priv 3", "setaccess 1 3", "user enable 3"} {
			if strings.Contains(c.Args, slot3) {
				t.Errorf("the compliant slagworks account at slot 3 must not be touched: %q", c.Args)
			}
		}
	}

	full := newFakeIPMITool(t)
	full.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "slagworks", "OPERATOR"},
	))
	res, err = Reconcile(context.Background(), Options{IPMITool: full.path, Desired: desired, SkipBootOrder: true})
	if err == nil || res != nil {
		t.Fatalf("expected a refusal with a nil Result, got res=%+v err=%v", res, err)
	}
	if !strings.Contains(err.Error(), `"monitor"`) {
		t.Errorf("error should name the account it has no slot for: %v", err)
	}
	if calls := full.calls(); len(calls) != 1 {
		t.Errorf("expected only the user list before refusing, got %d calls: %+v", len(calls), calls)
	}
}

// TestReconcile_PreservesVendorAdmin verifies that the vendor ADMIN
// at slot 2 is never disabled, even though it's not in Desired. ADMIN
// is the AWS-managed recovery cred; touching it would desync from the
// secret without operator sign-off.
func TestReconcile_PreservesVendorAdmin(t *testing.T) {
	fake := newFakeIPMITool(t)
	provisioned := `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    true       true       OPERATOR
4   monitor          true    true       true       USER
`
	fake.queueOutput("user list 1", provisioned)
	fake.queueOutput("chassis bootparam set bootflag force_pxe options=PEF,timestamp,reset_persistence", "")
	fake.queueOutput("user list 1", provisioned)

	res, err := Reconcile(context.Background(), Options{
		IPMITool: fake.path,
		Desired: []DesiredUser{
			{Name: "slagworks", Privilege: PrivOperator},
			{Name: "monitor", Privilege: PrivUser},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.LeftoversDisabled) != 0 {
		t.Fatalf("ADMIN must not be disabled; got LeftoversDisabled=%v", res.LeftoversDisabled)
	}
}

// TestReconcile_RefusesToManageReservedSlotByName covers the
// guarded path: if Options.Desired names a user (e.g. "ADMIN") that
// happens to live in a reserved slot, the planner must refuse rather
// than silently rotate that slot's password. The disable-leftover
// path already gates on reservedSlots; without this guard on the
// update path, a Desired entry naming the vendor ADMIN would
// silently re-target slot 2 — which holds the AWS-managed recovery
// cred — and lock us out if the slagworks user later got corrupted.
//
// Production never passes ADMIN in Desired, so this is purely a
// structural guard against future config / typo regressions.
func TestReconcile_RefusesToManageReservedSlotByName(t *testing.T) {
	fake := newFakeIPMITool(t)
	provisioned := `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3                    true    false      false      NO ACCESS
`
	fake.queueOutput("user list 1", provisioned)

	_, err := Reconcile(context.Background(), Options{
		IPMITool: fake.path,
		Desired: []DesiredUser{
			// Misconfigured: trying to manage the slot-2 ADMIN.
			{Name: "ADMIN", Privilege: PrivAdmin},
		},
	})
	if err == nil {
		t.Fatal("expected error when Desired names a reserved-slot user; got nil")
	}
	if !strings.Contains(err.Error(), "reserved slot") {
		t.Errorf("error should mention reserved slot; got %v", err)
	}
}

// TestReconcile_RestoresMissingUser covers the drift-recovery happy
// path: monitor was nuked from the BMC; reconcile re-adds it without
// touching slagworks. This is what "re-run deploy to recover" looks
// like at the package level.
func TestReconcile_RestoresMissingUser(t *testing.T) {
	fake := newFakeIPMITool(t)
	// Pre-state: slagworks present, monitor missing.
	fake.queueOutput("user list 1", `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    true       true       OPERATOR
4                    true    false      false      NO ACCESS
`)
	for _, cmd := range []string{
		"user priv 4 15 1",
		"user set name 4 monitor",
		"user set password 4 *",
		"user priv 4 2 1",
		"channel setaccess 1 4 callin=on ipmi=on link=on privilege=2",
		"user enable 4",
		"chassis bootparam set bootflag force_pxe options=PEF,timestamp,reset_persistence",
	} {
		fake.queueOutput(cmd, "")
	}
	fake.queueOutput("user list 1", `ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
1                    true    false      false      ADMINISTRATOR
2   ADMIN            true    true       true       ADMINISTRATOR
3   slagworks        true    true       true       OPERATOR
4   monitor          true    true       true       USER
`)

	res, err := Reconcile(context.Background(), Options{
		IPMITool: fake.path,
		Desired: []DesiredUser{
			{Name: "slagworks", Privilege: PrivOperator},
			{Name: "monitor", Privilege: PrivUser},
		},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.CreatedOrUpdated) != 1 {
		t.Fatalf("expected only monitor to be added, got %d: %+v",
			len(res.CreatedOrUpdated), res.CreatedOrUpdated)
	}
	if res.CreatedOrUpdated[0].Name != "monitor" {
		t.Errorf("expected monitor, got %s", res.CreatedOrUpdated[0].Name)
	}
}

// userTable renders an ipmitool `user list 1` table from (slot, name,
// priv) rows. priv "" renders NO ACCESS (a disabled slot).
func userTable(rows ...[3]string) string {
	var b strings.Builder
	b.WriteString("ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit\n")
	for _, r := range rows {
		priv := r[2]
		if priv == "" {
			priv = "NO ACCESS"
		}
		name := r[1]
		b.WriteString(r[0] + strings.Repeat(" ", 4-len(r[0])) + name +
			strings.Repeat(" ", 17-len(name)) + "true    true       true       " + priv + "\n")
	}
	return b.String()
}

// TestReconcile_SkipBootOrder covers the remote (LAN) path's boot-order
// opt-out: with SkipBootOrder the persistent force_pxe bootparam write must
// never run (the target may be a production host slagworks doesn't boot).
// Structural assertion: no bootparam response is queued, so the fake fails
// loudly if Reconcile issues one, and the calls log is checked directly.
func TestReconcile_SkipBootOrder(t *testing.T) {
	fake := newFakeIPMITool(t)
	table := userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "monitor", "USER"},
	)
	fake.queueOutput("user list 1", table)
	fake.queueOutput("user list 1", table)

	res, err := Reconcile(context.Background(), Options{
		IPMITool:      fake.path,
		Desired:       []DesiredUser{{Name: "monitor", Privilege: PrivUser}},
		SkipBootOrder: true,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.BootOrderPersisted {
		t.Error("BootOrderPersisted must be false when SkipBootOrder is set")
	}
	for _, c := range fake.calls() {
		if strings.Contains(c.Args, "bootparam") {
			t.Errorf("SkipBootOrder still issued a bootparam call: %q", c.Args)
		}
	}
}

// TestReconcile_AdminUserNotSlot2_RefusesBeforeWrites is the safety gate
// both paths share: AdminUser is the bmc-creds-admin account (the remote
// path's LAN session account; the in-band path learns it from the deploy
// cmdline), and the leftover pass disables every non-reserved account
// outside Desired. If AdminUser is not the slot-2 account, reconcile would
// disable slagworks' only BMC credential. It must refuse after the initial
// list, with no write of any kind, with a typed error naming the account
// and the slot it is actually at, and tell the operator to repoint the
// admin entry.
func TestReconcile_AdminUserNotSlot2_RefusesBeforeWrites(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "root", "ADMINISTRATOR"},
	))

	res, err := Reconcile(context.Background(), Options{
		IPMITool:  fake.path,
		Desired:   []DesiredUser{{Name: "monitor", Privilege: PrivUser}},
		AdminUser: "root",
	})
	if err == nil {
		t.Fatal("expected refusal when the admin account is not at slot 2")
	}
	if res != nil {
		t.Errorf("refusal must return a nil Result (nothing written), got %+v", res)
	}
	var gate *AdminNotSlot2Error
	if !errors.As(err, &gate) {
		t.Fatalf("refusal must be an *AdminNotSlot2Error, got %T: %v", err, err)
	}
	if gate.User != "root" || gate.Slot != 3 || gate.Slot2User != "ADMIN" {
		t.Errorf("gate error = %+v, want user=root slot=3 slot2=ADMIN", gate)
	}
	for _, want := range []string{`"root"`, "slot 3", "slot 2", "bmc-creds-admin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %s", err, want)
		}
	}
	calls := fake.calls()
	if len(calls) != 1 || calls[0].Args != "-I open user list 1" {
		t.Errorf("expected exactly the initial `user list 1`, got %+v", calls)
	}
}

// TestReconcile_AdminUserAbsent_Refuses: the named admin account isn't on
// the BMC at all (a stale admin entry). Still refuse: whatever account
// actually authenticates slagworks is unknown, so nothing may be disabled.
func TestReconcile_AdminUserAbsent_Refuses(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
	))
	_, err := Reconcile(context.Background(), Options{
		IPMITool:  fake.path,
		Desired:   []DesiredUser{{Name: "monitor", Privilege: PrivUser}},
		AdminUser: "root",
	})
	var gate *AdminNotSlot2Error
	if !errors.As(err, &gate) || gate.Slot != 0 {
		t.Fatalf("expected an AdminNotSlot2Error with Slot 0 (absent), got %v", err)
	}
	if !strings.Contains(err.Error(), "not on the BMC") {
		t.Errorf("error should say the account is not on the BMC: %v", err)
	}
	if n := len(fake.calls()); n != 1 {
		t.Errorf("refusal must happen before any write; got %d calls", n)
	}
}

// TestReconcile_AdminUserSlot2_CaseInsensitive: the BMC lists slot 2 as
// "ADMIN" while the admin entry says "admin". Absent a second case-variant
// account (which the next test refuses), it IS the slot-2 account; the gate
// must not refuse on casing alone.
func TestReconcile_AdminUserSlot2_CaseInsensitive(t *testing.T) {
	fake := newFakeIPMITool(t)
	table := userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "monitor", "USER"},
	)
	fake.queueOutput("user list 1", table)
	fake.queueOutput("user list 1", table)
	if _, err := Reconcile(context.Background(), Options{
		IPMITool:      fake.path,
		Desired:       []DesiredUser{{Name: "monitor", Privilege: PrivUser}},
		AdminUser:     "admin",
		SkipBootOrder: true,
	}); err != nil {
		t.Fatalf("slot-2 account differing only in case must pass the gate: %v", err)
	}
}

// TestReconcile_AdminUserAlsoElsewhere_Refuses: slot 2 carries the admin
// account's name but another non-reserved slot carries the same name
// (case-insensitively). Which account the credential names is then
// ambiguous, so the gate refuses rather than risk disabling it.
func TestReconcile_AdminUserAlsoElsewhere_Refuses(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"5", "admin", "ADMINISTRATOR"},
	))
	_, err := Reconcile(context.Background(), Options{
		IPMITool:  fake.path,
		Desired:   []DesiredUser{{Name: "monitor", Privilege: PrivUser}},
		AdminUser: "ADMIN",
	})
	var gate *AdminNotSlot2Error
	if !errors.As(err, &gate) || gate.Slot != 5 || !strings.Contains(err.Error(), "slot 5") {
		t.Fatalf("expected refusal naming the duplicate at slot 5, got %v", err)
	}
	if n := len(fake.calls()); n != 1 {
		t.Errorf("refusal must happen before any write; got %d calls", n)
	}
}

// TestAdminUserArg_RoundTrip: the admin username rides the deploy kernel
// cmdline (and through an iPXE script line), so the encoded token must
// carry no whitespace, quotes, '$' (iPXE settings expansion) or '='-ambiguity
// beyond the key's own, and must decode back exactly.
func TestAdminUserArg_RoundTrip(t *testing.T) {
	for _, name := range []string{
		"ADMIN", "root", "admin user", `ad"min`, "ad'min", "a$b{c}", "50%+x", "tab\tname", "eq=sign",
	} {
		enc := EncodeAdminUserArg(name)
		if strings.ContainsAny(enc, " \t\n\"'$={}") {
			t.Errorf("EncodeAdminUserArg(%q) = %q carries cmdline-unsafe characters", name, enc)
		}
		got, err := DecodeAdminUserArg(enc)
		if err != nil || got != name {
			t.Errorf("round trip %q -> %q -> %q (err %v)", name, enc, got, err)
		}
	}
	if _, err := DecodeAdminUserArg("bad%zzvalue"); err == nil {
		t.Error("a malformed escape must fail to decode")
	}
	if _, err := DecodeAdminUserArg(""); err == nil {
		t.Error("an empty value must fail to decode (the arg is omitted, never empty)")
	}
}

// TestReconcile_Rotate_RotatesExistingMonitor: a Desired user with Rotate
// gets a FRESH password even when the account already exists, is enabled,
// and has the right privilege (the provision-once rule does not apply).
// The generated cred is reported. This is the remote path with no stored
// password, or an explicit --rotate-monitor.
func TestReconcile_Rotate_RotatesExistingMonitor(t *testing.T) {
	fake := newFakeIPMITool(t)
	table := userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"4", "monitor", "USER"},
	)
	fake.queueOutput("user list 1", table)
	for _, cmd := range []string{
		"user set password 4 *",
		"user priv 4 2 1",
		"channel setaccess 1 4 callin=on ipmi=on link=on privilege=2",
		"user enable 4",
	} {
		fake.queueOutput(cmd, "")
	}
	fake.queueOutput("user list 1", table)

	res, err := Reconcile(context.Background(), Options{
		IPMITool:      fake.path,
		Desired:       []DesiredUser{{Name: "monitor", Privilege: PrivUser, Rotate: true}},
		SkipBootOrder: true,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.CreatedOrUpdated) != 1 {
		t.Fatalf("expected the rotated monitor cred, got %+v", res.CreatedOrUpdated)
	}
	u := res.CreatedOrUpdated[0]
	if u.Name != "monitor" || u.Slot != 4 || len(u.Password) != passwordLength {
		t.Errorf("rotated monitor: %+v", u)
	}
	for _, c := range fake.calls() {
		if strings.Contains(c.Args, "set name") {
			t.Errorf("rotating an existing account must not rename it: %q", c.Args)
		}
		// A generated password is new, so it cannot already be on the
		// BMC or in its reuse history: write without testing first.
		if strings.Contains(c.Args, "user test") {
			t.Errorf("a rotation must not test before writing: %q", c.Args)
		}
	}
}

// storedMonitorPW is the bmc-creds-monitor password the stored-password
// tests re-assert.
const storedMonitorPW = "St0red!monPW"

// assertPasswordWrites checks that every `user set password` call wrote
// exactly want to slot.
func assertPasswordWrites(t *testing.T, calls []fakeCall, slot, want string) {
	t.Helper()
	n := 0
	for _, c := range calls {
		if !strings.Contains(c.Args, "user set password") {
			continue
		}
		n++
		if !strings.Contains(c.Args, "user set password "+slot+" "+want) {
			t.Errorf("password write %q, want the stored password on slot %s", c.Args, slot)
		}
	}
	if n == 0 {
		t.Errorf("no `user set password` call; want the stored password written to slot %s", slot)
	}
}

// ipmitool `user test` output (lib/ipmi_user.c ipmi_user_test_password):
// "Success" with exit 0 for completion code 0x00; otherwise exit 1 with
// "Failure: password incorrect" (0x80: right size, wrong password),
// "Failure: wrong password size" (0x81), or "Unknown error" (anything else,
// including no response).
const (
	userTestSuccess   = "Success\n"
	userTestIncorrect = "Failure: password incorrect\n"
	userTestWrongSize = "Failure: wrong password size\n"
	userTestUnknown   = "Unknown error\n"
)

// queueUserTest queues the fake's answer to `user test <slot> <size> <pw>`:
// exit 0 for Success, exit 1 for every failure, as ipmitool does.
func (f *fakeIPMITool) queueUserTest(slot, size, pw, out string) {
	f.t.Helper()
	cmd := "user test " + slot + " " + size + " " + pw
	if out == userTestSuccess {
		f.queueOutput(cmd, out)
		return
	}
	f.queueFailure(cmd, out, 1)
}

// countCalls returns how many recorded calls contain substr.
func countCalls(calls []fakeCall, substr string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c.Args, substr) {
			n++
		}
	}
	return n
}

// monitorAtSlot4 is a user table with the monitor account at slot 4 at the
// given privilege ("" = disabled).
func monitorAtSlot4(priv string) string {
	return userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"4", "monitor", priv},
	)
}

// storedMonitorOptions is a reconcile of the monitor account that
// re-asserts pw, the way the remote path does with a stored password.
func storedMonitorOptions(fake *fakeIPMITool, pw string) Options {
	return Options{
		IPMITool:      fake.path,
		Desired:       []DesiredUser{{Name: "monitor", Privilege: PrivUser, Password: pw}},
		SkipBootOrder: true,
	}
}

// queueFixInPlace queues the priv/channel access/enable writes for slot 4
// at USER, which every stored-password run re-applies.
func queueFixInPlace(fake *fakeIPMITool) {
	for _, cmd := range []string{
		"user priv 4 2 1",
		"channel setaccess 1 4 callin=on ipmi=on link=on privilege=2",
		"user enable 4",
	} {
		fake.queueOutput(cmd, "")
	}
}

// TestReconcile_StoredPassword_Matches_NoWrite: when an existing monitor
// already holds the stored password (`user test` succeeds at 16 or 20
// bytes), reconcile writes no password: MegaRAC refuses to set a password
// to its current value (completion code 0x19), which failed every
// compliance run on cpu3.pop3. It still re-applies priv/channel
// access/enabled, and reports the account as verified, not written. A
// password longer than 16 bytes is never tested at 16: ipmitool truncates
// a test password to the size, so a prefix could falsely match.
func TestReconcile_StoredPassword_Matches_NoWrite(t *testing.T) {
	const longPW = "St0red!monPW-18byt"
	cases := []struct {
		name  string
		pw    string
		tests [][2]string // (size, ipmitool output) in order
	}{
		{"16 bytes", storedMonitorPW, [][2]string{{"16", userTestSuccess}}},
		{"20 bytes", storedMonitorPW, [][2]string{{"16", userTestWrongSize}, {"20", userTestSuccess}}},
		{"longer than 16 bytes", longPW, [][2]string{{"20", userTestSuccess}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeIPMITool(t)
			fake.queueOutput("user list 1", monitorAtSlot4("USER"))
			for _, ut := range tc.tests {
				fake.queueUserTest("4", ut[0], tc.pw, ut[1])
			}
			queueFixInPlace(fake)
			fake.queueOutput("user list 1", monitorAtSlot4("USER"))

			res, err := Reconcile(context.Background(), storedMonitorOptions(fake, tc.pw))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			calls := fake.calls()
			if n := countCalls(calls, "user set password"); n != 0 {
				t.Errorf("the BMC already holds the stored password; got %d `user set password` calls", n)
			}
			if n := countCalls(calls, "user test"); n != len(tc.tests) {
				t.Errorf("got %d `user test` calls, want %d", n, len(tc.tests))
			}
			if len(tc.pw) > 16 && countCalls(calls, "user test 4 16") != 0 {
				t.Error("a password longer than 16 bytes must not be tested at 16 (ipmitool truncates it)")
			}
			if len(res.CreatedOrUpdated) != 0 {
				t.Errorf("nothing was written, but CreatedOrUpdated = %+v", res.CreatedOrUpdated)
			}
			if len(res.PasswordVerified) != 1 || res.PasswordVerified[0].Slot != 4 || res.PasswordVerified[0].Name != "monitor" ||
				res.PasswordVerified[0].Password != "" {
				t.Errorf("PasswordVerified = %+v, want monitor at slot 4 with no password", res.PasswordVerified)
			}
		})
	}
}

// TestReconcile_StoredPassword_NoMatch_WritesAndConfirms: when the BMC does
// not hold the stored password (password changed by hand), reconcile writes
// it and tests again to confirm, then re-applies priv/channel
// access/enabled. "password incorrect" at 16 bytes is decisive (the BMC
// holds a 16-byte password that is not ours), so 20 is not tested then.
func TestReconcile_StoredPassword_NoMatch_WritesAndConfirms(t *testing.T) {
	cases := []struct {
		name  string
		tests [][2]string
	}{
		{"wrong size at both sizes", [][2]string{{"16", userTestWrongSize}, {"20", userTestWrongSize}}},
		{"incorrect at 16", [][2]string{{"16", userTestIncorrect}}},
		{"incorrect at 20", [][2]string{{"16", userTestWrongSize}, {"20", userTestIncorrect}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeIPMITool(t)
			fake.queueOutput("user list 1", monitorAtSlot4("USER"))
			for _, ut := range tc.tests {
				fake.queueUserTest("4", ut[0], storedMonitorPW, ut[1])
			}
			fake.queueOutput("user set password 4 "+storedMonitorPW, "")
			fake.queueUserTest("4", "16", storedMonitorPW, userTestSuccess)
			queueFixInPlace(fake)
			fake.queueOutput("user list 1", monitorAtSlot4("USER"))

			res, err := Reconcile(context.Background(), storedMonitorOptions(fake, storedMonitorPW))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			calls := fake.calls()
			assertPasswordWrites(t, calls, "4", storedMonitorPW)
			if n := countCalls(calls, "user test"); n != len(tc.tests)+1 {
				t.Errorf("got %d `user test` calls, want %d before the write and 1 to confirm", n, len(tc.tests))
			}
			if len(res.CreatedOrUpdated) != 1 || res.CreatedOrUpdated[0].Slot != 4 || res.CreatedOrUpdated[0].Password != "" {
				t.Errorf("CreatedOrUpdated = %+v, want monitor written at slot 4 with no generated password", res.CreatedOrUpdated)
			}
			if len(res.PasswordVerified) != 0 {
				t.Errorf("a written password is not merely verified: %+v", res.PasswordVerified)
			}
		})
	}
}

// TestReconcile_StoredPassword_WriteRefused: the BMC does not hold the
// stored password and refuses to set it (MegaRAC's 0x19, e.g. a password in
// its reuse history). Reconcile fails with an error that says the store's
// password is not on the BMC and names the fix, without the password.
func TestReconcile_StoredPassword_WriteRefused(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", monitorAtSlot4("USER"))
	fake.queueUserTest("4", "16", storedMonitorPW, userTestWrongSize)
	fake.queueUserTest("4", "20", storedMonitorPW, userTestWrongSize)
	for range 3 {
		fake.queueFailure("user set password 4 "+storedMonitorPW, megaRACRefusal("4"), 1)
		// Each refusal is followed by a test: the slot still does not hold it.
		fake.queueUserTest("4", "16", storedMonitorPW, userTestWrongSize)
		fake.queueUserTest("4", "20", storedMonitorPW, userTestWrongSize)
	}

	res, err := Reconcile(context.Background(), storedMonitorOptions(fake, storedMonitorPW))
	if err == nil {
		t.Fatal("expected the refused write to fail the reconcile")
	}
	for _, want := range []string{"does not hold", "0x19", "--rotate-monitor"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), storedMonitorPW) {
		t.Errorf("error leaks the stored password: %v", err)
	}
	if res == nil || len(res.CreatedOrUpdated) != 0 || len(res.PasswordVerified) != 0 {
		t.Errorf("nothing was written or verified; got %+v", res)
	}
}

// TestReconcile_StoredPassword_WriteNotConfirmed: the write succeeded but a
// second test still does not match. The account is reported written (the
// write landed) and reconcile fails rather than claim compliance.
func TestReconcile_StoredPassword_WriteNotConfirmed(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", monitorAtSlot4("USER"))
	fake.queueUserTest("4", "16", storedMonitorPW, userTestIncorrect)
	fake.queueOutput("user set password 4 "+storedMonitorPW, "")
	fake.queueUserTest("4", "16", storedMonitorPW, userTestIncorrect)

	res, err := Reconcile(context.Background(), storedMonitorOptions(fake, storedMonitorPW))
	if err == nil || !strings.Contains(err.Error(), "confirm") {
		t.Fatalf("expected an unconfirmed-write error, got %v", err)
	}
	if strings.Contains(err.Error(), storedMonitorPW) {
		t.Errorf("error leaks the stored password: %v", err)
	}
	if res == nil || len(res.CreatedOrUpdated) != 1 || res.CreatedOrUpdated[0].Slot != 4 {
		t.Errorf("the write landed, so the account is reported written; got %+v", res)
	}
}

// TestReconcile_StoredPassword_TestInconclusive_NoWrite: when `user test`
// fails for a reason other than a mismatch (no session, "Unknown error"),
// reconcile cannot tell whether the BMC holds the password, so it fails
// before writing rather than write blind into a refusal.
func TestReconcile_StoredPassword_TestInconclusive_NoWrite(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", monitorAtSlot4("USER"))
	fake.queueUserTest("4", "16", storedMonitorPW, userTestUnknown)
	fake.queueUserTest("4", "20", storedMonitorPW, "Error: Unable to establish IPMI v2 / RMCP+ session\n")

	res, err := Reconcile(context.Background(), storedMonitorOptions(fake, storedMonitorPW))
	if err == nil || !strings.Contains(err.Error(), "could not test") {
		t.Fatalf("expected an inconclusive-test error, got %v", err)
	}
	if strings.Contains(err.Error(), storedMonitorPW) {
		t.Errorf("error leaks the stored password: %v", err)
	}
	if n := countCalls(fake.calls(), "user set password"); n != 0 {
		t.Errorf("an inconclusive test must not lead to a write; got %d", n)
	}
	if res == nil || len(res.CreatedOrUpdated) != 0 || len(res.PasswordVerified) != 0 {
		t.Errorf("nothing was written or verified; got %+v", res)
	}
}

// TestReconcile_StoredPassword_CreatesAbsentMonitor: with the stored
// password set and no monitor on the BMC (slot cleared by hand), the
// account is created with the stored password, not a generated one.
func TestReconcile_StoredPassword_CreatesAbsentMonitor(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "", ""},
	))
	for _, cmd := range []string{
		"user priv 3 15 1",
		"user set name 3 monitor",
		"user set password 3 " + storedMonitorPW,
		"user priv 3 2 1",
		"channel setaccess 1 3 callin=on ipmi=on link=on privilege=2",
		"user enable 3",
	} {
		fake.queueOutput(cmd, "")
	}
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "monitor", "USER"},
	))

	res, err := Reconcile(context.Background(), Options{
		IPMITool:      fake.path,
		Desired:       []DesiredUser{{Name: "monitor", Privilege: PrivUser, Password: storedMonitorPW}},
		SkipBootOrder: true,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.CreatedOrUpdated) != 1 || res.CreatedOrUpdated[0].Slot != 3 || res.CreatedOrUpdated[0].Password != "" {
		t.Fatalf("expected monitor created at slot 3 with no generated password, got %+v", res.CreatedOrUpdated)
	}
	assertPasswordWrites(t, fake.calls(), "3", storedMonitorPW)
}

// TestReconcile_StoredPassword_DriftedMatching_FixedInPlace: a drifted
// monitor (disabled, or enabled at the wrong privilege) that still holds
// the stored password is fixed in place with no password write: priv,
// channel access and enabled re-applied. A leftover account is still
// disabled.
func TestReconcile_StoredPassword_DriftedMatching_FixedInPlace(t *testing.T) {
	for name, monitorPriv := range map[string]string{
		"disabled":   "",
		"wrong priv": "OPERATOR",
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeIPMITool(t)
			fake.queueOutput("user list 1", userTable(
				[3]string{"1", "", "ADMINISTRATOR"},
				[3]string{"2", "ADMIN", "ADMINISTRATOR"},
				[3]string{"4", "monitor", monitorPriv},
				[3]string{"5", "maas", "ADMINISTRATOR"},
			))
			fake.queueUserTest("4", "16", storedMonitorPW, userTestSuccess)
			queueFixInPlace(fake)
			fake.queueOutput("user disable 5", "")
			fake.queueOutput("user set name 5 ", "")
			fake.queueOutput("user list 1", userTable(
				[3]string{"1", "", "ADMINISTRATOR"},
				[3]string{"2", "ADMIN", "ADMINISTRATOR"},
				[3]string{"4", "monitor", "USER"},
				[3]string{"5", "", ""},
			))

			res, err := Reconcile(context.Background(), storedMonitorOptions(fake, storedMonitorPW))
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			calls := fake.calls()
			if n := countCalls(calls, "user set password"); n != 0 {
				t.Errorf("the monitor holds the stored password; got %d password writes", n)
			}
			for _, want := range []string{"user priv 4 2 1", "user enable 4"} {
				if countCalls(calls, want) != 1 {
					t.Errorf("drift not fixed in place: no %q", want)
				}
			}
			if len(res.PasswordVerified) != 1 || res.PasswordVerified[0].Slot != 4 || len(res.CreatedOrUpdated) != 0 {
				t.Errorf("want monitor verified at slot 4, nothing written; got verified=%+v written=%+v",
					res.PasswordVerified, res.CreatedOrUpdated)
			}
			if !slices.Equal(res.LeftoversDisabled, []string{"maas"}) {
				t.Errorf("LeftoversDisabled = %v, want [maas]", res.LeftoversDisabled)
			}
		})
	}
}

// setPasswordNoResponse is ipmitool's output for a `user set password` that
// got no response (a lost lanplus reply): no completion code, exit 1.
func setPasswordNoResponse(slot string) string {
	return "Set User Password command failed (user " + slot + ")\n"
}

// megaRACRefusal is AMI MegaRAC's answer to `user set password` with the
// slot's current password, or one in its reuse history: completion code
// 0x19, exit 1.
func megaRACRefusal(slot string) string {
	return "IPMI command failed: Unknown (0x19)\nSet User Password command failed (user " + slot + ")\n"
}

// writtenPasswords returns the password argument of every `user set
// password` call, and testedPasswords that of every `user test` call.
func writtenPasswords(calls []fakeCall) []string {
	var out []string
	for _, c := range calls {
		f := strings.Fields(c.Args)
		if i := slices.Index(f, "password"); i >= 2 && f[i-2] == "user" && f[i-1] == "set" && i+2 < len(f) {
			out = append(out, f[i+2])
		}
	}
	return out
}

func testedPasswords(calls []fakeCall) []string {
	var out []string
	for _, c := range calls {
		f := strings.Fields(c.Args)
		if i := slices.Index(f, "test"); i >= 1 && f[i-1] == "user" && i+3 < len(f) {
			out = append(out, f[i+3])
		}
	}
	return out
}

// TestReconcile_Rotate_LostWriteResponse_ReportsGeneratedPassword: a
// --rotate-monitor run on a MegaRAC BMC whose first `user set password`
// is applied but whose response is lost, so ipmitool exits 1. Retrying the
// write would send the BMC its current password, which MegaRAC refuses
// with 0x19; the run would then fail without reporting the generated
// password the BMC now holds, and global would keep the old one (monitor
// locked out). Instead the failed write is followed by `user test`, which
// finds the generated password on the BMC: no retry, and the password is
// reported for global to store.
func TestReconcile_Rotate_LostWriteResponse_ReportsGeneratedPassword(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", monitorAtSlot4("USER"))
	fake.queueFailure("user set password 4 *", setPasswordNoResponse("4"), 1)
	// What a retry would get: the generated password is now current.
	for range 2 {
		fake.queueFailure("user set password 4 *", megaRACRefusal("4"), 1)
	}
	fake.queueOutput("user test 4 16 *", userTestSuccess)
	queueFixInPlace(fake)
	fake.queueOutput("user list 1", monitorAtSlot4("USER"))

	opt := RemoteOptions(LANTarget{Host: "192.0.2.10", Username: "ADMIN", Password: "adminpw!"}, "") // --rotate-monitor
	opt.IPMITool = fake.path
	res, err := Reconcile(context.Background(), opt)
	if err != nil {
		var reported []ProvisionedUser
		if res != nil {
			reported = res.CreatedOrUpdated
		}
		t.Fatalf("the BMC applied the generated password but Reconcile failed, reporting %+v: %v", reported, err)
	}
	calls := fake.calls()
	written := writtenPasswords(calls)
	if len(written) != 1 {
		t.Fatalf("got %d `user set password` calls, want 1: a write the BMC holds must not be retried", len(written))
	}
	if tested := testedPasswords(calls); !slices.Equal(tested, written) {
		t.Errorf("after the failed write, `user test` checked %d password(s), want the written one", len(tested))
	}
	if len(res.CreatedOrUpdated) != 1 || res.CreatedOrUpdated[0].Slot != 4 || res.CreatedOrUpdated[0].Password != written[0] {
		t.Errorf("CreatedOrUpdated = %+v, want monitor at slot 4 carrying the generated password the BMC holds", res.CreatedOrUpdated)
	}
}

// TestReconcile_StoredPassword_LostWriteResponse_Confirmed: the re-assert
// path writes the stored password after `user test` finds a different one.
// When that write is applied but its response is lost, the next test finds
// the stored password on the BMC, so the run succeeds without a retry
// (which MegaRAC would refuse with 0x19) instead of failing with "does not
// hold the stored password".
func TestReconcile_StoredPassword_LostWriteResponse_Confirmed(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", monitorAtSlot4("USER"))
	fake.queueUserTest("4", "16", storedMonitorPW, userTestIncorrect)
	fake.queueFailure("user set password 4 "+storedMonitorPW, setPasswordNoResponse("4"), 1)
	for range 2 {
		fake.queueFailure("user set password 4 "+storedMonitorPW, megaRACRefusal("4"), 1)
	}
	fake.queueUserTest("4", "16", storedMonitorPW, userTestSuccess) // after the failed write
	fake.queueUserTest("4", "16", storedMonitorPW, userTestSuccess) // re-assert's confirmation
	queueFixInPlace(fake)
	fake.queueOutput("user list 1", monitorAtSlot4("USER"))

	res, err := Reconcile(context.Background(), storedMonitorOptions(fake, storedMonitorPW))
	if err != nil {
		t.Fatalf("the BMC holds the stored password after the write, but Reconcile failed: %v", err)
	}
	if n := countCalls(fake.calls(), "user set password"); n != 1 {
		t.Errorf("got %d `user set password` calls, want 1: a write the BMC holds must not be retried", n)
	}
	if len(res.CreatedOrUpdated) != 1 || res.CreatedOrUpdated[0].Slot != 4 || res.CreatedOrUpdated[0].Password != "" {
		t.Errorf("CreatedOrUpdated = %+v, want monitor written at slot 4 with no generated password", res.CreatedOrUpdated)
	}
}

// TestReconcile_StoredPassword_CreateIntoSlotAlreadyHoldingIt: creating the
// monitor writes the stored password to a free slot without testing first.
// When that slot already holds it (an un-named slot that kept a previous
// monitor's password), MegaRAC refuses the write with 0x19. The test after
// the refusal finds the password in place, so the account is created
// rather than the run failing on a write that had nothing to change. The
// create drops the slot to NO ACCESS before naming it, as every create does;
// that leaves the password alone, so the test still finds it.
func TestReconcile_StoredPassword_CreateIntoSlotAlreadyHoldingIt(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "", ""},
	))
	fake.queueOutput("user priv 3 15 1", "")
	fake.queueOutput("user set name 3 monitor", "")
	for range 3 {
		fake.queueFailure("user set password 3 "+storedMonitorPW, megaRACRefusal("3"), 1)
	}
	fake.queueUserTest("3", "16", storedMonitorPW, userTestSuccess)
	for _, cmd := range []string{
		"user priv 3 2 1",
		"channel setaccess 1 3 callin=on ipmi=on link=on privilege=2",
		"user enable 3",
	} {
		fake.queueOutput(cmd, "")
	}
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "monitor", "USER"},
	))

	res, err := Reconcile(context.Background(), storedMonitorOptions(fake, storedMonitorPW))
	if err != nil {
		t.Fatalf("slot 3 already holds the stored password, but Reconcile failed: %v", err)
	}
	if n := countCalls(fake.calls(), "user set password"); n != 1 {
		t.Errorf("got %d `user set password` calls, want 1: a password the slot holds must not be retried", n)
	}
	if len(res.CreatedOrUpdated) != 1 || res.CreatedOrUpdated[0].Slot != 3 || res.CreatedOrUpdated[0].Password != "" {
		t.Errorf("CreatedOrUpdated = %+v, want monitor created at slot 3 with no generated password", res.CreatedOrUpdated)
	}
}

// TestReconcile_PasswordAndRotate_Refused: a Desired user carrying both a
// password to write and Rotate is contradictory; refuse before any
// ipmitool call rather than pick one.
func TestReconcile_PasswordAndRotate_Refused(t *testing.T) {
	fake := newFakeIPMITool(t)
	res, err := Reconcile(context.Background(), Options{
		IPMITool: fake.path,
		Desired:  []DesiredUser{{Name: "monitor", Privilege: PrivUser, Password: storedMonitorPW, Rotate: true}},
	})
	if err == nil || res != nil {
		t.Fatalf("expected a refusal with no result, got res=%+v err=%v", res, err)
	}
	if strings.Contains(err.Error(), storedMonitorPW) {
		t.Errorf("refusal leaks the password: %v", err)
	}
	if n := len(fake.calls()); n != 0 {
		t.Errorf("refusal must precede any ipmitool call; got %d calls", n)
	}
}

// TestReconcile_PasswordTooLong_Refused: an IPMI password is at most 20
// bytes. A longer Desired password can be neither tested (ipmitool
// truncates it) nor written (ipmitool refuses it), so reconcile refuses
// before any ipmitool call instead of claiming the BMC does not hold it.
func TestReconcile_PasswordTooLong_Refused(t *testing.T) {
	const tooLong = "St0red!monPW-21bytes!"
	fake := newFakeIPMITool(t)
	res, err := Reconcile(context.Background(), storedMonitorOptions(fake, tooLong))
	if err == nil || res != nil {
		t.Fatalf("expected a refusal with no result, got res=%+v err=%v", res, err)
	}
	if !strings.Contains(err.Error(), "20 bytes") {
		t.Errorf("refusal should name the 20-byte limit: %v", err)
	}
	if strings.Contains(err.Error(), tooLong) {
		t.Errorf("refusal leaks the password: %v", err)
	}
	if n := len(fake.calls()); n != 0 {
		t.Errorf("refusal must precede any ipmitool call; got %d calls", n)
	}
}

// TestReconcile_ErrorRedactsOptionSecrets: a failure on any command (here
// the priv write after the stored password was verified) must not carry
// the stored monitor password or the LAN session's admin password, even
// when the BMC's output echoes them. The error text becomes the op's
// failure reason and a log line.
func TestReconcile_ErrorRedactsOptionSecrets(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"4", "monitor", "USER"},
	))
	fake.queueUserTest("4", "16", storedMonitorPW, userTestSuccess)
	fake.queueFailure("user priv 4", "echo: "+storedMonitorPW+" adminpw!", 1)

	opt := RemoteOptions(LANTarget{Host: "10.0.0.9", Username: "ADMIN", Password: "adminpw!"}, storedMonitorPW)
	opt.IPMITool = fake.path
	res, err := Reconcile(context.Background(), opt)
	if err == nil {
		t.Fatal("expected the priv failure to surface")
	}
	if res == nil {
		t.Fatal("a failure after the password write must return the partial Result")
	}
	for _, secret := range []string{storedMonitorPW, "adminpw!"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks %q: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("error should show where secrets were redacted: %v", err)
	}
	for _, u := range append(res.CreatedOrUpdated, res.PasswordVerified...) {
		if u.Password != "" {
			t.Errorf("a re-asserted stored password must not be copied into the Result: %+v", u)
		}
	}
}

// TestRunIPMITool_RedactsUserTestPassword: the password argument of
// `user test <slot> <16|20> <password>` is redacted from a failure's error
// text like `user set password`'s, whether or not it is a Desired password.
func TestRunIPMITool_RedactsUserTestPassword(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueUserTest("4", "20", storedMonitorPW, "Failure: wrong password size for "+storedMonitorPW+"\n")
	_, err := runIPMITool(context.Background(), Options{IPMITool: fake.path, IPMIInterface: "open"},
		"user", "test", "4", "20", storedMonitorPW)
	if err == nil {
		t.Fatal("expected the failed test to surface")
	}
	if strings.Contains(err.Error(), storedMonitorPW) {
		t.Errorf("error leaks the tested password: %v", err)
	}
	if !strings.Contains(err.Error(), "user test 4 20 <redacted>") {
		t.Errorf("error should show the command with the password redacted: %v", err)
	}
}

// TestReconcile_DuplicateDesiredName_ExtraDisabled: compliance is EXACTLY
// one monitor account. A second account carrying the desired name (here
// slot 6) would otherwise be skipped by the leftover pass as "desired" and
// survive with an unknown password. The first (lowest-slot) one is managed;
// the duplicate is disabled and its name cleared like any other leftover.
func TestReconcile_DuplicateDesiredName_ExtraDisabled(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "monitor", "USER"},
		[3]string{"6", "monitor", "USER"},
	))
	for _, cmd := range []string{
		"user disable 6",
		"user set name 6 ",
	} {
		fake.queueOutput(cmd, "")
	}
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "monitor", "USER"},
		[3]string{"6", "", ""},
	))

	res, err := Reconcile(context.Background(), Options{
		IPMITool:      fake.path,
		Desired:       []DesiredUser{{Name: "monitor", Privilege: PrivUser}},
		SkipBootOrder: true,
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(res.LeftoversDisabled, []string{"monitor"}) {
		t.Errorf("LeftoversDisabled: got %v, want [monitor] (the slot-6 duplicate)", res.LeftoversDisabled)
	}
	for _, c := range fake.calls() {
		if strings.Contains(c.Args, "disable 3") {
			t.Errorf("the managed (first) monitor at slot 3 must not be disabled: %q", c.Args)
		}
	}
}

// TestRemoteOptions pins the LAN invocation shape the site worker uses for
// the bmc_reconcile op: lanplus to the BMC host as the admin account, cipher
// suite 17 unless the admin entry overrides it, and the admin password via
// -E / IPMI_PASSWORD — never on argv, where it would show in the process
// table and in any error text that echoes the command line. The monitor
// account carries the stored password to re-assert, or, with none, Rotate.
func TestRemoteOptions(t *testing.T) {
	opt := RemoteOptions(LANTarget{Host: "10.252.240.41", Username: "ADMIN", Password: "s3cret!pw"}, "")
	if opt.IPMIInterface != "lanplus" {
		t.Errorf("IPMIInterface = %q, want lanplus", opt.IPMIInterface)
	}
	wantArgs := []string{"-H", "10.252.240.41", "-U", "ADMIN", "-C", "17", "-E"}
	if !slices.Equal(opt.Args, wantArgs) {
		t.Errorf("Args = %v, want %v", opt.Args, wantArgs)
	}
	for _, a := range opt.Args {
		if strings.Contains(a, "s3cret!pw") {
			t.Fatalf("admin password leaked onto argv: %v", opt.Args)
		}
	}
	if !slices.Equal(opt.Env, []string{"IPMI_PASSWORD=s3cret!pw"}) {
		t.Errorf("Env = %v, want the password in IPMI_PASSWORD", opt.Env)
	}
	if opt.AdminUser != "ADMIN" || !opt.SkipBootOrder {
		t.Errorf("remote safety knobs: AdminUser=%q SkipBootOrder=%v", opt.AdminUser, opt.SkipBootOrder)
	}
	// No stored password (a new BMC, or an explicit rotation): rotate.
	if !slices.Equal(opt.Desired, []DesiredUser{{Name: MonitorUser, Privilege: PrivUser, Rotate: true}}) {
		t.Errorf("Desired = %+v, want only monitor at USER, rotated", opt.Desired)
	}

	// A stored password: re-assert it, never rotate.
	stored := RemoteOptions(LANTarget{Host: "10.252.240.41", Username: "ADMIN", Password: "s3cret!pw"}, storedMonitorPW)
	if !slices.Equal(stored.Desired, []DesiredUser{{Name: MonitorUser, Privilege: PrivUser, Password: storedMonitorPW}}) {
		t.Errorf("Desired = %+v, want only monitor at USER with the stored password", stored.Desired)
	}
	for _, a := range stored.Args {
		if strings.Contains(a, storedMonitorPW) {
			t.Fatalf("stored monitor password leaked onto the session args: %v", stored.Args)
		}
	}

	custom := RemoteOptions(LANTarget{Host: "h", Username: "u", Password: "p", Cipher: 3}, "")
	if i := slices.Index(custom.Args, "-C"); i < 0 || custom.Args[i+1] != "3" {
		t.Errorf("a configured cipher must override the default: %v", custom.Args)
	}
}

// TestReconcile_Remote_EndToEnd drives a full LAN reconcile through the
// fake: a host carrying the legacy `slagworks` account and no monitor.
// Expect: monitor created at USER with a fresh password, slagworks disabled
// and cleared, no boot-order write, and on EVERY ipmitool call the admin
// password travels only in IPMI_PASSWORD (never argv).
func TestReconcile_Remote_EndToEnd(t *testing.T) {
	fake := newFakeIPMITool(t)
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "slagworks", "ADMINISTRATOR"},
		[3]string{"4", "", ""},
	))
	for _, cmd := range []string{
		"user priv 4 15 1",
		"user set name 4 monitor",
		"user set password 4 *",
		"user priv 4 2 1",
		"channel setaccess 1 4 callin=on ipmi=on link=on privilege=2",
		"user enable 4",
		"user disable 3",
		"user set name 3 ",
	} {
		fake.queueOutput(cmd, "")
	}
	fake.queueOutput("user list 1", userTable(
		[3]string{"1", "", "ADMINISTRATOR"},
		[3]string{"2", "ADMIN", "ADMINISTRATOR"},
		[3]string{"3", "", ""},
		[3]string{"4", "monitor", "USER"},
	))

	opt := RemoteOptions(LANTarget{Host: "10.0.0.9", Username: "ADMIN", Password: "adminpw!"}, "")
	opt.IPMITool = fake.path
	res, err := Reconcile(context.Background(), opt)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.CreatedOrUpdated) != 1 || res.CreatedOrUpdated[0].Name != "monitor" || res.CreatedOrUpdated[0].Slot != 4 ||
		len(res.CreatedOrUpdated[0].Password) != passwordLength {
		t.Fatalf("expected monitor provisioned at slot 4 with a generated password, got %+v", res.CreatedOrUpdated)
	}
	if !slices.Equal(res.LeftoversDisabled, []string{"slagworks"}) {
		t.Errorf("LeftoversDisabled = %v, want [slagworks]", res.LeftoversDisabled)
	}
	if res.BootOrderPersisted {
		t.Error("remote reconcile must not write boot order")
	}
	calls := fake.calls()
	if len(calls) == 0 {
		t.Fatal("no ipmitool calls recorded")
	}
	for _, c := range calls {
		if !strings.HasPrefix(c.Args, "-I lanplus -H 10.0.0.9 -U ADMIN -C 17 -E ") {
			t.Errorf("call not shaped as a lanplus session: %q", c.Args)
		}
		if strings.Contains(c.Args, "adminpw!") {
			t.Errorf("admin password on argv: %q", c.Args)
		}
		if c.Env != "adminpw!" {
			t.Errorf("IPMI_PASSWORD = %q on call %q, want the admin password", c.Env, c.Args)
		}
		if strings.Contains(c.Args, "bootparam") {
			t.Errorf("remote reconcile issued a boot-order write: %q", c.Args)
		}
	}
}

// TestSetPassword_ErrorRedactsPassword: when every `user set password`
// attempt fails (and the `user test` after each shows the slot does not
// hold the password), the returned error (which becomes an op result and a
// log line) must not carry the password, even though ipmitool's command
// line and output would otherwise echo it.
func TestSetPassword_ErrorRedactsPassword(t *testing.T) {
	fake := newFakeIPMITool(t)
	for range 3 {
		fake.queueFailure("user set password 4 *", "rejected "+storedMonitorPW, 1)
		fake.queueUserTest("4", "16", storedMonitorPW, userTestIncorrect)
	}
	err := setPassword(context.Background(), Options{
		IPMITool: fake.path, IPMIInterface: "open",
	}, 4, storedMonitorPW)
	if err == nil {
		t.Fatal("expected the set-password failure to surface")
	}
	calls := fake.calls()
	if n := countCalls(calls, "user set password"); n != 3 {
		t.Fatalf("expected 3 set-password attempts, got %d", n)
	}
	if n := countCalls(calls, "user test 4 16"); n != 3 {
		t.Errorf("expected a `user test` after each failed attempt, got %d", n)
	}
	if strings.Contains(err.Error(), storedMonitorPW) {
		t.Errorf("error leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Errorf("error should show where the password was redacted: %v", err)
	}
}

// TestRedact_LongestFirst: when one secret contains another, the longer one
// is redacted whole rather than leaving its tail after the shorter match.
func TestRedact_LongestFirst(t *testing.T) {
	if got := redact("x abc123 y abc", []string{"abc", "abc123"}); got != "x <redacted> y <redacted>" {
		t.Errorf("redact = %q", got)
	}
}

// TestGeneratePassword_Length covers the password generator: 12 chars,
// 4-class alphabet (upper, lower, digits, special).
func TestGeneratePassword_Length(t *testing.T) {
	p, err := generatePassword(passwordLength)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != passwordLength {
		t.Errorf("len: got %d, want %d", len(p), passwordLength)
	}
	const allowed = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#$%^&*"
	for _, c := range p {
		if !strings.ContainsRune(allowed, c) {
			t.Errorf("password char %q outside expected alphabet: %q", c, p)
		}
	}
}

// TestGeneratePassword_Complexity asserts every generated password
// contains at least one of each character class (upper, lower, digit,
// special). This is the load-bearing property — AMI MegaRAC firmware
// rejects any password missing a class with completion code 0xC8
// (which it confusingly also uses for actual length issues). Bug
// motivating this: kworker-l34-1.pop1 first deploy hit "Request
// data field length exceeded" on every password set attempt with
// the old uppercase+digits-only alphabet.
func TestGeneratePassword_Complexity(t *testing.T) {
	const (
		upper   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		lower   = "abcdefghijklmnopqrstuvwxyz"
		digits  = "0123456789"
		special = "!@#$%^&*"
	)
	// Hammer it 1000x so a low-probability reroll path is exercised.
	for i := range 1000 {
		p, err := generatePassword(passwordLength)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.ContainsAny(p, upper) {
			t.Errorf("iter %d: missing uppercase: %q", i, p)
		}
		if !strings.ContainsAny(p, lower) {
			t.Errorf("iter %d: missing lowercase: %q", i, p)
		}
		if !strings.ContainsAny(p, digits) {
			t.Errorf("iter %d: missing digit: %q", i, p)
		}
		if !strings.ContainsAny(p, special) {
			t.Errorf("iter %d: missing special: %q", i, p)
		}
	}
}

// TestGeneratePassword_Distribution sanity-checks rejection sampling
// produces a roughly uniform distribution over the 4-class alphabet.
// Doesn't rigorously test uniformity (chi-squared territory); just
// confirms every alphabet character appears at least once across
// 10K samples — would be overwhelmingly likely if unbiased,
// impossible if some characters are unreachable.
func TestGeneratePassword_Distribution(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#$%^&*"
	seen := map[byte]bool{}
	for i := 0; i < 10000 && len(seen) < len(alphabet); i++ {
		p, err := generatePassword(passwordLength)
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < len(p); j++ {
			seen[p[j]] = true
		}
	}
	for i := range len(alphabet) {
		if !seen[alphabet[i]] {
			t.Errorf("character %q never appeared across 10000 samples", alphabet[i])
		}
	}
}

// TestGeneratePassword_TooShortForComplexity covers the failure mode
// where length is so short that a 4-class password is impossible to
// produce reliably — n=3 can't satisfy "at least one of each of 4
// classes." The retry loop should give up after 20 attempts.
func TestGeneratePassword_TooShortForComplexity(t *testing.T) {
	_, err := generatePassword(3)
	if err == nil {
		t.Fatal("expected error on length=3 (cannot satisfy 4-class requirement)")
	}
}

// fakeIPMITool is a per-test scripted ipmitool replacement. Every
// invocation matches against a queued (command, output) pair and
// returns the canned stdout. Failure-injection works the same way
// (queueFailure sets a non-zero exit code for the matched call).
// Every invocation is also appended to a calls log (argv plus the
// IPMI_PASSWORD environment variable) so tests can assert exactly
// which commands ran and how the session password was passed.
//
// Implementation: write a small Bash script that picks responses
// from a JSON queue file. Cheaper than re-implementing exec.Cmd
// fakes inside the package.
type fakeIPMITool struct {
	t        *testing.T
	dir      string
	path     string
	queueRaw string // JSON path for live updates
	callsLog string // one line per invocation: argv TAB IPMI_PASSWORD TAB TZ
}

type scriptedCall struct {
	Match  string `json:"match"` // substring; matched against the joined command line
	Stdout string `json:"stdout"`
	Exit   int    `json:"exit"`
}

// fakeCall is one recorded ipmitool invocation.
type fakeCall struct {
	Args string // the joined argv after the binary name
	Env  string // the IPMI_PASSWORD environment variable ("" when unset)
	TZ   string // the TZ environment variable ("" when unset)
}

func newFakeIPMITool(t *testing.T) *fakeIPMITool {
	t.Helper()
	dir := t.TempDir()
	queue := filepath.Join(dir, "queue.json")
	calls := filepath.Join(dir, "calls.log")
	binPath := filepath.Join(dir, "ipmitool")

	// The fake logs its argv + IPMI_PASSWORD + TZ, then reads the queue file,
	// finds the first entry whose Match substring appears in its argv,
	// prints Stdout, exits Exit. On no match: emit a clear error and exit
	// 1 so the test fails loudly.
	script := `#!/usr/bin/env bash
set -u
QUEUE="` + queue + `"
ARGS="$*"
printf '%s\t%s\t%s\n' "$ARGS" "${IPMI_PASSWORD-}" "${TZ-}" >> "` + calls + `"
python3 - "$QUEUE" "$ARGS" <<'PY'
import json, sys
queue_path, args = sys.argv[1], sys.argv[2]
with open(queue_path) as f:
    q = json.load(f)
for i, entry in enumerate(q):
    if entry["match"] in args:
        del q[i]
        with open(queue_path, "w") as f:
            json.dump(q, f)
        sys.stdout.write(entry["stdout"])
        sys.exit(entry.get("exit", 0))
sys.stderr.write(f"fake-ipmitool: no queued response for: {args}\n")
sys.stderr.write(f"queue had: {[e['match'] for e in q]}\n")
sys.exit(1)
PY
`
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake ipmitool: %v", err)
	}
	if err := os.WriteFile(queue, []byte("[]"), 0644); err != nil {
		t.Fatalf("write queue: %v", err)
	}

	return &fakeIPMITool{t: t, dir: dir, path: binPath, queueRaw: queue, callsLog: calls}
}

// queueOutput appends a (match, stdout) pair to the fake's response
// queue. Match is a substring; we match against the joined command
// line. "*" in match is treated as any-substring — used so the test
// doesn't need to spell out generated password values.
func (f *fakeIPMITool) queueOutput(matchPattern, stdout string) {
	f.t.Helper()
	f.queue(matchPattern, stdout, 0)
}

// queueFailure is queueOutput for a call that exits non-zero.
func (f *fakeIPMITool) queueFailure(matchPattern, stdout string, exit int) {
	f.t.Helper()
	f.queue(matchPattern, stdout, exit)
}

func (f *fakeIPMITool) queue(matchPattern, stdout string, exit int) {
	f.t.Helper()
	// The "*" wildcard maps to "" — match against the prefix only.
	match := matchPattern
	if before, _, ok := strings.Cut(matchPattern, "*"); ok {
		match = before
	}
	// Append to the queue file on disk.
	data, err := os.ReadFile(f.queueRaw)
	if err != nil {
		f.t.Fatalf("read queue: %v", err)
	}
	var q []scriptedCall
	if err := json.Unmarshal(data, &q); err != nil {
		f.t.Fatalf("parse queue: %v", err)
	}
	q = append(q, scriptedCall{Match: match, Stdout: stdout, Exit: exit})
	out, _ := json.Marshal(q)
	if err := os.WriteFile(f.queueRaw, out, 0644); err != nil {
		f.t.Fatalf("write queue: %v", err)
	}
}

// calls returns every recorded invocation, in order.
func (f *fakeIPMITool) calls() []fakeCall {
	f.t.Helper()
	data, err := os.ReadFile(f.callsLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("read calls log: %v", err)
	}
	var out []fakeCall
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		args, rest, _ := strings.Cut(line, "\t")
		env, tz, _ := strings.Cut(rest, "\t")
		out = append(out, fakeCall{Args: args, Env: env, TZ: tz})
	}
	return out
}

// statefulBMC is an ipmitool replacement that keeps a BMC's user table in a
// JSON file across calls, for tests that run Reconcile more than once against
// one BMC: what a later run lists follows from what earlier runs wrote, which
// the scripted fakeIPMITool cannot express. As on a real BMC, `user list`
// shows each slot's channel-1 privilege limit whatever its enable bit, and
// naming, disabling or setting the password of a slot leaves the limit
// alone. It handles only the commands the in-band path issues and fails on
// any other.
type statefulBMC struct {
	t     *testing.T
	path  string // the fake ipmitool
	state string // its JSON state file
}

// bmcSlot is one user slot of a statefulBMC.
type bmcSlot struct {
	Name     string
	Priv     PrivLevel // channel-1 privilege limit; privNoAccess for none
	Password string
	Enabled  bool // the enable bit, which `user list` does not show
}

type bmcState struct {
	Slots map[int]bmcSlot
	// RefusePasswordWrites makes every `user set password` fail.
	RefusePasswordWrites bool
}

// statefulBMCScript is the fake ipmitool. It keeps its state in state.json
// beside itself.
const statefulBMCScript = `#!/usr/bin/env python3
import json, os, sys

state_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "state.json")
args = sys.argv[1:]
if args[:1] == ["-I"]:
    args = args[2:]
cmd = " ".join(args)
with open(state_path) as f:
    state = json.load(f)
slots = state["Slots"]

def slot(n):
    if n not in slots:
        sys.exit("fake BMC: no slot %s: %s" % (n, cmd))
    return slots[n]

PRIV = {1: "CALLBACK", 2: "USER", 3: "OPERATOR", 4: "ADMINISTRATOR", 15: "NO ACCESS"}
if args[:3] == ["user", "list", "1"]:
    print("ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit")
    for n in sorted(slots, key=int):
        s = slots[n]
        print("%-4s%-17s%-8s%-11s%-11s%s" % (n, s["Name"], "true", "true", "true", PRIV[s["Priv"]]))
elif args[:3] == ["user", "set", "name"]:
    slot(args[3])["Name"] = args[4]
elif args[:3] == ["user", "set", "password"]:
    if state["RefusePasswordWrites"]:
        sys.exit("Set User Password command failed (user %s)" % args[3])
    slot(args[3])["Password"] = args[4]
elif args[:2] == ["user", "priv"]:
    slot(args[2])["Priv"] = int(args[3])
elif args[:2] == ["channel", "setaccess"]:
    for a in args[4:]:
        if a.startswith("privilege="):
            slot(args[3])["Priv"] = int(a[len("privilege="):])
elif args[:2] == ["user", "enable"]:
    slot(args[2])["Enabled"] = True
elif args[:2] == ["user", "disable"]:
    slot(args[2])["Enabled"] = False
elif args[:3] == ["chassis", "bootparam", "set"]:
    pass
else:
    sys.exit("fake BMC: unhandled command: " + cmd)
with open(state_path, "w") as f:
    json.dump(state, f)
`

func newStatefulBMC(t *testing.T, slots map[int]bmcSlot) *statefulBMC {
	t.Helper()
	dir := t.TempDir()
	b := &statefulBMC{t: t, path: filepath.Join(dir, "ipmitool"), state: filepath.Join(dir, "state.json")}
	if err := os.WriteFile(b.path, []byte(statefulBMCScript), 0o755); err != nil {
		t.Fatalf("write fake ipmitool: %v", err)
	}
	b.save(bmcState{Slots: slots})
	return b
}

func (b *statefulBMC) load() bmcState {
	b.t.Helper()
	data, err := os.ReadFile(b.state)
	if err != nil {
		b.t.Fatalf("read fake BMC state: %v", err)
	}
	var s bmcState
	if err := json.Unmarshal(data, &s); err != nil {
		b.t.Fatalf("parse fake BMC state: %v", err)
	}
	return s
}

func (b *statefulBMC) save(s bmcState) {
	b.t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		b.t.Fatalf("encode fake BMC state: %v", err)
	}
	if err := os.WriteFile(b.state, data, 0o644); err != nil {
		b.t.Fatalf("write fake BMC state: %v", err)
	}
}

// refusePasswordWrites makes every later `user set password` fail (true) or
// succeed (false).
func (b *statefulBMC) refusePasswordWrites(refuse bool) {
	b.t.Helper()
	s := b.load()
	s.RefusePasswordWrites = refuse
	b.save(s)
}

// slot returns slot n as the BMC holds it now.
func (b *statefulBMC) slot(n int) bmcSlot {
	b.t.Helper()
	return b.load().Slots[n]
}
