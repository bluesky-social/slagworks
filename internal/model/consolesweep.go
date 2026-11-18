package model

// Console determination sweep (commission-time discovery of
// slagworks_kernel_params).
//
// A device-type minted by commission (FindOrCreateDeviceType) has no
// slagworks_kernel_params, and there is no fallback console — deploy
// refuses until the field is set. Rather than making the operator
// hand-enter it, commission DETERMINES it empirically: when the field
// is unset, the boot handler boots the commission kernel with a
// broadcast console cmdline (every candidate tty), the agent emits a
// distinct nonce marker on each candidate, and global scans the
// machine's console_cast_events for the nonces. The candidate whose
// nonce arrived is the tty the BMC SOL is physically wired to; global
// writes it back to the device-type custom field (only if still
// empty). The operator's job becomes reviewing, not guessing.

// ConsoleSweepTTYs are the candidate serial ports the sweep probes and
// the broadcast commission cmdline lists. The agent (marker emission)
// and the site worker's boot handler (broadcast cmdline) must agree on
// this set, which is why it lives here and not in either binary.
// tty0 (VGA) is always prepended to the broadcast cmdline separately —
// it is not a SOL candidate.
var ConsoleSweepTTYs = []string{"ttyS0", "ttyS1"}

// ConsoleSweepBaud is the probe/broadcast baud. The whole BMC fleet
// SOLs at 115200; devices with non-standard serial (e.g. 57600
// switches) have no BMC, produce no cast events, and end the sweep
// with no hit — the operator sets "none" or a manual value.
const ConsoleSweepBaud = 115200

// ConsoleSweep rides in the commission report (CommissionResult
// .console_sweep) when the agent ran a determination sweep. Global
// resolves which candidate's nonce reached console_cast_events.
type ConsoleSweep struct {
	Candidates []ConsoleSweepCandidate `json:"candidates"`
}

// ConsoleSweepCandidate is one probed tty: the marker containing Nonce
// was written to /dev/<TTY> at Baud during commission.
type ConsoleSweepCandidate struct {
	TTY   string `json:"tty"`
	Baud  int    `json:"baud"`
	Nonce string `json:"nonce"`
}
