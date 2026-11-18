package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/agent/inventory"
	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
	"github.com/bluesky-social/slagworks/internal/model"
)

// oobIsAMT reports whether the boot cmdline marks this host's OOB endpoint
// as Intel AMT (slagworks.oob=amt, emitted by the site worker's boot
// handler for a machine whose bmc-creds-admin entry is kind=amt). Every
// IPMI-shaped agent step (commission bmc_ipmitool, deploy bmc_reconcile)
// is skipped then: there is no IPMI BMC, and an in-band ipmitool must not
// rotate accounts or report data for a BMC that isn't there.
func oobIsAMT() bool { return kernelArgValue("slagworks.oob") == "amt" }

// inventoryOptions derives the inventory collection options from the boot
// cmdline.
func inventoryOptions() inventory.Options {
	return inventory.Options{SkipBMC: oobIsAMT()}
}

// runBMCReconcile drives the agent's BMC reconcile phase: load the
// `ipmi_devintf` kernel module so /dev/ipmi0 appears, run reconcile
// against the BMC, and produce a BMCCreds payload for slagworksd.
//
// The only account provisioned is `monitor` (USER, the ipmi_exporter
// scrape cred). slagworks drives BMCs with the slot-2 bmc-creds-admin
// account and provisions no account of its own, so a pre-existing
// `slagworks` account from an older deploy is disabled and cleared by
// the leftover pass like any other stray account.
//
// That leftover pass would also disable the bmc-creds-admin account if it
// weren't at slot 2, locking slagworks out of the BMC. The deploy cmdline
// carries that account's name (slagworks.bmc_admin_user), and reconcile
// refuses before any write unless it is the slot-2 account.
//
// Returns:
//   - non-nil creds + nil err when a monitor password was written.
//   - nil creds + nil err when monitor was already provisioned.
//   - non-nil creds + non-nil err on a partial failure after a
//     password was written (the caller must still phone it home).
//   - nil creds + non-nil err on a failure before any write (including
//     the slot-2 gate's refusal).
func runBMCReconcile(actx *agent.Context) (*model.BMCCreds, error) {
	actx.Log.Info("BMC reconcile starting")

	opt, err := inBandReconcileOptions(actx.Log, kernelArgValue(bmcreconcile.AdminUserKernelArg))
	if err != nil {
		return nil, err
	}
	if err := loadIPMIDevIntf(); err != nil {
		return nil, fmt.Errorf("load ipmi_devintf: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return reconcileInBand(ctx, actx.Log, opt)
}

// inBandReconcileOptions builds the in-band reconcile options from the
// slagworks.bmc_admin_user cmdline value ("" when the arg is absent: the
// device has no bmc-creds-admin entry, so there is no account to guard and
// the gate is skipped, loudly). A malformed value is an error: fail closed
// rather than reconcile ungated.
func inBandReconcileOptions(log *slog.Logger, argValue string) (bmcreconcile.Options, error) {
	opt := bmcreconcile.Options{
		IPMIInterface: "open",
		Desired: []bmcreconcile.DesiredUser{
			{Name: bmcreconcile.MonitorUser, Privilege: bmcreconcile.PrivUser},
		},
	}
	if argValue == "" {
		log.Warn("BMC reconcile: no " + bmcreconcile.AdminUserKernelArg + " on the kernel cmdline (the device has no " +
			"bmc-creds-admin entry); slot-2 admin gate skipped")
		return opt, nil
	}
	user, err := bmcreconcile.DecodeAdminUserArg(argValue)
	if err != nil {
		return opt, fmt.Errorf("refusing BMC reconcile: %w", err)
	}
	opt.AdminUser = user
	return opt, nil
}

// reconcileInBand runs the reconcile and maps its result to the
// phone-home payload (see runBMCReconcile for the return contract).
func reconcileInBand(ctx context.Context, log *slog.Logger, opt bmcreconcile.Options) (*model.BMCCreds, error) {
	res, err := bmcreconcile.Reconcile(ctx, opt)
	if err != nil {
		var gate *bmcreconcile.AdminNotSlot2Error
		if errors.As(err, &gate) {
			log.Warn("BMC reconcile refused before any write: the bmc-creds-admin account is not the slot-2 account "+
				"(admin_slot 0 = not on the BMC); point the device's bmc-creds-admin entry at the slot-2 account",
				"admin_user", gate.User, "admin_slot", gate.Slot, "slot2_user", gate.Slot2User)
		}
		// A partial failure may still have WRITTEN passwords to the BMC
		// (Reconcile returns whatever it provisioned before erroring).
		// Those live creds must reach phone-home even on the soft-fail
		// path, or the BMC silently drifts from slagworksd's stored
		// creds. Build the creds payload from the partial result and
		// return it alongside the error; the caller decides soft/hard.
		if res != nil && len(res.CreatedOrUpdated) > 0 {
			return credsFromResult(res), fmt.Errorf("reconcile (partial, %d cred(s) written): %w",
				len(res.CreatedOrUpdated), err)
		}
		return nil, fmt.Errorf("reconcile: %w", err)
	}

	log.Info("BMC reconcile complete",
		"admin_user_gated", opt.AdminUser != "",
		"provisioned", len(res.CreatedOrUpdated),
		"leftovers_disabled", len(res.LeftoversDisabled),
		"names_not_cleared", res.NamesNotCleared,
		"boot_order_persisted", res.BootOrderPersisted)

	// Subsequent-deploy happy path: monitor was already provisioned
	// on a prior deploy; reconcile wrote no password (only the
	// idempotent boot-order write and any leftover disables). There is
	// no new password to phone home, so the agent reports
	// deploy_complete with a nil BMCCreds — global keeps the stored
	// monitor entry untouched.
	if len(res.CreatedOrUpdated) == 0 {
		return nil, nil
	}

	return credsFromResult(res), nil
}

// credsFromResult maps a reconcile Result's provisioned monitor user
// into the phone-home creds payload. Used by both the success path and
// the partial-failure path (where the password was written before the
// error and must still reach slagworksd).
func credsFromResult(res *bmcreconcile.Result) *model.BMCCreds {
	creds := &model.BMCCreds{
		BootOrderPersisted: res.BootOrderPersisted,
		LeftoversDisabled:  res.LeftoversDisabled,
	}
	for _, u := range res.CreatedOrUpdated {
		if u.Name == bmcreconcile.MonitorUser {
			creds.Monitor = &model.BMCUserCred{
				Username: u.Name, Password: u.Password, IPMIUserSlot: u.Slot,
			}
		}
	}
	return creds
}

// loadIPMIDevIntf makes sure /dev/ipmi0 exists by modprobing
// ipmi_devintf (the userland char-device interface, which isn't
// always auto-loaded). Returns an error if /dev/ipmi0 doesn't appear
// after — meaning either no IPMI hardware is present or the right
// backend never bound.
//
// We also try ipmi_si as a belt-and-suspenders measure for hosts
// where the kernel's boot-time autoload didn't fire on a KCS/SMIC/BT
// controller. Best-effort: many systems use ipmi_ssif (SMBus) or
// other backends, in which case modprobing ipmi_si returns ENODEV
// and that's fine — the actual backend is already loaded.
func loadIPMIDevIntf() error {
	// Best-effort: kernel may have auto-loaded ipmi_si at boot if a
	// KCS/SMIC/BT controller is present. If not (or if the system uses
	// a different backend), modprobe fails with ENODEV and we ignore
	// it. The dev-node existence check below catches a missing
	// backend regardless.
	_ = exec.Command("modprobe", "ipmi_si").Run()

	cmd := exec.Command("modprobe", "ipmi_devintf")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("modprobe ipmi_devintf: %w (output: %s)", err, string(out))
	}

	// Wait for /dev/ipmi0 to appear. modprobe returns once the
	// module is loaded and the uevent is fired, but udev creates
	// the char-device node asynchronously — checking immediately
	// can race and ENOENT spuriously. Poll 10x100ms (1s budget);
	// in practice udev settles in <50ms on every system we've seen.
	const pollAttempts = 10
	const pollInterval = 100 * time.Millisecond
	for range pollAttempts {
		if _, err := os.Stat("/dev/ipmi0"); err == nil {
			return nil
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("/dev/ipmi0 not present after modprobe (kernel IPMI backend missing or no IPMI hardware)")
}
