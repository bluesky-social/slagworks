// Package bmcreconcile brings a BMC's IPMI user table into compliance:
// the vendor admin account at slot 2 is left untouched, the desired
// accounts (production: a single `monitor` at USER privilege) exist with
// the right privilege and LAN channel access, and every other named,
// non-reserved account is disabled and its name cleared.
//
// It runs in two places:
//
//   - In-band, from the deploy agent inside the freshly installed OS
//     (`ipmitool -I open` via /dev/ipmi0). In-band IPMI is
//     ADMINISTRATOR-equivalent regardless of the BMC's network privilege
//     model, so the agent needs no network credential; it learns only the
//     admin account's NAME (AdminUserKernelArg) for the slot-2 gate. This
//     path provisions once and never rotates a working password, and it
//     re-asserts the persistent PXE boot order.
//
//   - Over the LAN, from the site worker that owns the BMC's OOB network
//     (the bmc_reconcile op; RemoteOptions / ReconcileRemote). The session
//     authenticates with the device's bmc-creds-admin entry. This path
//     always asserts the monitor password, but rotates it only when asked:
//     it re-asserts the password stored in pop/<pop>/bmc-creds-monitor,
//     which the op carries (testing an existing account first and writing
//     only when the BMC does not hold it, since MegaRAC refuses to set a
//     password to its current value), and generates a fresh one only when
//     the store has none for the device or the operator passed
//     --rotate-monitor. A
//     rotation leaves the oob-gw exporters scraping with a stale password
//     until Ansible re-renders their config, so compliance must not rotate.
//     It never touches boot order, because it may target production hosts
//     slagworks does not boot. The op then runs ReconcileTime (time.go),
//     which brings the BMC's NTP, timezone and clock into compliance over
//     Redfish and IPMI LAN with the same credential.
//
// Both paths refuse before any write unless the bmc-creds-admin account
// (AdminUser) is the reserved slot-2 account: the leftover pass would
// otherwise disable slagworks' only BMC credential.
//
// slagworks itself drives BMCs over the network with the bmc-creds-admin
// credential only; it provisions no account of its own.
package bmcreconcile

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PrivLevel is an IPMI privilege level we set on a user via
// `ipmitool user priv`. The numeric values come from IPMI 2.0 §6.6.
type PrivLevel int

const (
	PrivCallback PrivLevel = 1
	PrivUser     PrivLevel = 2
	PrivOperator PrivLevel = 3
	PrivAdmin    PrivLevel = 4
)

// privNoAccess is the NO ACCESS privilege limit (0xF), which bars a user
// from the channel. Reconcile only writes it: parseUserList reads a slot
// listing NO ACCESS as Privilege 0, disabled.
const privNoAccess PrivLevel = 0xF

func (p PrivLevel) String() string {
	switch p {
	case PrivCallback:
		return "CALLBACK"
	case PrivUser:
		return "USER"
	case PrivOperator:
		return "OPERATOR"
	case PrivAdmin:
		return "ADMINISTRATOR"
	default:
		return fmt.Sprintf("PRIV(%d)", int(p))
	}
}

// passwordLength is the length of generated BMC user passwords.
// 12 characters across a 4-class alphabet (~74 bits of entropy).
// Long enough to resist brute force on a rate-limited IPMI auth path,
// short enough to clear length policies on every BMC firmware we
// deal with. The 4 classes also satisfy AMI MegaRAC's complexity
// policy — see generatePassword for the full story.
const passwordLength = 12

// maxPasswordBytes is the longest password IPMI 2.0 stores (the 20-byte
// form of Set User Password; the 1.5 form is 16).
const maxPasswordBytes = 20

// User is an IPMI user as the BMC reports it via `user list 1`.
// Slot 0 is reserved (per IPMI spec). Slot 1 is conventionally the
// anonymous/null user. Real-world BMCs usually populate slot 2 with
// the vendor's "ADMIN" user.
type User struct {
	Slot      int
	Name      string
	Enabled   bool
	Privilege PrivLevel
}

// DesiredUser describes a user we want present after reconcile. Name
// is the lookup key — the reconciler matches existing users by name
// (case-insensitive) regardless of slot.
//
// Password and Rotate pick how the account's password is handled; set at
// most one. With neither, reconcile provisions once: it writes a generated
// password only to an account it creates (or a partial-create remnant) and
// never rewrites a working one (the in-band deploy path).
type DesiredUser struct {
	Name      string
	Privilege PrivLevel
	// Password is the password the account must have. Reconcile writes it
	// to an account it creates; an existing account, enabled or not, is
	// first tested (`user test`) and written only when it does not already
	// hold it (reassertPassword). Either way a BMC whose account drifted
	// (password changed by hand, slot cleared) converges back to it. The
	// remote path sets it to the device's stored pop/<pop>/bmc-creds-monitor
	// password. It is a secret: it never enters a Result, and runIPMITool
	// redacts it from every error.
	Password string
	// Rotate writes a freshly generated password even to an account that
	// exists and is compliant. The remote path sets it when nothing is
	// stored for the device, or on an explicit --rotate-monitor.
	Rotate bool
}

// Result is what the reconcile phase produces. Phone-home payload
// is built from this struct.
type Result struct {
	// CreatedOrUpdated is the set of users the reconciler wrote a password
	// to. A generated password is carried for the caller to deliver to
	// slagworksd (the agent phones it back, then drops it); a
	// DesiredUser.Password the reconciler wrote is not, since the caller
	// already holds it. Empty slice means everything was already
	// provisioned (subsequent-deploy happy path).
	CreatedOrUpdated []ProvisionedUser

	// PasswordVerified is the set of existing users whose
	// DesiredUser.Password the BMC already held (`user test` matched), so
	// the reconciler wrote no password; their privilege, channel access and
	// enabled state were still re-applied. Password is always empty.
	PasswordVerified []ProvisionedUser

	// CurrentUsers is the post-reconcile snapshot of the user table.
	// Used for drift detection on the slagworksd side.
	CurrentUsers []User

	// LeftoversDisabled names users that were on the BMC but were
	// neither the managed account for a desired name nor reserved
	// (vendor admin at slot 2, anonymous slot 1). The reconciler
	// disabled each and cleared its name where the BMC allowed it (see
	// NamesNotCleared), or, when no empty slot was left for a missing
	// desired account, renamed the slot to that account; the names are
	// reported back for audit.
	LeftoversDisabled []string

	// NamesNotCleared lists leftovers that were disabled but whose name
	// the BMC refused to clear (Supermicro rejects an empty name). They
	// are disabled, so the BMC is compliant; the name stays in the slot.
	NamesNotCleared []string

	// BootOrderPersisted is true if the reconciler wrote a persistent
	// [PXE, disk, ...] boot order: always on in-band success (the
	// reconciler unconditionally re-asserts boot order so a BMC that
	// was hand-poked back to disk-first gets corrected), never with
	// SkipBootOrder.
	BootOrderPersisted bool
}

// ProvisionedUser is a user the reconciler created or updated. The agent
// serializes this into the phone-home payload and discards it from process
// memory immediately after.
type ProvisionedUser struct {
	Slot int
	Name string
	// Password is the password the reconciler generated and wrote; empty
	// when it wrote the caller's DesiredUser.Password instead, so a
	// non-empty Password means the account's password was rotated.
	Password  string
	Privilege PrivLevel
}

// MonitorUser is the read-only account every reconcile provisions: the
// ipmi_exporter scrape credential, stored per host in
// pop/<pop>/bmc-creds-monitor.
const MonitorUser = "monitor"

// DefaultLANCipher is the RMCP+ cipher suite used for a LAN session
// when the device's admin entry sets no ipmi_cipher (suite 17:
// HMAC-SHA256 / AES-CBC-128, the production default).
const DefaultLANCipher = 17

// Options control the reconcile loop.
type Options struct {
	// Desired is the canonical list of users that must be present
	// post-reconcile. Order doesn't matter; matching is by Name.
	// Production passes [{monitor, USER}].
	Desired []DesiredUser

	// IPMITool is the path to the ipmitool binary. Empty defaults to
	// "ipmitool" on PATH. Tests override to a fake.
	IPMITool string

	// IPMIInterface picks the ipmitool -I value. Default "open" =
	// in-band via /dev/ipmi0. The remote path uses "lanplus".
	IPMIInterface string

	// Args is extra args placed after -I: the LAN connection details
	// (-H/-U/-C/-E) for the remote path.
	Args []string

	// Env is appended to the ipmitool process environment. The remote
	// path passes the session password as IPMI_PASSWORD (read by -E) so
	// it never appears on argv.
	Env []string

	// AdminUser, when set, is the device's bmc-creds-admin account —
	// slagworks' only BMC credential, and the remote path's LAN session
	// account. Reconcile refuses before any write (*AdminNotSlot2Error)
	// unless that account is the reserved slot-2 account: the leftover
	// pass disables every other non-desired account, which would
	// otherwise include it.
	AdminUser string

	// SkipBootOrder suppresses the persistent force_pxe boot-order write.
	// The remote path sets it: it may target production hosts slagworks
	// doesn't boot.
	SkipBootOrder bool
}

// secrets returns every password opt carries: the Desired users' passwords
// and the LAN session's IPMI_PASSWORD. runIPMITool redacts all of them from
// error text, whichever command failed.
func (o Options) secrets() []string {
	var out []string
	for _, d := range o.Desired {
		out = append(out, d.Password)
	}
	for _, e := range o.Env {
		if v, ok := strings.CutPrefix(e, "IPMI_PASSWORD="); ok {
			out = append(out, v)
		}
	}
	return out
}

// LANTarget is a BMC reached over IPMI LAN and the admin account the
// session authenticates as (the device's bmc-creds-admin entry).
type LANTarget struct {
	Host     string
	Username string
	Password string
	// Cipher is the RMCP+ cipher suite; 0 selects DefaultLANCipher.
	Cipher int
}

// lanSession returns the Options for an ipmitool session to t over the LAN:
// `ipmitool -I lanplus -H <host> -U <user> -C <cipher> -E` with the
// password in IPMI_PASSWORD, never on argv.
func lanSession(t LANTarget) Options {
	cipher := t.Cipher
	if cipher == 0 {
		cipher = DefaultLANCipher
	}
	return Options{
		IPMIInterface: "lanplus",
		Args:          []string{"-H", t.Host, "-U", t.Username, "-C", strconv.Itoa(cipher), "-E"},
		Env:           []string{"IPMI_PASSWORD=" + t.Password},
	}
}

// RemoteOptions builds the Options for a compliance reconcile over the
// LAN: a lanSession as the admin account, a single monitor account at USER
// privilege, the slot-2 admin gate on the session account, and no
// boot-order write. monitorPassword is the device's stored
// pop/<pop>/bmc-creds-monitor password, which is re-asserted; empty (no
// stored entry, or an explicit rotation) rotates the monitor password to a
// freshly generated one.
func RemoteOptions(t LANTarget, monitorPassword string) Options {
	opt := lanSession(t)
	opt.Desired = []DesiredUser{{
		Name:      MonitorUser,
		Privilege: PrivUser,
		Password:  monitorPassword,
		Rotate:    monitorPassword == "",
	}}
	opt.AdminUser = t.Username
	opt.SkipBootOrder = true
	return opt
}

// ReconcileRemote runs Reconcile with RemoteOptions(t, monitorPassword)
// against ipmitool on PATH.
func ReconcileRemote(ctx context.Context, t LANTarget, monitorPassword string) (*Result, error) {
	return Reconcile(ctx, RemoteOptions(t, monitorPassword))
}

// reservedSlots are slots the reconciler never touches:
//   - slot 1: IPMI 2.0 reserves this for the anonymous/null user.
//   - slot 2: the vendor admin. This is the account in
//     `pop/<pop>/bmc-creds-admin`, the only credential slagworks uses
//     to drive the BMC. Touching it would desync the BMC from that
//     secret without operator sign-off.
var reservedSlots = map[int]bool{
	1: true,
	2: true,
}

// Reconcile runs the full reconcile loop:
//  1. List current users via `ipmitool user list 1`.
//  2. With AdminUser set, refuse (before any write) unless it is the
//     slot-2 account.
//  3. Add any user in Options.Desired that's missing (and re-password the
//     ones that exist when they carry a Password or Rotate), in an empty
//     slot or, when none is left, in a leftover's slot (disabled and set
//     to NO ACCESS first).
//  4. Disable + clear name on any user that's not the managed account
//     for a Desired name and not in a reserved slot (slot 1 anon, slot 2
//     vendor admin). A second account sharing a Desired name is a
//     leftover too: compliance is exactly one account per Desired name.
//  5. Unless SkipBootOrder, write the persistent [PXE, disk, ...] boot
//     order.
//
// Failures partway through leave the BMC in a partial state — that's
// acceptable because reconcile is idempotent: re-running picks up
// where the previous run left off. A nil Result means nothing was
// written; a non-nil Result alongside an error carries every generated
// password already written, which the caller must still deliver to
// slagworksd.
func Reconcile(ctx context.Context, opt Options) (*Result, error) {
	if opt.IPMITool == "" {
		opt.IPMITool = "ipmitool"
	}
	if opt.IPMIInterface == "" {
		opt.IPMIInterface = "open"
	}
	for _, d := range opt.Desired {
		if d.Password != "" && d.Rotate {
			return nil, fmt.Errorf("bmcreconcile: desired user %q sets both a password to write and Rotate; set at most one", d.Name)
		}
		// ipmitool truncates a longer password to test it and refuses to
		// set it, so it could be neither verified nor written.
		if len(d.Password) > maxPasswordBytes {
			return nil, fmt.Errorf("bmcreconcile: the password for desired user %q is longer than %d bytes, the IPMI maximum; "+
				"fix the stored entry, or re-run with --rotate-monitor to replace it with a fresh one", d.Name, maxPasswordBytes)
		}
	}

	current, err := ListUsers(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("bmcreconcile: list users: %w", err)
	}

	if opt.AdminUser != "" {
		if err := checkAdminUser(current, opt.AdminUser); err != nil {
			return nil, err
		}
	}

	res := &Result{CurrentUsers: current}

	// Index existing users by lowercase name. The first (lowest-slot)
	// account carrying a name is the one reconcile manages; any later
	// duplicate is a leftover.
	byName := map[string]User{}
	for _, u := range current {
		name := strings.ToLower(strings.TrimSpace(u.Name))
		if name == "" {
			continue
		}
		if _, dup := byName[name]; !dup {
			byName[name] = u
		}
	}

	// Plan: which Desired entries need a write? managed records the slot
	// each Desired name ends up owning, so slot allocation and the leftover
	// pass spare exactly those accounts.
	managed := map[int]bool{}
	var plans []plan
	for _, d := range opt.Desired {
		if existing, ok := byName[strings.ToLower(d.Name)]; ok {
			// Refuse to update a user that lives in a reserved slot,
			// even if the name happens to match a Desired entry. The
			// disable path below also gates on reservedSlots, but
			// without this check a Desired entry naming "ADMIN" (or
			// whatever the vendor put at slot 2) would silently
			// rotate the bmc-creds-admin account — slagworks' only
			// credential for this BMC — and lock it out. Fail loud
			// instead.
			if reservedSlots[existing.Slot] {
				return nil, fmt.Errorf("bmcreconcile: refusing to manage reserved slot %d (user %q): "+
					"slot 2 is the bmc-creds-admin account; remove it from Options.Desired",
					existing.Slot, existing.Name)
			}
			managed[existing.Slot] = true
			if d.Password != "" || d.Rotate {
				// Re-assert the caller's password, or rotate: write it
				// whatever state the account is in.
				plans = append(plans, plan{
					desired:       d,
					slot:          existing.Slot,
					exists:        true,
					needsPassword: true,
				})
				continue
			}
			if existing.Privilege == d.Privilege && existing.Enabled {
				continue // nothing to do
			}
			// An existing-but-DISABLED user is treated as not-yet-fully-
			// provisioned and gets a fresh password (needsPassword). This
			// covers the partial-create crash window: create does
			// NO ACCESS -> set-name -> set-password -> priv -> ENABLE in
			// that order, so a crash after set-name but before the priv
			// write leaves the slot named and listing NO ACCESS, which
			// parseUserList reads as disabled, with a password slagworksd
			// never received. Without needsPassword we'd "fix" such a slot to enabled
			// at the right priv with an unknown password and never report a
			// cred — a stranded account (idempotency-of-partial-retry
			// break). An ENABLED user with merely the wrong privilege is a
			// genuine in-place priv fix: leave its working password alone.
			plans = append(plans, plan{
				desired:       d,
				slot:          existing.Slot,
				exists:        true,
				needsPassword: !existing.Enabled,
			})
		} else {
			plans = append(plans, plan{desired: d, slot: -1, needsPassword: true})
		}
	}

	// Allocate slots for the new users: empty slots first, then slots
	// holding a leftover (see freeSlots).
	if needAlloc := countSlots(plans); needAlloc > 0 {
		free := freeSlots(current, needAlloc, managed)
		if len(free) < needAlloc {
			var names []string
			for _, p := range plans {
				if p.slot == -1 {
					names = append(names, strconv.Quote(p.desired.Name))
				}
			}
			return nil, fmt.Errorf("bmcreconcile: need a user slot for each of %s, but only %d slot(s) outside the reserved "+
				"slots 1 and 2 hold no desired account (total slots: %d); the BMC's user table is too small for the desired "+
				"accounts: remove an entry from Options.Desired, or provision this BMC's accounts by hand",
				strings.Join(names, ", "), len(free), len(current))
		}
		i := 0
		for j := range plans {
			if plans[j].slot == -1 {
				plans[j].slot = free[i].Slot
				plans[j].replaces = strings.TrimSpace(free[i].Name)
				managed[free[i].Slot] = true
				i++
			}
		}
	}

	// Apply each plan. For a Desired user with neither Password nor Rotate
	// this is PROVISION ONCE, NEVER ROTATE: a password is generated and
	// written ONLY when CREATING an absent user. For a user that already
	// exists but has the wrong privilege or is disabled, we fix the
	// privilege/channel-access/enabled state IN PLACE and leave its password
	// untouched — so the monitor cred already stored in
	// pop/<pop>/bmc-creds-monitor stays valid. Regenerating on every deploy
	// made the cred a moving target that drifted whenever a deploy half-ran.
	// The remote path instead always asserts a password: the stored one
	// (Password), written only when the BMC does not already hold it, or a
	// generated one (Rotate) only when nothing is stored or the operator
	// asked for a rotation.
	for _, p := range plans {
		if p.needsPassword {
			// ASSERT A PASSWORD (absent user, an existing-but-disabled slot
			// from a partial create, or a Password/Rotate user): the caller's
			// Password, else a generated one. Only a generated password is
			// reported: it is the only one the caller does not already hold.
			// Error paths below return res (not nil): earlier plans in
			// this loop may have already written passwords to the BMC,
			// and those live creds MUST reach the phone-home payload
			// even when a later plan fails — discarding them here is
			// how a partial multi-user failure silently drifts the BMC
			// from slagworksd's stored creds. A user whose password was
			// written is reported before its priv/enable writes, so a
			// named-but-misconfigured slot with a known password is still
			// reported when those fail.
			if p.replaces != "" {
				// Taking over a leftover's slot: disable it before the
				// rename, so the leftover's password stops authenticating
				// now. The NO ACCESS write below is what makes a run that
				// dies before the password write leave a slot the next run
				// reads as disabled and re-provisions as a partial create.
				if err := runIPMIToolVoid(ctx, opt, "user", "disable", strconv.Itoa(p.slot)); err != nil {
					return res, fmt.Errorf("bmcreconcile: disable leftover slot %d (%s) to reuse it for %s: %w",
						p.slot, p.replaces, p.desired.Name, err)
				}
				res.LeftoversDisabled = append(res.LeftoversDisabled, p.replaces)
			}
			if !p.exists {
				// Drop the slot to NO ACCESS before naming it, so a run that
				// stops before the priv write leaves a remnant the next run
				// reads as disabled and re-passwords. A free slot keeps the
				// privilege limit of whatever account held it last (the
				// leftover pass disables and clears an account but leaves
				// its limit); named at that limit, the remnant would read
				// as an enabled account and get enabled without a password
				// write, live with the previous occupant's password.
				if err := runIPMIToolVoid(ctx, opt, "user", "priv",
					strconv.Itoa(p.slot), strconv.Itoa(int(privNoAccess)), "1"); err != nil {
					return res, fmt.Errorf("bmcreconcile: set NO ACCESS on slot %d before naming it: %w", p.slot, err)
				}
				if err := runIPMIToolVoid(ctx, opt, "user", "set", "name",
					strconv.Itoa(p.slot), p.desired.Name); err != nil {
					return res, fmt.Errorf("bmcreconcile: set name slot %d: %w", p.slot, err)
				}
			}
			u := ProvisionedUser{Slot: p.slot, Name: p.desired.Name, Privilege: p.desired.Privilege}
			if p.exists && p.desired.Password != "" {
				// RE-ASSERT on an existing account: test before writing.
				wrote, err := reassertPassword(ctx, opt, p.slot, p.desired.Password)
				switch {
				case wrote:
					res.CreatedOrUpdated = append(res.CreatedOrUpdated, u)
				case err == nil:
					res.PasswordVerified = append(res.PasswordVerified, u)
				}
				if err != nil {
					return res, err
				}
			} else {
				password := p.desired.Password
				if password == "" {
					pw, err := generatePassword(passwordLength)
					if err != nil {
						return res, fmt.Errorf("bmcreconcile: generate password: %w", err)
					}
					password, u.Password = pw, pw
				}
				if err := setPassword(ctx, opt, p.slot, password); err != nil {
					return res, err
				}
				res.CreatedOrUpdated = append(res.CreatedOrUpdated, u)
			}
			if err := applyPrivAndEnable(ctx, opt, p.slot, p.desired.Privilege); err != nil {
				return res, err
			}
			continue
		}
		// UPDATE IN PLACE: the user exists, is ENABLED (so it has a working
		// password slagworksd already holds), but its privilege is wrong. Fix
		// priv + channel access + enabled WITHOUT touching the password. No
		// CreatedOrUpdated entry: the password is unchanged, so there is
		// nothing new to phone home and global keeps the cred it has.
		// (provision-once: never rotate a working cred.)
		if err := applyPrivAndEnable(ctx, opt, p.slot, p.desired.Privilege); err != nil {
			return res, err
		}
	}

	// Disable any user that's not the managed account for a desired name
	// and not in a reserved slot. Reading from the pre-write `current`
	// list is fine because both exclusions go by slot number, whatever
	// name `current` shows there: a slot we just provisioned (including a
	// leftover's slot we took over) is a managed slot.
	for _, u := range current {
		if reservedSlots[u.Slot] || managed[u.Slot] {
			continue
		}
		name := strings.TrimSpace(u.Name)
		if name == "" {
			continue
		}
		// Disable first so an in-flight session can't auth, then
		// clear the name so the slot reports as empty. Disabling is what
		// makes the BMC compliant; the name clear only tidies the slot,
		// and Supermicro BMCs reject an empty name ("Invalid data field in
		// request"), so a failed clear is recorded rather than aborting
		// the leftovers after it.
		if err := runIPMIToolVoid(ctx, opt, "user", "disable",
			strconv.Itoa(u.Slot)); err != nil {
			return res, fmt.Errorf("bmcreconcile: disable leftover slot %d (%s): %w",
				u.Slot, name, err)
		}
		if err := runIPMIToolVoid(ctx, opt, "user", "set", "name",
			strconv.Itoa(u.Slot), ""); err != nil {
			res.NamesNotCleared = append(res.NamesNotCleared, name)
		}
		res.LeftoversDisabled = append(res.LeftoversDisabled, name)
	}
	sort.Strings(res.LeftoversDisabled)
	sort.Strings(res.NamesNotCleared)

	// Re-read user list so Result.CurrentUsers reflects post-reconcile state.
	post, err := ListUsers(ctx, opt)
	if err != nil {
		return res, fmt.Errorf("bmcreconcile: re-list users: %w", err)
	}
	res.CurrentUsers = post

	if opt.SkipBootOrder {
		return res, nil
	}

	// Boot order: persistent [PXE, disk, ...]. ipmitool's
	// `chassis bootparam set bootflag force_pxe options=PEF,timestamp,reset_persistence`
	// command sets the boot flags valid bit so the BIOS honors the
	// override on next boot, and clears the "valid for one boot only"
	// flag so it persists across reboots.
	//
	// We don't read-then-conditionally-write because (a) BMC firmware
	// often returns "boot flags invalid" when no override is set, and
	// (b) writing is idempotent — the same flags written twice is a
	// no-op on the BMC side.
	if err := runIPMIToolVoid(ctx, opt,
		"chassis", "bootparam", "set", "bootflag", "force_pxe",
		"options=PEF,timestamp,reset_persistence",
	); err != nil {
		return res, fmt.Errorf("bmcreconcile: set persistent PXE boot: %w", err)
	}
	res.BootOrderPersisted = true

	return res, nil
}

// AdminUserKernelArg is the deploy kernel cmdline key carrying the
// device's bmc-creds-admin account name (never the password) to the
// in-band reconcile's slot-2 gate. Its value is EncodeAdminUserArg-encoded.
const AdminUserKernelArg = "slagworks.bmc_admin_user"

// EncodeAdminUserArg encodes an account name for the kernel cmdline (and
// the iPXE script line that carries it): query-escaping leaves only
// [A-Za-z0-9-_.~%+], so whitespace, quotes, '$' (iPXE settings expansion)
// and '=' can never split or corrupt the token.
func EncodeAdminUserArg(name string) string { return url.QueryEscape(name) }

// DecodeAdminUserArg reverses EncodeAdminUserArg. An empty or malformed
// value is an error: the producer omits the arg rather than send it empty.
func DecodeAdminUserArg(v string) (string, error) {
	name, err := url.QueryUnescape(v)
	if err != nil {
		return "", fmt.Errorf("bmcreconcile: malformed %s value: %w", AdminUserKernelArg, err)
	}
	if name == "" {
		return "", fmt.Errorf("bmcreconcile: empty %s value", AdminUserKernelArg)
	}
	return name, nil
}

// AdminNotSlot2Error is the slot-2 gate's refusal: the bmc-creds-admin
// account is not (unambiguously) the reserved slot-2 account, so the
// leftover pass would disable slagworks' only BMC credential. Nothing was
// written.
type AdminNotSlot2Error struct {
	// User is the bmc-creds-admin account name.
	User string
	// Slot is where an account named User sits outside slot 2; 0 when no
	// such account is on the BMC.
	Slot int
	// Slot2User is the account slot 2 holds ("" when empty).
	Slot2User string
}

func (e *AdminNotSlot2Error) Error() string {
	where := "is not on the BMC"
	if e.Slot != 0 {
		where = fmt.Sprintf("is at slot %d", e.Slot)
	}
	if strings.EqualFold(e.Slot2User, e.User) {
		// Slot 2 matches, but a second account with the same name exists.
		return fmt.Sprintf("bmcreconcile: refusing to reconcile: the bmc-creds-admin account %q is at slot 2 but an account "+
			"with the same name also exists at slot %d, so it is ambiguous which one the credential names; "+
			"remove or rename the slot-%d account on the BMC first", e.User, e.Slot, e.Slot)
	}
	return fmt.Sprintf("bmcreconcile: refusing to reconcile: the bmc-creds-admin account %q %s, not the reserved slot 2 "+
		"(slot 2 holds %q), so the leftover-account pass would disable slagworks' only BMC credential; "+
		"point the device's pop/<pop>/bmc-creds-admin entry (keyed by NetBox serial) at the slot-2 account first",
		e.User, where, e.Slot2User)
}

// checkAdminUser is the pre-write slot-2 gate (see AdminNotSlot2Error).
// Names compare case-insensitively, like the rest of the reconciler: a
// casing-only difference is the same account unless a second account
// matching adminUser exists, which is ambiguous and refused.
func checkAdminUser(users []User, adminUser string) error {
	slot2 := ""
	otherSlot := 0
	for _, u := range users {
		name := strings.TrimSpace(u.Name)
		switch {
		case u.Slot == 2:
			slot2 = name
		case otherSlot == 0 && strings.EqualFold(name, adminUser):
			otherSlot = u.Slot
		}
	}
	if !strings.EqualFold(slot2, adminUser) || otherSlot != 0 {
		return &AdminNotSlot2Error{User: adminUser, Slot: otherSlot, Slot2User: slot2}
	}
	return nil
}

// ListUsers parses `ipmitool user list 1` output into User records.
// ipmitool's list format is fixed-width columns (parseUserList has the
// byte-level detail):
//
//	ID  Name              Callin   Link Auth  IPMI Msg  Channel Priv Limit
//	1                     true     false      false     ADMINISTRATOR
//	2   ADMIN             true     true       true      ADMINISTRATOR
//
// Empty-name slots have spaces in the Name column; we strip and
// tolerate.
func ListUsers(ctx context.Context, opt Options) ([]User, error) {
	if opt.IPMITool == "" {
		opt.IPMITool = "ipmitool"
	}
	if opt.IPMIInterface == "" {
		opt.IPMIInterface = "open"
	}
	out, err := runIPMITool(ctx, opt, "user", "list", "1")
	if err != nil {
		return nil, fmt.Errorf("user list: %w", err)
	}
	return parseUserList(out)
}

// parseUserList parses ipmitool's `user list 1` output. The format
// is column-aligned with a header row; we use the header to find the
// byte offsets of each column boundary, then slice data rows at those
// offsets.
//
// Why column offsets and not strings.Fields: the Name column can be
// blank, and `Fields` collapses runs of whitespace, so the columns
// shift when name is empty.
//
// In a terminal the header looks like:
//
//	ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit
//
// but that is a rendering: ipmitool prints it as a C literal holding two
// TABs ("ID  Name\t     Callin  Link Auth\tIPMI Msg   Channel Priv Limit",
// lib/ipmi_user.c dump_user_access), while each data row is
// printf("%-4d%-17s%-8s%-11s%-11s%-s") and holds only spaces. The header's
// offsets match the rows' columns (Name 4, Callin 21, Channel 51) only once
// the TABs are expanded to 8-column stops, so the header is expanded before
// its offsets are taken; on the raw bytes Callin lands at 14 and Channel at
// 43, which truncates names and misses every privilege. The rows are sliced
// as printed.
//
// We capture the start of "Name" and the start of "Callin" — those
// bracket the Name field. The privilege string is everything from
// "Channel" onwards (the right edge varies but the privilege token
// starts at that column, so right-trimmed slice is fine).
func parseUserList(s string) ([]User, error) {
	lines := strings.Split(s, "\n")
	var headerLine string
	var dataLines []string
	for _, line := range lines {
		if line == "" {
			continue
		}
		if headerLine == "" && strings.HasPrefix(strings.TrimSpace(line), "ID") {
			headerLine = line
			continue
		}
		if headerLine != "" {
			dataLines = append(dataLines, line)
		}
	}
	if headerLine == "" {
		return nil, errors.New("no header row in user list output")
	}
	headerLine = expandTabs(headerLine)

	nameStart := strings.Index(headerLine, "Name")
	callinStart := strings.Index(headerLine, "Callin")
	channelStart := strings.Index(headerLine, "Channel")
	if nameStart < 0 || callinStart < 0 || channelStart < 0 {
		return nil, fmt.Errorf("user list header missing expected columns: %q", headerLine)
	}

	var users []User
	for i, line := range dataLines {
		// Don't fail rows shorter than expected — some BMCs trim
		// trailing whitespace, so a NO-ACCESS row may end before the
		// channel column. Pad virtually.
		slotStr := strings.TrimSpace(safeSlice(line, 0, nameStart))
		if slotStr == "" {
			continue
		}
		slot, err := strconv.Atoi(slotStr)
		if err != nil {
			return nil, fmt.Errorf("line %d: bad slot %q: %w", i, slotStr, err)
		}

		name := strings.TrimSpace(safeSlice(line, nameStart, callinStart))
		privStr := strings.TrimSpace(safeSlice(line, channelStart, len(line)))

		priv := parsePriv(privStr)
		enabled := priv != 0

		users = append(users, User{
			Slot:      slot,
			Name:      name,
			Enabled:   enabled,
			Privilege: priv,
		})
	}
	if len(users) == 0 {
		return nil, errors.New("no user rows parsed (output format unexpected)")
	}
	return users, nil
}

// expandTabs replaces each TAB in s with the spaces that reach the next
// 8-column tab stop, as a terminal renders it.
func expandTabs(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\t' {
			b.WriteByte(s[i])
			continue
		}
		b.WriteString(strings.Repeat(" ", 8-b.Len()%8))
	}
	return b.String()
}

// safeSlice clamps start/end to the string's bounds so callers
// don't have to do per-line length checks. Empty for inverted or
// out-of-range ranges.
func safeSlice(s string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(s) {
		end = len(s)
	}
	if start >= end {
		return ""
	}
	return s[start:end]
}

func parsePriv(s string) PrivLevel {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "CALLBACK":
		return PrivCallback
	case "USER":
		return PrivUser
	case "OPERATOR":
		return PrivOperator
	case "ADMINISTRATOR", "ADMIN":
		return PrivAdmin
	default:
		return 0
	}
}

// freeSlots returns up to n slots for new accounts, never a reserved slot
// (1, 2) or a managed one (a desired account's): empty slots first, then
// slots holding a leftover, which the leftover pass would disable anyway.
// Falling back to a leftover's slot is what lets a BMC whose every
// non-reserved slot is named (strays on an inherited host, or Supermicro
// leftovers disabled earlier whose names the BMC refused to clear)
// converge instead of refusing on every run until an operator deletes an
// account by hand.
func freeSlots(users []User, n int, managed map[int]bool) []User {
	var empty, leftovers []User
	for _, u := range users {
		if reservedSlots[u.Slot] || managed[u.Slot] {
			continue
		}
		if strings.TrimSpace(u.Name) == "" {
			empty = append(empty, u)
		} else {
			leftovers = append(leftovers, u)
		}
	}
	empty = append(empty, leftovers...)
	return empty[:min(n, len(empty))]
}

// countSlots returns how many plans still need a slot allocated.
func countSlots(plans []plan) int {
	n := 0
	for _, p := range plans {
		if p.slot == -1 {
			n++
		}
	}
	return n
}

// plan is the write one Desired entry needs.
type plan struct {
	desired DesiredUser
	slot    int
	exists  bool
	// replaces is the leftover account whose slot this new account takes
	// over because no empty slot was left ("" for an empty slot or an
	// existing account). It is disabled and set to NO ACCESS before the
	// rename, and reported in LeftoversDisabled.
	replaces string
	// needsPassword is true when this slot must get a password written: an
	// absent user, an existing-but-disabled slot left by a partial create,
	// or any existing user whose Desired entry carries a Password or Rotate.
	// False for an enabled existing user whose only problem is the wrong
	// privilege — provision-once leaves its working password untouched.
	needsPassword bool
}

// setPassword writes password to the slot. Some BMC firmware (notably
// Supermicro AMI on H13SSH boards) rejects `user set password` with
// "Request data field length exceeded" if the slot isn't fully provisioned
// yet: try the v1.5 16-byte form (ipmitool default) first, then the v2.0
// 20-byte form, then a short sleep + retry the v1.5. Generated passwords
// are 12 ASCII chars, well under either byte limit.
//
// A failed attempt does not show that the slot lacks the password: the BMC
// may have applied a write whose response was lost, or the slot may have
// held the password already. AMI MegaRAC then refuses every further write
// of it (0x19: it is the current password), so the ladder would fail with
// the password live on the BMC, and a generated one would never reach the
// caller. After each failure the slot is therefore tested (`user test`),
// and a slot that holds the password is a success. A test that cannot run
// proves nothing, so the ladder goes on.
func setPassword(ctx context.Context, opt Options, slot int, password string) error {
	var passErr error
	for _, attempt := range []struct {
		args  []string
		sleep time.Duration
	}{
		{[]string{"user", "set", "password", strconv.Itoa(slot), password}, 0},
		{[]string{"user", "set", "password", strconv.Itoa(slot), password, "20"}, 0},
		{[]string{"user", "set", "password", strconv.Itoa(slot), password}, 2 * time.Second},
	} {
		if attempt.sleep > 0 {
			time.Sleep(attempt.sleep)
		}
		passErr = runIPMIToolVoid(ctx, opt, attempt.args...)
		if passErr == nil {
			return nil
		}
		if held, err := testPassword(ctx, opt, slot, password); err == nil && held {
			return nil
		}
	}
	return fmt.Errorf("bmcreconcile: set password slot %d: %w", slot, passErr)
}

// reassertPassword makes an existing slot hold password, writing it only when
// the BMC does not already hold it, and reports whether it wrote. Testing
// first is required, not an optimization: AMI MegaRAC refuses to set a
// password to its current value (completion code 0x19, its reuse-history
// refusal), which failed every compliance run on cpu3.pop3 (2026-10-30).
// After a write the password is tested again, so a write the BMC did not
// apply is an error rather than a compliant result.
func reassertPassword(ctx context.Context, opt Options, slot int, password string) (bool, error) {
	held, err := testPassword(ctx, opt, slot, password)
	if err != nil || held {
		return false, err
	}
	if err := setPassword(ctx, opt, slot, password); err != nil {
		return false, fmt.Errorf("bmcreconcile: slot %d does not hold the stored password, and writing it failed "+
			"(MegaRAC refuses a password still in its reuse history with completion code 0x19): %w; "+
			"set the account's password on the BMC to the stored one, or re-run with --rotate-monitor to replace it with a fresh one", slot, err)
	}
	held, err = testPassword(ctx, opt, slot, password)
	if err != nil {
		return true, fmt.Errorf("bmcreconcile: wrote the stored password to slot %d but could not confirm it: %w", slot, err)
	}
	if !held {
		return true, fmt.Errorf("bmcreconcile: wrote the stored password to slot %d but the BMC does not confirm holding it (user test still fails); "+
			"check the account on the BMC, or re-run with --rotate-monitor to replace it with a fresh one", slot)
	}
	return true, nil
}

// testPassword reports whether slot holds password, asking the BMC with
// `ipmitool user test <slot> <16|20> <password>` (Set User Password,
// operation "test password") at each size the password fits. A BMC stores
// a password as 16 or 20 bytes, and ipmitool truncates a test password to
// the size, so a longer password tested at 16 could match on its prefix.
// Reconcile refuses a password over maxPasswordBytes up front, so at least
// the 20-byte test always runs.
//
// ipmitool (lib/ipmi_user.c, ipmi_user_test_password) exits 0 only on a
// match, printing "Success". Otherwise it exits 1 and prints "Failure:
// password incorrect" (completion code 0x80: right size, wrong password),
// "Failure: wrong password size" (0x81), or "Unknown error" (any other
// code, or no response). The exit status decides a match; the output is
// read only to tell a mismatch from a test that could not run.
//
// 0x80 at a size is decisive: the BMC holds a different password of that
// size. 0x81 at every size tested is also no match. Any other failure with
// no match is an error, since whether the BMC holds the password is
// unknown.
func testPassword(ctx context.Context, opt Options, slot int, password string) (bool, error) {
	var inconclusive error
	for _, size := range []int{16, 20} {
		if len(password) > size {
			continue
		}
		out, err := runIPMITool(ctx, opt, "user", "test", strconv.Itoa(slot), strconv.Itoa(size), password)
		switch {
		case err == nil:
			return true, nil
		case strings.Contains(out, "Failure: password incorrect"):
			return false, nil
		case strings.Contains(out, "Failure: wrong password size"):
		default:
			inconclusive = err
		}
	}
	if inconclusive != nil {
		return false, fmt.Errorf("bmcreconcile: could not test the password of slot %d: %w", slot, inconclusive)
	}
	return false, nil
}

// applyPrivAndEnable sets a user's privilege (per-channel), grants LAN channel
// access, and enables the user — idempotent, password-untouched. Used both when
// creating a user (after the password write) and when fixing an existing user's
// privilege in place (provision-once: never rewrite the password).
func applyPrivAndEnable(ctx context.Context, opt Options, slot int, priv PrivLevel) error {
	// Privilege is per-channel. Channel 1 is the primary LAN channel on every
	// BMC we deal with.
	if err := runIPMIToolVoid(ctx, opt, "user", "priv",
		strconv.Itoa(slot), strconv.Itoa(int(priv)), "1"); err != nil {
		return fmt.Errorf("bmcreconcile: set priv slot %d: %w", slot, err)
	}
	// `user priv` only sets the privilege ceiling; without `channel setaccess`
	// granting `link=on` + `ipmi=on` for channel 1, the user won't be able to
	// authenticate over the LAN at all (BMC accepts the auth handshake then
	// silently rejects with "insufficient channel access"). We always enable
	// callin/ipmi/link on channel 1 because every downstream consumer
	// (slagworksd power ops, ipmi_exporter scrapes) talks to the BMC over LAN.
	privArg := "privilege=" + strconv.Itoa(int(priv))
	if err := runIPMIToolVoid(ctx, opt, "channel", "setaccess", "1",
		strconv.Itoa(slot), "callin=on", "ipmi=on", "link=on", privArg); err != nil {
		return fmt.Errorf("bmcreconcile: channel setaccess slot %d: %w", slot, err)
	}
	if err := runIPMIToolVoid(ctx, opt, "user", "enable",
		strconv.Itoa(slot)); err != nil {
		return fmt.Errorf("bmcreconcile: enable slot %d: %w", slot, err)
	}
	return nil
}

// generatePassword returns a random password drawn from a 4-class
// alphabet (uppercase + lowercase + digits + special), guaranteed
// to contain at least one character from each class. n controls the
// length; production uses passwordLength (12).
//
// Why 4-class: AMI MegaRAC SP-X firmware (Supermicro H13SSH +
// likely others) rejects passwords that fail an internal complexity
// policy with the same IPMI completion code (0xC8, "Request data
// field length exceeded") it uses for actual length issues. Our
// previous all-uppercase-and-digits passwords tripped this on
// every kworker-l34-1.pop1 deploy, masking as a length problem.
// See docs/user/hardware/bmc-vendor-matrix.md and
// truenas/middleware#11325.
//
// Special-character set is conservative — common policy-allowed
// punctuation that isn't load-bearing in shell, JSON, or URL
// contexts (we never put these passwords in those contexts, but
// belt-and-suspenders).
//
// Implementation note: rejection sampling avoids modulo bias
// (`b % len(alphabet)` skews if 256 isn't a multiple of alphabet
// length). After producing n chars we re-roll the whole password
// if it doesn't have at least one of each class. With a 12-char
// password and a balanced 4-class alphabet the first-roll success
// rate is >99%, so the retry loop is effectively a no-op.
func generatePassword(n int) (string, error) {
	const (
		upper   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		lower   = "abcdefghijklmnopqrstuvwxyz"
		digits  = "0123456789"
		special = "!@#$%^&*"
	)
	const alphabet = upper + lower + digits + special
	const max = 256 - (256 % len(alphabet))

	for range 20 {
		out := make([]byte, n)
		buf := make([]byte, 1)
		for i := 0; i < n; {
			if _, err := rand.Read(buf); err != nil {
				return "", err
			}
			if int(buf[0]) >= max {
				continue
			}
			out[i] = alphabet[int(buf[0])%len(alphabet)]
			i++
		}
		s := string(out)
		if strings.ContainsAny(s, upper) &&
			strings.ContainsAny(s, lower) &&
			strings.ContainsAny(s, digits) &&
			strings.ContainsAny(s, special) {
			return s, nil
		}
	}
	return "", errors.New("bmcreconcile: failed to generate complexity-compliant password after 20 tries (length too short for 4-class requirement?)")
}

// runIPMITool runs ipmitool with the given args and returns combined
// stdout+stderr on success. Failure cases attach the command line and
// output to the error for diagnosis — with every password redacted,
// because the error text lands in op results and logs.
func runIPMITool(ctx context.Context, opt Options, args ...string) (string, error) {
	cmdArgs := append([]string{"-I", opt.IPMIInterface}, opt.Args...)
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, opt.IPMITool, cmdArgs...)
	if len(opt.Env) > 0 {
		cmd.Env = append(os.Environ(), opt.Env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		secrets := append(passwordArgs(cmdArgs), opt.secrets()...)
		return string(out), fmt.Errorf("ipmitool %s: %w (output: %s)",
			redact(strings.Join(cmdArgs, " "), secrets),
			err, redact(strings.TrimSpace(string(out)), secrets))
	}
	return string(out), nil
}

// passwordArgs returns the password values on an ipmitool command line:
// the argument after -P, the password of `user set password <slot>
// <password> [16|20]`, and the password of `user test <slot> <16|20>
// <password>`.
func passwordArgs(cmdArgs []string) []string {
	var out []string
	for i, a := range cmdArgs {
		if a == "-P" && i+1 < len(cmdArgs) {
			out = append(out, cmdArgs[i+1])
		}
		if a == "password" && i >= 2 && cmdArgs[i-2] == "user" && cmdArgs[i-1] == "set" && i+2 < len(cmdArgs) {
			out = append(out, cmdArgs[i+2])
		}
		if a == "test" && i >= 1 && cmdArgs[i-1] == "user" && i+3 < len(cmdArgs) {
			out = append(out, cmdArgs[i+3])
		}
	}
	return out
}

// redact replaces every non-empty secret in s with "<redacted>", longest
// first, so a secret that contains another is never left half-redacted.
func redact(s string, secrets []string) string {
	secrets = slices.Clone(secrets)
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "<redacted>")
		}
	}
	return s
}

// runIPMIToolVoid runs ipmitool ignoring stdout. Convenience for the
// many setter commands (set name, set password, etc.) where success
// produces empty or trivial output and we only care about errors.
func runIPMIToolVoid(ctx context.Context, opt Options, args ...string) error {
	_, err := runIPMITool(ctx, opt, args...)
	return err
}
