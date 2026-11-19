//go:build integration

package qemutest

import "time"

// Budgets are the per-marker wait budgets for SerialMonitor.WaitFor
// and FakeHub.WaitForObservation. Derived from observed serial-run
// timings on the KVM runner (originally cpu2-pop3; now cpu3-pop3; PR #27 / commits 26417435910 + 26418236085 +
// 26418903547) and set at ~3× the maximum observed value, rounded
// up to a clean number, with a 30s floor for sub-second markers
// (otherwise an unrelated goroutine stall would falsely time out).
//
// The point of these budgets is FAST FEEDBACK on wedges: previously
// the matrix used 18min for deploy_complete and ate 18min on every
// star-wedge. With 3min30s here, a wedge fails 5× faster.
//
// Outer test ctx (`context.WithTimeout(ctx, 10*time.Minute)` etc.)
// stays as a backstop, but the per-marker budgets here are the
// primary timeout. If a budget needs loosening, do it here at the
// table — don't sprinkle ad-hoc time.Minute literals back into
// call sites.
//
// Bumping a single budget? Note WHY in the comment:
//   - is the slow case a real-but-rare path we want to tolerate?
//     (loosen + log)
//   - or is the slow case a regression hiding behind generous
//     limits? (don't loosen, fix the code)
type budgetsT struct {
	// CommissionComplete: end-to-end commission run from PXE through
	// `slagworks: commission complete` serial marker. Observed
	// p50 ~39s, max 41.3s across 8 hardware variants.
	CommissionComplete time.Duration

	// DeployComplete: end-to-end deploy run from PXE through
	// `slagworks: deploy complete` serial marker. RAID1 cells take
	// longer (75-81s observed at -parallel 32) than single-disk
	// (~54-63s); budget is sized for the RAID1 case under full
	// suite parallelism.
	DeployComplete time.Duration

	// FATALMarker: agent FATAL paths that abort during deploy
	// (safety check, identity check, missing-efivarfs). Observed
	// 24-60s depending on which phase fails.
	FATALMarker time.Duration

	// PhoneHomeObs: post-install reboot + first-boot Ubuntu init +
	// agent phone-home POST. Observed p50 ~10s after the
	// fixes (netplan + apt-daily mask). Pre-fix this was wedging at
	// >2 parallel cells.
	PhoneHomeObs time.Duration

	// DHCPNoBootableDevice: serial marker for "No bootable device"
	// when the DHCP server intentionally refuses to PXE. Observed
	// ~22s (full PXE retry budget).
	DHCPNoBootableDevice time.Duration

	// HubObservation: a hub observation that the test EXPECTS to
	// be already-recorded by the time it calls WaitForObservation
	// (the common case — the agent posts it during deploy). The
	// observed timing is 0ms because the observation is already
	// buffered when the wait starts. The 30s budget covers
	// scheduler stalls, not actual observation latency.
	HubObservation time.Duration

	// RecoveryMarkerLong: long recovery markers (cmdline-branch
	// boot through to RECOVERY_MODE, FATAL_AT honoring). Observed
	// 24-60s.
	RecoveryMarkerLong time.Duration

	// RecoveryMarkerShort: short recovery markers (banner-IP line,
	// "agent exited with code"). Observed sub-second; 30s floor
	// covers scheduler stalls.
	RecoveryMarkerShort time.Duration

	// BMCOpUpdate: time for the executor to run a BMC power op and the
	// terminal op_update to traverse worker -> FakeHub. The actuator
	// round-trip over real bmclib -> TLS FakeBMC -> QMP is sub-second; the
	// budget is dominated by executor claim + QMP settle, 60s is generous.
	BMCOpUpdate time.Duration

	// BMCPowerCycleMarker: time from ForceRestart (QMP system_reset) to a
	// fresh post-reboot firmware marker on the serial. Fast (SeaBIOS banner
	// in <1s); 30s floor covers scheduler stalls.
	BMCPowerCycleMarker time.Duration

	// PXEBootMarker: time from PowerOn (QMP cont) to the real iPXE
	// chainload revealing /boot/ipxe on the serial. Covers the
	// PAUSED boot -> DHCP -> TFTP -> HTTP boot-script fetch, a few
	// seconds; generous floor for the full PXE stack.
	PXEBootMarker time.Duration

	// KernelStartOneCPU: time from QEMU start to the netbooted kernel's
	// first line ("Linux version") for a UEFI VM confined to one host
	// CPU: firmware, DHCP, TFTP of iPXE, then the kernel and initrd over
	// HTTP. Observed 13.7-13.9s on an idle KVM host and 19-23s with
	// eight such VMs sharing two CPUs.
	KernelStartOneCPU time.Duration
}

// Budgets is the global table; tests reference it directly.
var Budgets = budgetsT{
	CommissionComplete: 2*time.Minute + 30*time.Second,
	// Resolved 2026-07-04: the "blows 3m30s at -parallel 32"
	// failures were both on June 18-19 — the exact merge window of
	// the initrd-upstream-steal rewrite (cefdbd9). Post-rewrite, six
	// consecutive green full-suite runs at -parallel 32 (July 3-4)
	// show deploy_complete worst-case 75-81s (bios-raid1-clean); the
	// under-load slowdown does not reproduce on the stock-initramfs
	// boot path. 3m is ~2.2x the observed worst case at full
	// parallelism, restoring the fast wedge detection the temporary
	// 6m bump traded away.
	DeployComplete: 3 * time.Minute,
	FATALMarker:    3 * time.Minute,
	// PhoneHomeObs spans the post-install reboot (firmware ->
	// boot-local -> deployed-OS boot -> phone-home POST). Observed
	// 10-27s at -parallel 32; 2m tolerates a slow POST without
	// hiding a wedge for the outer context's tens of minutes.
	PhoneHomeObs:         2 * time.Minute,
	DHCPNoBootableDevice: 90 * time.Second,
	HubObservation:       30 * time.Second,
	RecoveryMarkerLong:   3 * time.Minute,
	RecoveryMarkerShort:  30 * time.Second,
	BMCOpUpdate:          60 * time.Second,
	BMCPowerCycleMarker:  30 * time.Second,
	PXEBootMarker:        90 * time.Second,
	// A hosted CI runner is several times slower than the hosts the
	// observations come from, and the VM gets a single CPU of it.
	KernelStartOneCPU: 2 * time.Minute,
}
