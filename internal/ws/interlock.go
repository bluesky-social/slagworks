package ws

import (
	"encoding/json"
	"fmt"

	"github.com/bluesky-social/slagworks/internal/model"
)

// isOutletOp reports whether a params blob is explicitly marked as a P4b
// outlet op (pdu_runtime.outlet == true). It probes ONLY that marker so a
// params blob that won't fully decode (malformed / legacy typed block on an
// unrelated op) is classified as NOT an outlet op — the interlock's
// applicability boundary is "explicitly an outlet op", never "params happen to
// be undecodable". A confirmed outlet op is then strictly re-decoded
// fail-closed by outletClaimEligibility. An unparseable blob here yields false
// (not an outlet op): the normal executor machinery handles it as before.
func isOutletOp(raw json.RawMessage) bool {
	var probe struct {
		PDURuntime *struct {
			Outlet bool `json:"outlet"`
		} `json:"pdu_runtime"`
	}
	if len(raw) == 0 {
		return false
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	return probe.PDURuntime != nil && probe.PDURuntime.Outlet
}

// outletClaimEligibility is the P4b self-power interlock: a global
// claim-arbitration eligibility gate that runs in handleClaimOp BEFORE the
// store CAS and holds NO db lock. It decides "may THIS worker serve THIS
// outlet op"; the CAS remains the sole concurrency authority (who won the
// race). See docs/plans/2026-07-11-pdu-outlet-cycle-p4b-interlock.md.
//
// Applicability is by the EXPLICIT marker PDURuntime.Outlet == true, never by
// DownstreamSerial being non-empty: a missing serial on an outlet op is an
// eligibility FAILURE, not "not an outlet op" (fail-closed). Non-outlet ops
// (BMC power, deploy, commission, machine --via-pdu) are outside the interlock
// by kind and are always eligible here.
//
// verifiedSerial is the claiming worker's hardware serial (P4a, "" =
// unverified). registered is the persisted registered-worker-serial set for
// the slug (connected OR not), used only for INV-1. Both carry the workers'
// SMBIOS spelling while DownstreamSerial carries NetBox's, and the hello gate
// matched them case-insensitively, so every comparison here uses
// model.SameHardwareSerial: exact equality would leave the interlock inert for
// a worker whose board and NetBox record differ only in case.
//
// Returns (eligible, reason). reason is a log-only diagnostic string when
// eligible is false. A declined claim is NOT an error: the caller returns the
// same already-claimed response a lost race produces, so the worker moves on
// and the op stays pending for a partner (INV-3 decline-not-handoff).
// Callers MUST have already confirmed op is an outlet op (isOutletOp) before
// calling; this function strictly re-decodes and fails closed on any anomaly.
func outletClaimEligibility(op *model.Operation, verifiedSerial string, registered map[string]bool) (bool, string) {
	params, err := op.DecodeParams()
	if err != nil {
		// The applicability probe already classified this as an outlet op, so a
		// strict-decode failure here is a corrupt outlet op — fail closed.
		return false, fmt.Sprintf("decode outlet op params failed (fail-closed): %v", err)
	}
	rt := params.PDURuntime
	// Defensive: the caller confirmed the outlet marker via isOutletOp; if the
	// strict decode disagrees (should be impossible), fail closed rather than
	// treat it as a non-outlet op.
	if rt == nil || !rt.Outlet {
		return false, "outlet marker vanished on strict decode (fail-closed)"
	}

	// The op is an outlet op; every failure below is fail-closed.

	// Invalid outlet op: an outlet op with no resolved downstream serial can't
	// be self-power-checked (the interlock can't identify what it cuts). This
	// should have been rejected at creation; decline defensively.
	if rt.DownstreamSerial == "" {
		return false, "outlet op has empty downstream_serial (invalid; fail-closed decline)"
	}

	// INV empty-serial ineligibility: an unverified worker can't be proven NOT
	// to be the downstream device, so it must not serve an outlet op.
	if verifiedSerial == "" {
		return false, "claiming worker has no verified hardware serial; ineligible for outlet ops (fail-closed)"
	}

	// INV-1 (no serving-worker off): `off` whose downstream is a registered
	// site worker (connected or not) is refused for EVERY claimant, never
	// overridable. This keys on the DOWNSTREAM device being a serving worker,
	// not on who claims.
	if op.Type == model.OpPowerOff && containsSerial(registered, rt.DownstreamSerial) {
		return false, fmt.Sprintf("refusing outlet off: downstream serial %q is a registered site worker (INV-1: no serving-worker-off path)", rt.DownstreamSerial)
	}

	// INV-2 (self-feed): the claiming worker IS the downstream device — it is
	// ineligible to serve (cutting its own feed), EXCEPT cycle/on carrying the
	// explicit AllowSelf override (never off, which INV-1 already refused when
	// the downstream is a registered worker).
	if model.SameHardwareSerial(rt.DownstreamSerial, verifiedSerial) {
		if rt.AllowSelf && (op.Type == model.OpPowerCycle || op.Type == model.OpPowerOn) {
			return true, ""
		}
		return false, fmt.Sprintf("self-feed: claiming worker serial %q is downstream %q (INV-2 decline; a partner claims it)", verifiedSerial, rt.DownstreamSerial)
	}

	return true, ""
}

// containsSerial reports whether set holds a serial that names the same
// device as serial (model.SameHardwareSerial).
func containsSerial(set map[string]bool, serial string) bool {
	for s := range set {
		if model.SameHardwareSerial(s, serial) {
			return true
		}
	}
	return false
}
