package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// OpParams is the typed union of everything that can ride in an
// operation's params blob (Operation.Params, stored as jsonb and pushed
// inside the WS SiteSnapshot). Every field is omitempty so the on-wire
// shape is byte-identical to the ad-hoc maps/structs it replaces: an op
// only renders the keys its type actually sets.
//
// The blob crosses several boundaries with no shared compile-time type
// before this struct existed: the DB row, the WS snapshot, the httpboot
// kernel-cmdline derivation, and slagctl display. Typing it in one place
// closes the silent-drop-at-a-marshal-boundary hazard (a producer and a
// consumer that disagreed on a key name lost data with no signal). The
// wire-shape test (TestOpParamsWireKeys) pins the full key set.
//
// Field groups by op type:
//   - deploy: Destructive, WipeAllDisks, Recovery (+ test-only FatalAt,
//     Efivarfs), plus TailscaleRuntime enrollment.
//   - power (on/off/cycle): BootDevice.
//   - set_boot_device: Device, Persistent.
//   - bmc_reconcile: RotateMonitor, MonitorRuntime (the stored monitor
//     password, resolved at op-creation on global).
//   - all BMC-actuating ops: BMCRuntime (creds + serial gate + quirks),
//     resolved at op-creation on global.
//   - `--via pdu` ops: PDURuntime (outlet set + apc creds); its presence
//     is the transport discriminator the executor routes on.
//   - `--via amt` ops (explicit, or auto-selected for a machine whose
//     admin-secret entry is kind=amt): AMTRuntime (endpoint + digest
//     creds); like PDURuntime, its presence routes the executor.
//
// Every credential field anywhere under OpParams carries secret:"true".
// That tag is the one definition of what operator-facing responses redact
// (RedactParams) and what the operations table drops once an op is
// terminal (store.ScrubTerminalOperationCredentials);
// TestOpParamsCredentialFieldsAreMarked fails on an unmarked
// credential-looking field. Both match keys exactly, so DecodeParams
// refuses a credential key spelled in any other letter case.
type OpParams struct {
	// Deploy flags.
	Destructive  bool `json:"destructive,omitempty"`
	WipeAllDisks bool `json:"wipe_all_disks,omitempty"`
	Recovery     bool `json:"recovery,omitempty"`

	// FatalAt and Efivarfs are TEST-ONLY deploy knobs wired to the
	// agent's SLAGWORKS_FATAL_AT env var and the BOOT hook's
	// slagworks.efivarfs= cmdline knob respectively. Set only by the
	// qemutest fixtures; never emitted by slagctl. They are not yet gated
	// on SLAGWORKS_TEST=1, so a stray fixture blob could re-arm the
	// pop1 efivarfs ship-blocker in production.
	FatalAt  string `json:"fatal_at,omitempty"`
	Efivarfs string `json:"efivarfs,omitempty"`

	// DataMarker is a TEST-ONLY deploy knob wired to the agent's
	// slagworks.data_marker= cmdline token. When set, the deploy installs a
	// first-boot systemd unit in the target that writes the token into
	// /data/PRESERVE_MARKER (only if absent) and echoes the marker's content
	// to the serial console on every boot. The preserve-data qemu test uses
	// it to prove a data array survives a redeploy; production never sets it
	// (same gating concern as FatalAt/Efivarfs above).
	DataMarker string `json:"data_marker,omitempty"`

	// BootDevice is the one-time boot override for a power op
	// (on/off/cycle): "pxe" or "disk". Distinct from Device below.
	BootDevice string `json:"boot_device,omitempty"`

	// Device + Persistent belong to a set_boot_device op: Device is
	// "pxe"/"disk"/"default", Persistent whether the override survives a
	// power cycle.
	Device     string `json:"device,omitempty"`
	Persistent bool   `json:"persistent,omitempty"`

	// RotateMonitor marks a bmc_reconcile op the operator asked to rotate
	// the monitor password (`slagctl machine bmc-reconcile --rotate-monitor`):
	// the worker writes a freshly generated password, and global stores it,
	// instead of re-asserting the stored one. Such an op never carries
	// MonitorRuntime.
	RotateMonitor bool `json:"rotate_monitor,omitempty"`

	// Transport / enrollment envelopes, resolved on global at
	// op-creation time. Pointers so an absent block omits its key
	// entirely (matching the pre-typing wire shape, where the merge only
	// stamped the block it resolved).
	BMCRuntime       *BMCRuntime       `json:"bmc_runtime,omitempty"`
	PDURuntime       *PDURuntime       `json:"pdu_runtime,omitempty"`
	AMTRuntime       *AMTRuntime       `json:"amt_runtime,omitempty"`
	TailscaleRuntime *TailscaleRuntime `json:"tailscale_runtime,omitempty"`
	MonitorRuntime   *MonitorRuntime   `json:"monitor_runtime,omitempty"`
}

// MonitorRuntime carries a bmc_reconcile op's stored monitor credential:
// the password in the device's pop/<pop>/bmc-creds-monitor entry (keyed by
// NetBox serial), resolved on global at op-creation like BMCRuntime's admin
// cred and handled the same way: it rides only the op params to the site
// worker, which writes it to the BMC's monitor account instead of
// generating one. It never enters a log line, an op result or the worker's
// report. Absent when the store has no entry for the device (a new BMC) or
// the op rotates (RotateMonitor); the worker then generates a password.
type MonitorRuntime struct {
	Password string `json:"password" secret:"true"`
}

// BMCRuntime is the per-op block carrying the BMC credential, the
// expected chassis serial (mismatch => the site worker aborts the op),
// and vendor quirks. Resolved on global at op-creation time (NetBox for
// the serial, AWS Secrets Manager / the Machine row for creds). It also
// travels standalone in the ws power_status / boot_status RPCs.
//
// ExpectedChassisSerial and ExtraConfig deliberately lack omitempty:
// the pre-typing wire shape always rendered both keys (ExtraConfig
// defaults to a non-nil empty map on the producer), and consumers rely
// on the keys existing.
type BMCRuntime struct {
	ExpectedChassisSerial string         `json:"expected_chassis_serial"`
	Username              string         `json:"username,omitempty"`
	Password              string         `json:"password,omitempty" secret:"true"`
	ExtraConfig           map[string]any `json:"extra_config"`
}

// IpmiCipher returns the suite ID configured in extra_config, or 0 if
// none is set. JSON numbers unmarshal as float64; we coerce to int. 0 is
// the "use bmclib/ipmitool default" sentinel (suite 0 is "auto", suite
// 17 the production default).
func (r BMCRuntime) IpmiCipher() int {
	if r.ExtraConfig == nil {
		return 0
	}
	v, ok := r.ExtraConfig["ipmi_cipher"]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}

// PDURuntime is the op envelope for PDU-based power control, present
// ONLY when the operator passed `--via pdu`. Its presence is the signal
// the site-worker executor uses to route to a PDU controller instead of
// a BMC controller. An empty BMCIP does not implicitly select PDU; explicit
// --via remains the transport discriminator. See docs/dev/pdu-power-control.md.
type PDURuntime struct {
	Outlets []PDUOutlet `json:"outlets"`
	// DownstreamSerial is the NetBox serial of the device powered by the
	// outlet(s) in this envelope. Stamped at op-creation by the PDU
	// outlet endpoints (P2). Consumed by the P4 self-power interlock in
	// global claim-arbitration to prevent a site worker from cutting its
	// own power feed. Empty for ops created via the machine power path
	// (those target the load's own PSU feeds; downstream = the machine
	// itself, which the executor already knows).
	DownstreamSerial string `json:"downstream_serial,omitempty"`
	// Outlet marks this envelope as originating from the operator-facing
	// PDU-outlet endpoints (`slagctl power outlet`), which is the EXPLICIT
	// applicability marker for the P4b self-power interlock. The interlock
	// classifies an op as an outlet op by Outlet==true, NOT by
	// DownstreamSerial being non-empty: a missing serial on an outlet op is
	// an eligibility FAILURE (rejected at creation, declined fail-closed at
	// claim), never a bypass. Machine `--via pdu` ops (resolvePDURuntime)
	// leave this false — their downstream IS the anchored machine, so they
	// are outside the outlet interlock by kind.
	Outlet bool `json:"outlet,omitempty"`
	// AllowSelf is the INV-2 operator override: it lets a single-worker site
	// deliberately CYCLE/ON its own feed (the claiming worker == the
	// downstream device) even though that briefly drops the worker itself.
	// Set only by the outlet endpoints from an explicit `--allow-self` flag,
	// and honored ONLY for cycle/on — it NEVER applies to `off` (INV-1: no
	// serving-worker-off path in this command). Consumed by the claim-time
	// interlock and by the worker-side defense-in-depth skip.
	AllowSelf bool `json:"allow_self,omitempty"`
}

// AMTRuntime is the op envelope for Intel AMT power/boot control,
// present only when the op's transport is amt (`--via amt`, or `--via`
// omitted on a machine whose bmc-creds-admin entry is kind=amt). Its
// presence routes the site-worker executor to an AMT controller, the same
// way PDURuntime routes to a PDU controller. Resolved on global at
// op-creation: Host is the device's NetBox oob_ip (mask stripped),
// Username/Password the serial-keyed kind=amt admin entry. Site workers
// never read Secrets Manager.
type AMTRuntime struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password" secret:"true"`
}

// PDUOutlet describes one switched outlet feeding one of a machine's
// PSUs. Self-contained so the site worker can actuate without a NetBox
// round-trip.
type PDUOutlet struct {
	PDUHost  string `json:"pdu_host"`         // PDU OOB mgmt IP, e.g. "10.254.240.154"
	Outlet   int    `json:"outlet"`           // 1-based index (NetBox "Outlet 5" -> 5)
	Serial   string `json:"serial,omitempty"` // PDU NetBox serial == secret key (diag + cred join)
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty" secret:"true"`
}

// TailscaleRuntime carries a single-use tailnet auth key (plus the
// device's desired tailnet hostname) to a host being deployed. Resolved
// on global at op-creation, read by the boot-script handler to emit
// kernel-cmdline tokens for the deploy agent. The key is minted per
// deploy op, single-use, pre-authorized, tagged tag:<pop>.
type TailscaleRuntime struct {
	// AuthKey is the single-use tskey-auth-... value. Empty when minting
	// was skipped (no minter configured) or failed soft — the agent then
	// skips enrollment (no-op), it does not fail the deploy.
	AuthKey string `json:"auth_key,omitempty" secret:"true"`
	// Hostname is the tailnet node name to register (--hostname), matching
	// the ansible <short>-<pop> scheme so a re-deploy reuses the MagicDNS
	// name rather than spawning a duplicate node.
	Hostname string `json:"hostname,omitempty"`
	// Tags is the tag set assigned to the node (e.g. ["tag:pop2"]).
	// Recorded for audit/debug; the key itself already carries the tags.
	Tags []string `json:"tags,omitempty"`
}

// isJSONObject reports whether raw is a JSON object (starts with '{'
// after leading whitespace). Non-object params (a bare `null`, an array,
// a scalar) are tolerated as "no typed content" rather than an error:
// the pre-typing extractors probed shape this way, and treating a
// non-object blob as empty keeps the worker from failing an op on a
// payload it should simply ignore.
func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// transportKeys are the OpParams keys whose presence routes the executor
// to a PDU or AMT controller instead of the BMC.
var transportKeys = []string{"pdu_runtime", "amt_runtime"}

// exactKeyPaths are the params locations that something other than
// encoding/json reads, by exact key: every credential (RedactParams and
// store.ScrubTerminalOperationCredentials) and each transport key (the
// null check in DecodeParams).
var exactKeyPaths = func() [][]string {
	var paths [][]string
	for _, p := range secretParamPaths {
		paths = append(paths, p)
	}
	for _, k := range transportKeys {
		paths = append(paths, []string{k})
	}
	return paths
}()

// refuseCaseVariantKey returns an error when v, walked along path, has a
// key that differs from the path's key only in letter case: encoding/json
// matches such a key to the field (by strings.EqualFold, its rule), but
// the exact-key readers of exactKeyPaths do not see it. Every member of an
// object is checked, a repeated key's earlier members included, because the
// decoder reads them all. It runs on params the typed decode has accepted,
// so each value it walks is the object or array the path expects, or null.
// at is the dotted location of v, for the error.
func refuseCaseVariantKey(v json.RawMessage, path []string, at string) error {
	if len(path) == 0 {
		return nil
	}
	if path[0] == EachElement {
		at = strings.TrimSuffix(at, ".") + EachElement + "."
		var elems []json.RawMessage
		if err := json.Unmarshal(v, &elems); err != nil {
			return fmt.Errorf("decode op params: %w", err)
		}
		for _, e := range elems {
			if err := refuseCaseVariantKey(e, path[1:], at); err != nil {
				return err
			}
		}
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(v))
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode op params: %w", err)
	}
	if tok != json.Delim('{') {
		return nil // null: nothing below it
	}
	for dec.More() {
		if tok, err = dec.Token(); err != nil {
			return fmt.Errorf("decode op params: %w", err)
		}
		var child json.RawMessage
		if err := dec.Decode(&child); err != nil {
			return fmt.Errorf("decode op params: %w", err)
		}
		switch k, _ := tok.(string); {
		case k == path[0]:
			if err := refuseCaseVariantKey(child, path[1:], at+k+"."); err != nil {
				return err
			}
		case strings.EqualFold(k, path[0]):
			return fmt.Errorf("decode op params: key %q differs from %q only in letter case; write the exact key "+
				"(the credential redaction, the terminal-op credential scrub and the transport check match keys exactly)",
				at+k, at+path[0])
		}
	}
	return nil
}

// DecodeParams unmarshals an operation's params blob into the typed
// union. Empty/nil params and non-object params (null/array/scalar)
// decode to the zero OpParams with no error — those carry no typed
// content. A JSON object that fails to decode into OpParams (e.g. a
// malformed runtime block) returns an error so a consumer can fail the
// op cleanly rather than silently mis-parse it, and so does one that
// spells a credential or transport key in any letter case but the exact
// one (see refuseCaseVariantKey).
func DecodeParams(raw json.RawMessage) (OpParams, error) {
	var p OpParams
	if len(raw) == 0 || !isJSONObject(raw) {
		return p, nil
	}
	// pdu_runtime and amt_runtime are the transport-safety discriminators:
	// the executor routes PDU/AMT-vs-BMC on their presence. A literal null
	// must not collapse to "absent" (pointer stays nil -> silent BMC
	// fallback, which for an AMT machine means driving an AMT endpoint as
	// IPMI). Fail closed: reject null explicitly.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err == nil {
		for _, key := range transportKeys {
			if v, ok := probe[key]; ok && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				return OpParams{}, fmt.Errorf("decode op params: %s is null (must be an object); refusing to fall back to BMC transport", key)
			}
		}
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return OpParams{}, fmt.Errorf("decode op params: %w", err)
	}
	// encoding/json read {"bmc_runtime":{"Password":...}} as the BMC
	// credential just now, but the redaction would show it to operators and
	// the scrub would leave it in the table, since both look for "password"
	// exactly; the null check above likewise missed {"PDU_Runtime":null}.
	for _, path := range exactKeyPaths {
		if err := refuseCaseVariantKey(raw, path, ""); err != nil {
			return OpParams{}, err
		}
	}
	return p, nil
}

// DecodeParams decodes the receiver operation's params blob.
func (o Operation) DecodeParams() (OpParams, error) { return DecodeParams(o.Params) }

// OpResult is the typed union for an operation's result blob
// (Operation.Result, stored as jsonb and pushed on the WS OpUpdate).
// Terminal failures carry Error (and, for agent-reported deploy/commission
// failures, the Phase they died in). A successful power/lifecycle op
// leaves the row untouched (nil result); a bmc_reconcile op records its
// non-secret outcome in BMCReconcile whether it completed or failed.
type OpResult struct {
	Error        string              `json:"error,omitempty"`
	Phase        string              `json:"phase,omitempty"`
	BMCReconcile *BMCReconcileResult `json:"bmc_reconcile,omitempty"`
}

// BMCReconcileResult is the persisted outcome of a bmc_reconcile op. It
// holds only non-secret facts: a generated monitor password goes to
// pop/<pop>/bmc-creds-monitor only, and the stored one the op re-asserts
// rides the op's params (MonitorRuntime), never its result.
type BMCReconcileResult struct {
	// MonitorSlot is the IPMI user slot of the monitor account whose
	// password was written; 0 when no password was written.
	MonitorSlot int `json:"monitor_slot,omitempty"`
	// MonitorRotated is true when that password was freshly generated and
	// stored, and false when reconcile re-asserted the stored password (or
	// wrote none). Never omitted: a compliance run says plainly that it did
	// not rotate, because a rotation leaves the oob-gw exporters scraping
	// with a stale password until Ansible re-renders their config.
	MonitorRotated bool `json:"monitor_rotated"`
	// MonitorPasswordWritten is true when reconcile wrote the monitor
	// password, and false when the BMC already held the stored one (or none
	// was written). Never omitted, like MonitorRotated.
	MonitorPasswordWritten bool `json:"monitor_password_written"`
	// LeftoversDisabled names the accounts reconcile disabled. Each name
	// was also cleared from its slot where the BMC allowed it (Supermicro
	// refuses an empty name).
	LeftoversDisabled []string `json:"leftover_users_disabled,omitempty"`
	BMCTimeCompliance
}

// BMCTimeCompliance is the outcome of a bmc_reconcile op's time pass:
// NTP enabled with the BMC's OOB prefix ntp_servers as the only servers,
// the BMC's timezone UTC, and its clock within a minute of the site
// worker's. Embedded in both the worker's report and the persisted
// result, so the fields sit flat beside the account facts.
//
// A time failure is reported in TimeError and never fails the op: the
// op's pass/fail is the account compliance whose monitor secret global
// stores, and the time pass is re-asserted by every reconcile.
type BMCTimeCompliance struct {
	// NTPServers are the servers enforced; empty when the pass was
	// skipped (TimeError says why).
	NTPServers []string `json:"ntp_servers,omitempty"`
	// NTPConfigured is true when this run rewrote a non-compliant NTP
	// configuration (disabled, or other servers) and read it back
	// compliant.
	NTPConfigured bool `json:"ntp_configured,omitempty"`
	// TimezoneSet is true when this run set the BMC's timezone to UTC.
	TimezoneSet bool `json:"timezone_set,omitempty"`
	// ClockStepped is true when this run stepped the BMC clock to the
	// site worker's because it was more than a minute off.
	ClockStepped bool `json:"clock_stepped,omitempty"`
	// ClockSkewSeconds is the BMC clock minus the site worker's at the
	// start of the run (positive = BMC ahead); 0 when it was not read.
	ClockSkewSeconds int64 `json:"clock_skew_seconds,omitempty"`
	// TimeError is non-empty when the BMC's time configuration is not
	// known to be compliant after the run, including a skipped pass.
	TimeError string `json:"time_error,omitempty"`
}

// Marshal renders the result as a json.RawMessage for storage/wire.
func (r OpResult) Marshal() json.RawMessage {
	b, _ := json.Marshal(r)
	return b
}

// DecodeResult unmarshals an operation's result blob into the typed
// union. Empty/nil or non-object results decode to the zero OpResult
// with no error.
func DecodeResult(raw json.RawMessage) (OpResult, error) {
	var r OpResult
	if len(raw) == 0 || !isJSONObject(raw) {
		return r, nil
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return OpResult{}, fmt.Errorf("decode op result: %w", err)
	}
	return r, nil
}

// DecodeResult decodes the receiver operation's result blob.
func (o Operation) DecodeResult() (OpResult, error) { return DecodeResult(o.Result) }
