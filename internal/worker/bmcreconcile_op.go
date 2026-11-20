package worker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
	"github.com/bluesky-social/slagworks/internal/model"
)

// bmcReconcileTimeout bounds one remote reconcile: a list, a handful of
// writes per account (each an RMCP+ session; set-password retries sleep
// 2s), and a re-list. Minutes of headroom on a slow BMC, well inside the
// 30-minute stuck-op sweep.
const bmcReconcileTimeout = 5 * time.Minute

// bmcTimeTimeout bounds the time pass: a few Redfish reads and PATCHes
// (each up to 30s), up to 20s each waiting for an NTP PATCH's Redfish Task
// and then for NTP to read back (the clock step's disable, an enable), and
// one ipmitool call. The re-enable after a clock step runs detached and may
// outlive it by up to 100s. Together with bmcReconcileTimeout it stays well
// inside the 30-minute stuck-op sweep.
const bmcTimeTimeout = 3 * time.Minute

// runBMCReconcile executes a claimed bmc_reconcile op: bring the BMC's
// IPMI users, then its time configuration, into compliance over the LAN
// with the device's admin credential (model.OpBMCReconcile).
//
// Order: validate the op's runtime, mark execution started (a phase, so
// the orphaned-claim sweep doesn't fail a reconcile that outlives its
// 2-minute claimed-no-phase grace), run the chassis-serial identity gate
// through the same controller path as power ops, reconcile the accounts,
// then run the time pass (reconcileBMCTime). The time pass runs after the
// accounts so none of its failures can stop them, and its failures only
// ever land in the report's time fields.
//
// Monitor password: the op carries the device's stored
// pop/<pop>/bmc-creds-monitor password (MonitorRuntime), which reconcile
// re-asserts (writing it only when the BMC does not already hold it), or
// none (a new BMC, or rotate_monitor), in which case reconcile generates
// one.
//
// Reporting: a reconcile that returned no Result (it refused or failed
// before any write) fails through the ordinary op_update, and the time pass
// does not run (there is no report to carry it). Otherwise the outcome goes
// ONLY in a bmc_reconcile_report observation, with monitor_rotated,
// monitor_password_written and any generated monitor password; global
// stores a generated password in pop/<pop>/bmc-creds-monitor (beside the
// current one when its write's outcome is unknown) and finalizes
// the op with non-secret facts. A re-asserted stored password is not copied
// into the report. Neither password ever rides an op_update (persisted
// verbatim in the operations table) or a log line. An op global ended
// mid-run (opRemoved: an abort, a drift fail, a sweep) runs no further
// pass and queues no report, which global would drop anyway.
func (e *Executor) runBMCReconcile(ctx context.Context, op model.Operation) {
	log := e.log.With("op_id", op.ID, "type", op.Type, "machine_id", op.MachineID)
	fail := func(err error) {
		log.Error("bmc reconcile failed", "err", err)
		e.cache.RecordOpUpdate(op.ID.String(), string(model.OpFailed), marshalError(err))
	}

	m := e.cache.MachineByID(op.MachineID)
	if m == nil {
		fail(errors.New("machine not in cache"))
		return
	}
	params, err := model.DecodeParams(op.Params)
	if err != nil {
		fail(err)
		return
	}
	switch {
	case params.PDURuntime != nil || params.AMTRuntime != nil:
		fail(errors.New("bmc_reconcile runs over IPMI LAN only, but the op carries a pdu/amt runtime; re-issue it with `slagctl machine bmc-reconcile`"))
		return
	case params.BMCRuntime == nil || params.BMCRuntime.Username == "" || params.BMCRuntime.Password == "":
		fail(errors.New("bmc_reconcile op carries no BMC admin credential (bmc_runtime); global must resolve the device's pop/<pop>/bmc-creds-admin entry when creating the op"))
		return
	case params.MonitorRuntime != nil && (params.RotateMonitor || params.MonitorRuntime.Password == ""):
		// Either would rotate when the op says to re-assert, or the
		// reverse; refuse rather than guess which the operator meant.
		fail(errors.New("bmc_reconcile op carries an unusable stored monitor password (empty, or alongside rotate_monitor); " +
			"re-issue it with `slagctl machine bmc-reconcile`"))
		return
	case m.BMCIP == "":
		fail(errors.New("machine has no BMC IP (NetBox oob_ip); set the device's oob_ip in NetBox"))
		return
	}
	rt := *params.BMCRuntime
	monitorPassword := ""
	var unconfirmed []string
	if params.MonitorRuntime != nil {
		monitorPassword = params.MonitorRuntime.Password
		for _, u := range params.MonitorRuntime.Unconfirmed {
			unconfirmed = append(unconfirmed, u.Password)
		}
	}

	e.cache.UpdateOpPhase(op.ID.String(), "bmc_reconcile")

	// Identity gate, same as power ops: the BMC's FRU chassis serial must
	// match NetBox before any write. openController fails the op on error.
	ctrl, ok := e.openController(ctx, log, op, m)
	if !ok {
		return
	}
	_ = ctrl.Close(ctx)
	if e.opRemoved(log, op, "bmc user reconcile") {
		return
	}

	lan := bmcreconcile.LANTarget{
		Host:     bmcHostOnly(m.BMCIP),
		Username: rt.Username,
		Password: rt.Password,
		Cipher:   rt.IpmiCipher(),
	}
	rctx, cancel := context.WithTimeout(ctx, bmcReconcileTimeout)
	defer cancel()
	res, err := e.bmcReconcile(rctx, lan, monitorPassword, unconfirmed)
	if res == nil {
		if err == nil {
			err = errors.New("bmc reconcile returned no result")
		}
		fail(err)
		return
	}

	rep := model.BMCReconcileReport{LeftoversDisabled: res.LeftoversDisabled}
	for _, u := range res.CreatedOrUpdated {
		if u.Name == bmcreconcile.MonitorUser {
			// u.Password is set only when reconcile generated it; a
			// re-asserted stored password stays out of the report. A
			// generated password the BMC may or may not have applied
			// (u.PasswordUnconfirmed, always with err) is reported too,
			// flagged: this report is its only copy, and global keeps it
			// beside the stored password for the next op to settle.
			rep.Monitor = &model.BMCUserCred{
				Username: u.Name, Password: u.Password, IPMIUserSlot: u.Slot,
				PasswordUnconfirmed: u.PasswordUnconfirmed,
			}
			rep.MonitorRotated = u.Password != "" && !u.PasswordUnconfirmed
			rep.MonitorPasswordWritten = !u.PasswordUnconfirmed
		}
	}
	for _, u := range res.PasswordVerified {
		if u.Name == bmcreconcile.MonitorUser {
			// Nothing written. u.Password is empty when the BMC held the
			// stored password, and otherwise the unconfirmed password it
			// held instead, which global must now store as the password.
			rep.Monitor = &model.BMCUserCred{Username: u.Name, Password: u.Password, IPMIUserSlot: u.Slot}
			rep.MonitorRotated = u.Password != ""
		}
	}
	if err != nil {
		rep.Error = err.Error()
	}
	if e.opRemoved(log, op, "bmc time pass") {
		// Global drops a report for an op that is no longer active
		// without storing its password, so none is queued.
		if rep.Monitor != nil && rep.Monitor.Password != "" {
			log.Warn("the BMC's monitor account may now hold a generated password that was not stored, because the op ended during the reconcile; " +
				"re-run `slagctl machine bmc-reconcile` to converge it")
		}
		return
	}
	rep.BMCTimeCompliance = e.reconcileBMCTime(ctx, m.BMCIP, lan)
	if rerr := e.cache.recordBMCReconcileReport(op.MachineID, op.ID, rep); rerr != nil {
		fail(fmt.Errorf("queue bmc_reconcile_report: %w", rerr))
		return
	}
	e.cache.MarkOpStatus(op.ID.String(), model.OpRunning)
	log.Info("bmc reconcile done; report queued for global",
		"monitor_reported", rep.Monitor != nil,
		"monitor_rotated", rep.MonitorRotated,
		"monitor_password_written", rep.MonitorPasswordWritten,
		"leftovers_disabled", rep.LeftoversDisabled,
		"names_not_cleared", res.NamesNotCleared,
		"error", rep.Error,
		"ntp_servers", rep.NTPServers,
		"ntp_configured", rep.NTPConfigured,
		"timezone_set", rep.TimezoneSet,
		"clock_stepped", rep.ClockStepped,
		"clock_skew_seconds", rep.ClockSkewSeconds)
	if rep.TimeError != "" {
		log.Warn("bmc time compliance incomplete", "time_error", rep.TimeError)
	}
}

// reconcileBMCTime runs the time pass with the NTP servers of the OOB
// prefix containing the BMC's IP, or reports why it was skipped.
func (e *Executor) reconcileBMCTime(ctx context.Context, bmcIP string, lan bmcreconcile.LANTarget) model.BMCTimeCompliance {
	servers, skip := bmcNTPServers(e.cache.GetOOBConfig(), lan.Host)
	if skip != "" {
		return model.BMCTimeCompliance{TimeError: skip}
	}
	tctx, cancel := context.WithTimeout(ctx, bmcTimeTimeout)
	defer cancel()
	tr := e.bmcTime(tctx, bmcreconcile.TimeTarget{RedfishAddr: bmcIP, LAN: lan, NTPServers: servers})
	tc := model.BMCTimeCompliance{
		NTPServers:       tr.NTPServers,
		NTPConfigured:    tr.NTPConfigured,
		TimezoneSet:      tr.TimezoneSet,
		ClockStepped:     tr.ClockStepped,
		ClockSkewSeconds: int64(tr.ClockSkew.Round(time.Second) / time.Second),
	}
	if tr.Err != nil {
		tc.TimeError = tr.Err.Error()
	}
	return tc
}

// bmcNTPServers returns the NTP servers a BMC must use: the ntp_servers of
// the site's slagworks-oob prefix, when that prefix contains the BMC's IP.
// That is the same NetBox field the site worker advertises to the BMC as
// DHCP option 42, so the static NTP config and the DHCP-supplied one
// agree. Otherwise it returns why there is none; there is no default.
func bmcNTPServers(oob model.OOBConfig, bmcHost string) ([]string, string) {
	const skipped = "; NTP, timezone and clock compliance skipped"
	if oob.Subnet == "" {
		return nil, "this site has no slagworks-oob prefix in NetBox, so the BMC has no ntp_servers" + skipped
	}
	_, subnet, err := net.ParseCIDR(oob.Subnet)
	if err != nil {
		return nil, fmt.Sprintf("the site's slagworks-oob prefix %q is not a CIDR%s", oob.Subnet, skipped)
	}
	if ip := net.ParseIP(bmcHost); ip == nil || !subnet.Contains(ip) {
		return nil, fmt.Sprintf("the BMC's IP %s is outside the site's slagworks-oob prefix %s, so it has no ntp_servers%s; "+
			"check the device's oob_ip in NetBox", bmcHost, oob.Subnet, skipped)
	}
	if len(oob.NTP) == 0 {
		return nil, fmt.Sprintf("the slagworks-oob prefix %s has no ntp_servers custom field%s; "+
			"set it in NetBox to the cage's NTP server (the OOB VIP) and re-run `slagctl machine bmc-reconcile`", oob.Subnet, skipped)
	}
	return oob.NTP, ""
}

// bmcHostOnly strips a ":port" suffix from a snapshot BMC IP. The suffix
// only exists for the qemutest Redfish fake and is meaningless to
// ipmitool's -H.
func bmcHostOnly(bmcIP string) string {
	if h, _, err := net.SplitHostPort(bmcIP); err == nil {
		return h
	}
	return bmcIP
}
