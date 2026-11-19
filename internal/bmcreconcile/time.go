package bmcreconcile

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MaxClockSkew is how far a BMC clock may be from the site worker's (which
// chrony keeps on the cage's NTP) before the time pass steps it.
const MaxClockSkew = 60 * time.Second

// TimeTarget is the input of the time pass: the BMC, the admin LAN
// session (the same credential as the account reconcile), and the NTP
// servers to enforce.
type TimeTarget struct {
	// RedfishAddr is the BMC's Redfish host[:port]: the snapshot BMC IP,
	// whose port suffix exists only for the qemutest fake.
	RedfishAddr string
	// LAN is the IPMI LAN session the clock step runs over.
	LAN LANTarget
	// NTPServers are the servers to configure, in order: the ntp_servers of
	// the OOB prefix containing the BMC's IP. Must be non-empty; the caller
	// skips the pass (and reports why) when there are none.
	NTPServers []string
}

// TimeResult is the time pass's outcome.
type TimeResult struct {
	// NTPServers are the servers enforced.
	NTPServers []string
	// NTPConfigured is true when the run rewrote a non-compliant NTP
	// configuration (disabled, or other servers) and read it back
	// compliant.
	NTPConfigured bool
	// TimezoneSet is true when the run set the BMC's timezone to UTC.
	TimezoneSet bool
	// ClockStepped is true when the run stepped the BMC clock.
	ClockStepped bool
	// ClockSkew is the BMC clock minus the worker's at the start of the
	// run; 0 when it could not be read.
	ClockSkew time.Duration
	// Err is nil when the BMC's time configuration is compliant after the
	// run. It joins every failure, and ends with what to do.
	Err error
}

// ReconcileTime brings a BMC's time configuration into compliance over the
// LAN: NTP enabled with t.NTPServers as the only servers, the BMC's
// timezone UTC, and its clock within MaxClockSkew of this host's. It is
// the bmc_reconcile op's time pass and never touches power, boot order or
// accounts.
//
// Method, per per-vendor canaries run 2026-10-28:
//
//   - NTP: the standard Manager NetworkProtocol PATCH
//     {"NTP":{"ProtocolEnabled":true,"NTPServers":[...]}} on every vendor,
//     with the resource's real ETag. Supermicro accepts it that way (and
//     refuses `If-Match: *`), iDRAC9 and AMI accept it too, so the
//     Supermicro OEM NTP endpoint is not needed. A 2xx is not applied:
//     Supermicro answers 202 with a Redfish Task that applies the change
//     and can fail, leaving NTP as it was (every NTP PATCH on seven
//     X12/X13 BMCs on 01.03.31, 2026-10-29), and AMI also applies it
//     asynchronously. So the pass waits for the Task when there is one,
//     then reads the change back on two consecutive reads: while the
//     Task runs, Supermicro's first read can echo the PATCHed values.
//   - Timezone: iDRAC's Time.1.Timezone attribute ("UTC"); every other
//     BMC's standard Manager DateTimeLocalOffset ("+00:00").
//   - Clock: when the clock is more than MaxClockSkew off, disable NTP,
//     `ipmitool sel time set now`, re-enable NTP. BMCs do not poll NTP
//     right after a config change, and Supermicro ignores Set SEL Time
//     while NTP is on.
//
// The vendor family comes from the Redfish manager Id (iDRAC.Embedded.1,
// Self for AMI MegaRAC, 1 for Supermicro): it names the BMC firmware,
// which is what decides the method, while NetBox manufacturer slugs name
// the chassis vendor and are inconsistent (asus vs asustek-computer-inc).
func ReconcileTime(ctx context.Context, t TimeTarget) TimeResult {
	lan := lanSession(t.LAN)
	r := &timeReconciler{
		rf:            newRedfishClient(t.RedfishAddr, t.LAN.Username, t.LAN.Password, defaultRedfishHTTPClient()),
		stepClock:     func(ctx context.Context) error { return setSELTimeNow(ctx, lan) },
		pollInterval:  2 * time.Second,
		settleTimeout: 20 * time.Second,
	}
	return r.run(ctx, t.NTPServers)
}

// setSELTimeNow sets the BMC clock to this host's with IPMI Set SEL Time.
// `now` rather than an explicit time: ipmitool 1.8.19 rejects a four-digit
// year in one. TZ=UTC is load-bearing: ipmitool passes "now" through
// ipmi_localtime2utc, which shifts it by the host's UTC offset, so on a
// host not in UTC the BMC would be set off by that offset.
func setSELTimeNow(ctx context.Context, opt Options) error {
	if opt.IPMITool == "" {
		opt.IPMITool = "ipmitool"
	}
	opt.Env = append(slices.Clone(opt.Env), "TZ=UTC")
	if err := runIPMIToolVoid(ctx, opt, "sel", "time", "set", "now"); err != nil {
		return fmt.Errorf("set SEL time: %w", err)
	}
	return nil
}

// bmcFamily is the BMC firmware family, from the Redfish manager Id.
type bmcFamily int

const (
	familyStandard bmcFamily = iota // Supermicro ("1") and anything unrecognized
	familyDell                      // iDRAC ("iDRAC.Embedded.1")
	familyAMI                       // AMI MegaRAC: ASUS, Tyan ("Self")
)

func familyOf(managerID string) bmcFamily {
	switch {
	case strings.HasPrefix(managerID, "iDRAC"):
		return familyDell
	case managerID == "Self":
		return familyAMI
	default:
		return familyStandard
	}
}

// dellTimezoneAttr is the iDRAC attribute holding the BMC timezone.
const dellTimezoneAttr = "Time.1.Timezone"

// reenableTimeout bounds the write of re-enabling NTP after a clock step
// (a GET for the ETag, the PATCH, and one 412 retry). Its confirmation
// gets two settleTimeouts on top (the Task wait, then the read-back), in
// the same detached context, so a slow write cannot eat the window the
// BMC has to apply it.
const reenableTimeout = time.Minute

type timeReconciler struct {
	rf *redfishClient
	// stepClock sets the BMC clock to this host's; production is
	// setSELTimeNow over the LAN session.
	stepClock func(context.Context) error
	// pollInterval and settleTimeout bound each wait for an
	// asynchronously applied (202) NTP PATCH to take effect: the wait for
	// its Task, and the reads of NTP (the disable before the step, the
	// read-back after every enable).
	pollInterval  time.Duration
	settleTimeout time.Duration
}

// redfishTask is the part of a Redfish Task the time pass reads.
type redfishTask struct {
	TaskState string `json:"TaskState"`
	Messages  []struct {
		Message string `json:"Message"`
	} `json:"Messages"`
}

type redfishLink struct {
	ODataID string `json:"@odata.id"`
}

type redfishManager struct {
	ID                  string      `json:"Id"`
	DateTime            string      `json:"DateTime"`
	DateTimeLocalOffset string      `json:"DateTimeLocalOffset"`
	NetworkProtocol     redfishLink `json:"NetworkProtocol"`
}

type redfishNTP struct {
	ProtocolEnabled *bool    `json:"ProtocolEnabled"`
	NTPServers      []string `json:"NTPServers"`
}

type redfishNetworkProtocol struct {
	NTP *redfishNTP `json:"NTP"`
}

// ntpPatch is the NetworkProtocol PATCH body. NTPServers is omitted when
// only toggling ProtocolEnabled (the step's disable).
type ntpPatch struct {
	NTP struct {
		ProtocolEnabled bool     `json:"ProtocolEnabled"`
		NTPServers      []string `json:"NTPServers,omitempty"`
	} `json:"NTP"`
}

func ntpBody(enabled bool, servers []string) ntpPatch {
	var p ntpPatch
	p.NTP.ProtocolEnabled = enabled
	p.NTP.NTPServers = servers
	return p
}

func (r *timeReconciler) run(ctx context.Context, servers []string) TimeResult {
	res := TimeResult{NTPServers: servers}
	var errs []string
	finish := func() TimeResult {
		if len(errs) > 0 {
			res.Err = fmt.Errorf("bmc time compliance incomplete: %s; fix the cause and re-run `slagctl machine bmc-reconcile`",
				strings.Join(errs, "; "))
		}
		return res
	}
	if len(servers) == 0 {
		errs = append(errs, "no NTP server to configure")
		return finish()
	}

	mgrPath, err := r.managerPath(ctx)
	if err != nil {
		errs = append(errs, err.Error())
		return finish()
	}
	var mgr redfishManager
	if _, err := r.rf.get(ctx, mgrPath, &mgr); err != nil {
		errs = append(errs, "read manager: "+err.Error())
		return finish()
	}
	skew, skewErr := clockSkew(mgr.DateTime, time.Now())
	if skewErr != nil {
		errs = append(errs, skewErr.Error())
	} else {
		res.ClockSkew = skew
	}
	fam := familyOf(mgr.ID)

	// Timezone first: a BMC that keeps SEL time in local time would take
	// the step below in the wrong zone.
	tzOK, tzSet, err := r.reconcileTimezone(ctx, fam, mgrPath, mgr)
	res.TimezoneSet = tzSet
	if err != nil {
		errs = append(errs, err.Error())
	}

	npPath := mgr.NetworkProtocol.ODataID
	if npPath == "" {
		npPath = mgrPath + "/NetworkProtocol"
	}
	var np redfishNetworkProtocol
	if _, err := r.rf.get(ctx, npPath, &np); err != nil {
		errs = append(errs, "read NTP settings: "+err.Error())
		return finish()
	}
	if np.NTP == nil {
		errs = append(errs, fmt.Sprintf("the BMC's %s has no NTP property, so NTP cannot be configured over Redfish", npPath))
		return finish()
	}
	serversOK, ntpOK := ntpCompliant(np.NTP, servers)
	desired := ntpPatchServers(servers, np.NTP.NTPServers)

	switch {
	case skewErr == nil && (skew > MaxClockSkew || skew < -MaxClockSkew):
		errs = append(errs, r.step(ctx, fam, npPath, mgrPath, servers, desired, ntpOK, &res)...)
	case ntpOK:
	case fam == familyAMI && serversOK && tzOK && skewErr == nil:
		// One ASUS model reports ProtocolEnabled=false a minute after it is
		// enabled while it keeps syncing from the DHCP-supplied server.
		// The right server, a correct clock and UTC are what compliance is
		// for, so this is compliant; re-enabling would only flap again.
	default:
		if task, err := r.rf.patch(ctx, npPath, ntpBody(true, desired)); err != nil {
			errs = append(errs, "configure NTP: "+err.Error())
		} else if _, err := r.confirmNTP(ctx, npPath, task, servers, fam == familyAMI); err != nil {
			errs = append(errs, "configure NTP: "+err.Error())
		} else {
			res.NTPConfigured = true
		}
	}
	return finish()
}

// managerPath returns the first Redfish manager's path. Every BMC on the
// fleet has exactly one.
func (r *timeReconciler) managerPath(ctx context.Context) (string, error) {
	var coll struct {
		Members []redfishLink `json:"Members"`
	}
	if _, err := r.rf.get(ctx, "/redfish/v1/Managers", &coll); err != nil {
		return "", fmt.Errorf("list Redfish Managers: %w", err)
	}
	if len(coll.Members) == 0 {
		return "", fmt.Errorf("the BMC's Redfish Managers collection is empty")
	}
	p := coll.Members[0].ODataID
	if err := checkRedfishPath(p); err != nil {
		return "", fmt.Errorf("list Redfish Managers: %w", err)
	}
	return p, nil
}

// reconcileTimezone makes the BMC's timezone UTC. ok reports whether it
// is UTC after the call; set whether this call changed it.
func (r *timeReconciler) reconcileTimezone(ctx context.Context, fam bmcFamily, mgrPath string, mgr redfishManager) (ok, set bool, err error) {
	if fam == familyDell {
		path := mgrPath + "/Attributes"
		var attrs struct {
			Attributes map[string]any `json:"Attributes"`
		}
		if _, err := r.rf.get(ctx, path, &attrs); err != nil {
			return false, false, fmt.Errorf("read iDRAC %s: %w", dellTimezoneAttr, err)
		}
		tz, _ := attrs.Attributes[dellTimezoneAttr].(string)
		if tz == "UTC" {
			return true, false, nil
		}
		body := map[string]any{"Attributes": map[string]string{dellTimezoneAttr: "UTC"}}
		if _, err := r.rf.patch(ctx, path, body); err != nil {
			return false, false, fmt.Errorf("set iDRAC %s from %q to UTC: %w", dellTimezoneAttr, tz, err)
		}
		return true, true, nil
	}
	off := mgr.DateTimeLocalOffset
	if off == "" {
		// No DateTimeLocalOffset: the DateTime's own offset is the BMC's.
		if t, err := time.Parse(time.RFC3339, mgr.DateTime); err == nil {
			off = t.Format("-07:00")
		}
	}
	switch off {
	case "+00:00", "-00:00", "Z":
		return true, false, nil
	}
	if _, err := r.rf.patch(ctx, mgrPath, map[string]string{"DateTimeLocalOffset": "+00:00"}); err != nil {
		return false, false, fmt.Errorf("set DateTimeLocalOffset from %q to +00:00: %w", off, err)
	}
	return true, true, nil
}

// step sets a wrong BMC clock: NTP off, Set SEL Time, NTP back on with the
// desired servers (so a non-compliant NTP config is fixed by the same
// write). NTP is re-enabled, and read back, whenever this may have
// disabled it, even if the step failed or ctx ended, because a BMC left
// with NTP off is worse than a failed step. A disable that got no response
// may have applied, so it counts (re-enabling NTP that is on is harmless)
// and is waited for like an accepted one, but only an accepted one is
// followed by the clock step. Returns the failures.
func (r *timeReconciler) step(ctx context.Context, fam bmcFamily, npPath, mgrPath string, servers, desired []string, ntpOK bool, res *TimeResult) []string {
	var errs []string
	var mayBeDisabled bool
	if task, err := r.rf.patch(ctx, npPath, ntpBody(false, nil)); err != nil {
		errs = append(errs, "disable NTP for the clock step: "+err.Error())
		if mayBeDisabled = mayHaveApplied(err); mayBeDisabled {
			_ = r.waitNTPDisabled(ctx, npPath, "")
		}
	} else {
		mayBeDisabled = true
		if err := r.waitNTPDisabled(ctx, npPath, task); err != nil {
			errs = append(errs, "disable NTP for the clock step: the BMC accepted it but its "+err.Error()+"; stepped anyway")
		}
		if err := r.stepClock(ctx); err != nil {
			errs = append(errs, "step the clock: "+err.Error())
		} else {
			res.ClockStepped = true
		}
	}
	if mayBeDisabled || !ntpOK {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reenableTimeout+2*r.settleTimeout)
		task, err := r.rf.patch(rctx, npPath, ntpBody(true, desired))
		var last *redfishNTP
		if err == nil {
			// After this run's own disable, matching servers prove nothing
			// on AMI either: the read-back must see NTP on. The ASUS flap
			// (see run) shows a minute after an enable, past settleTimeout.
			last, err = r.confirmNTP(rctx, npPath, task, servers, fam == familyAMI && !mayBeDisabled)
		}
		cancel()
		switch {
		case err != nil && mayBeDisabled && (last == nil || !ntpEnabled(last)):
			// Unless NTP reads back enabled (on other servers), assume
			// the disable above is what the BMC is left with.
			errs = append(errs, "re-enable NTP after the clock step failed, so NTP is DISABLED on the BMC: "+err.Error())
		case err != nil:
			errs = append(errs, "configure NTP: "+err.Error())
		case !ntpOK:
			res.NTPConfigured = true
		}
	}
	if !res.ClockStepped {
		return errs
	}
	var mgr redfishManager
	if _, err := r.rf.get(ctx, mgrPath, &mgr); err != nil {
		return append(errs, "re-read the clock after the step: "+err.Error())
	}
	after, err := clockSkew(mgr.DateTime, time.Now())
	switch {
	case err != nil:
		errs = append(errs, "after the step: "+err.Error())
	case after > MaxClockSkew || after < -MaxClockSkew:
		errs = append(errs, fmt.Sprintf("the BMC accepted the clock step but is still %s off", after.Round(time.Second)))
	}
	return errs
}

// waitNTPDisabled waits for the step's NTP disable to take effect, because
// a BMC ignores Set SEL Time while NTP is on: for its Task to end when the
// PATCH returned one, then, up to settleTimeout, for NTP to read disabled
// (AMI applies the PATCH asynchronously without a Task). Waiting out the
// Task also keeps a late disable from landing after the re-enable has been
// read back, which is why step also waits, with no Task to wait for, after
// a disable that got no response and may still be applying. Best effort:
// the step runs anyway, and the post-step clock read reports a step that
// did not take. Returns the Task's failure, if any, so the caller can
// report it.
func (r *timeReconciler) waitNTPDisabled(ctx context.Context, npPath, task string) error {
	if task != "" {
		if err := r.waitTask(ctx, task); err != nil {
			return err
		}
	}
	_, _, _ = r.pollNTP(ctx, npPath, 1, func(ntp *redfishNTP) bool { return !ntpEnabled(ntp) })
	return nil
}

// confirmNTP confirms an NTP enable the BMC accepted: it waits for the
// PATCH's Task, when there is one, then reads NTP back until it reads
// compliant (ntpCompliant, or the servers alone when serversSuffice: the
// AMI flap, see run). On failure it returns the last settings read (nil
// when that read failed) and an error naming the Task's outcome and those
// settings.
func (r *timeReconciler) confirmNTP(ctx context.Context, npPath, task string, servers []string, serversSuffice bool) (*redfishNTP, error) {
	if task != "" {
		if terr := r.waitTask(ctx, task); terr != nil {
			// Two reads, reporting the second, for the same echo as below:
			// step's DISABLED alarm depends on this state.
			ntp, _, err := r.pollNTP(ctx, npPath, 2, func(*redfishNTP) bool { return true })
			if ntp == nil {
				return nil, fmt.Errorf("the BMC accepted the NTP change but its %w; its NTP settings could not be read: %v", terr, err)
			}
			return ntp, fmt.Errorf("the BMC accepted the NTP change but its %w; it still reports %s", terr, describeNTP(ntp))
		}
	}
	// Compliance must hold on two consecutive reads, pollInterval apart:
	// on nova13.pop2 (Supermicro X13, 01.03.31, 2026-10-30) the first GET
	// on the PATCH's keep-alive connection, milliseconds after it, echoed
	// the PATCHed values while the Task ran, and every read after that
	// showed NTP still disabled; the Task ended in Exception.
	ntp, ok, err := r.pollNTP(ctx, npPath, 2, func(got *redfishNTP) bool {
		serversOK, compliant := ntpCompliant(got, servers)
		return compliant || (serversSuffice && serversOK)
	})
	switch {
	case ok:
		return ntp, nil
	case err != nil:
		return nil, fmt.Errorf("the BMC accepted the NTP change but it could not be read back: %w", err)
	}
	return ntp, fmt.Errorf("the BMC accepted the NTP change but still reports %s", describeNTP(ntp))
}

func describeNTP(ntp *redfishNTP) string {
	state := "disabled"
	if ntpEnabled(ntp) {
		state = "enabled"
	}
	return fmt.Sprintf("NTP %s with servers %v", state, nonEmpty(ntp.NTPServers))
}

// waitTask polls the Redfish Task at path every pollInterval, up to
// settleTimeout, until it ends. It returns nil when the Task Completed, or
// when its outcome is unknown: it is gone (404), could not be read before
// the deadline, or ctx ended; the caller's read-back then decides.
// Otherwise the error names the state it ended in and its messages, or
// says it was still running.
func (r *timeReconciler) waitTask(ctx context.Context, path string) error {
	deadline := time.Now().Add(r.settleTimeout)
	for {
		var t redfishTask
		_, err := r.rf.get(ctx, path, &t)
		running := false
		switch {
		case isNotFound(err):
			return nil
		case err != nil:
			// Retried until the deadline.
		case t.TaskState == "Completed":
			return nil
		case t.TaskState == "Exception", t.TaskState == "Killed", t.TaskState == "Cancelled", t.TaskState == "Interrupted":
			var msgs []string
			for _, m := range t.Messages {
				if m.Message != "" {
					msgs = append(msgs, strconv.Quote(m.Message))
				}
			}
			if len(msgs) > 0 {
				return fmt.Errorf("task %s ended in %s (%s)", path, t.TaskState, strings.Join(msgs, ", "))
			}
			return fmt.Errorf("task %s ended in %s", path, t.TaskState)
		default:
			running = true
		}
		if !time.Now().Before(deadline) {
			if !running {
				return nil // unreadable to the end
			}
			return fmt.Errorf("task %s was still running after %s (TaskState %q)", path, r.settleTimeout, t.TaskState)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.pollInterval):
		}
	}
}

// pollNTP reads npPath's NTP settings every pollInterval, for up to
// settleTimeout or until ctx ends, until done has accepted `reads`
// consecutive reads; a run of accepted reads under way at the deadline may
// finish. It returns the last settings read and whether done accepted
// them; when the last read failed, ntp is nil and err says why.
func (r *timeReconciler) pollNTP(ctx context.Context, npPath string, reads int, done func(*redfishNTP) bool) (ntp *redfishNTP, ok bool, err error) {
	deadline := time.Now().Add(r.settleTimeout)
	run := 0
	for {
		var np redfishNetworkProtocol
		_, err = r.rf.get(ctx, npPath, &np)
		if err == nil && np.NTP == nil {
			err = fmt.Errorf("the BMC's %s has no NTP property", npPath)
		}
		if err == nil && done(np.NTP) {
			if run++; run >= reads {
				return np.NTP, true, nil
			}
		} else {
			run = 0
		}
		if err != nil {
			np.NTP = nil
		}
		if run == 0 && !time.Now().Before(deadline) {
			return np.NTP, false, err
		}
		select {
		case <-ctx.Done():
			return np.NTP, false, err
		case <-time.After(r.pollInterval):
		}
	}
}

// ntpCompliant is the NTP compliance rule, shared by run's up-front check
// and the read-back after an enable: serversOK when ntp names exactly
// servers, in order, ignoring the empty slots BMCs pad with; ok when it is
// also enabled.
func ntpCompliant(ntp *redfishNTP, servers []string) (serversOK, ok bool) {
	serversOK = slices.Equal(nonEmpty(ntp.NTPServers), servers)
	return serversOK, serversOK && ntpEnabled(ntp)
}

func ntpEnabled(ntp *redfishNTP) bool {
	return ntp.ProtocolEnabled != nil && *ntp.ProtocolEnabled
}

// clockSkew is the BMC clock (a Redfish DateTime, RFC 3339 with offset)
// minus now.
func clockSkew(dateTime string, now time.Time) (time.Duration, error) {
	t, err := time.Parse(time.RFC3339, dateTime)
	if err != nil {
		return 0, fmt.Errorf("the BMC's Manager DateTime %q is not RFC 3339, so its clock cannot be checked or stepped", dateTime)
	}
	return t.Sub(now), nil
}

// ntpPatchServers is the NTPServers array to PATCH: the desired servers,
// padded with "" over every later slot the BMC reports when any of those
// still names a server. BMCs report fixed-length arrays (iDRAC three
// slots, Supermicro two), a shorter PATCH array is not guaranteed to clear
// the tail, and "" is how the BMCs themselves report an empty slot.
// Without a stale tail the array is exactly the desired servers, the shape
// proven on every vendor.
func ntpPatchServers(desired, current []string) []string {
	out := slices.Clone(desired)
	if len(current) > len(desired) && len(nonEmpty(current[len(desired):])) > 0 {
		for len(out) < len(current) {
			out = append(out, "")
		}
	}
	return out
}

func nonEmpty(ss []string) []string {
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
