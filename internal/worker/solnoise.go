package worker

import "bytes"

// solNoisePatterns are the fixed byte sequences ipmitool itself prints to the
// SOL pty as its own chrome — NOT host serial output. We strip them at the cast
// pump (the source) so every consumer (TUI console pane, slagctl console,
// replay) gets a clean cast rather than each filtering client-side.
//
// Each pattern includes its trailing CRLF deliberately. ipmitool emits these
// via its error/log path, which always terminates the message with a newline,
// so the CRLF is part of the unit to remove — dropping the message together
// with its own line terminator keeps the surrounding cast byte-clean (no
// orphaned blank line) and, just as importantly, anchors the match on the right:
// a real console line that merely *contains* one of these strings as a substring
// (followed by more text, not by CRLF) will not match and passes through intact.
//
// What's in here (verbatim, case-sensitive, from a real pop1 deploy capture
// /private/tmp/l56-1-console.txt, 2026-06-11):
//
//   - the activation banner ipmitool prints on every (re)activate; on an idle
//     host the silence watchdog re-activates periodically so it stacks.
//   - the keepalive-timeout + payload-deactivate pair ipmitool prints every time
//     the BMC SOL session dies because the host reset/rebooted. These flood the
//     console pane on every reboot cycle and carry zero signal (PLA, this change).
//
// To extend: add the exact bytes ipmitool emits, including the trailing CRLF.
// Be conservative — only ipmitool's own teardown/keepalive/activation chrome
// belongs here, never anything a booting host could legitimately print. When in
// doubt, leave it out: a false positive that hides real boot output is worse
// than one surviving noise line.
var solNoisePatterns = [][]byte{
	[]byte("[SOL Session operational.  Use ~? for help]\r\n"), // note: two spaces
	[]byte("Error: No response to keepalive - Terminating session\r\n"),
	[]byte("Error: No response de-activating SOL payload\r\n"),
}

// solNoiseFilter removes every occurrence of any solNoisePatterns entry from a
// byte stream, robust to a pattern being split across reads. It is a streaming
// filter: feed it each pty read via Filter; it returns the bytes to keep and
// retains a short tail internally when the buffer ends with a prefix of some
// pattern, so a pattern straddling a read boundary is still matched on the next
// call. The held tail is bounded by the longest pattern, so memory is O(maxlen).
//
// Patterns are matched anywhere in the stream (not anchored to a preceding line
// boundary). This is intentional: ipmitool occasionally appends one of these
// stderr messages directly after non-newline-terminated BMC output (e.g. right
// after an ANSI reset sequence mid-screen-paint), so requiring a leading newline
// would leak those. The trailing-CRLF-in-pattern requirement is what prevents
// false positives instead. The patterns contain no ESC bytes, so removing a run
// can never split an ANSI escape sequence — the bytes on either side stay
// contiguous and the surrounding paint is preserved verbatim.
//
// Not safe for concurrent use; the cast pump drives it from one read loop.
type solNoiseFilter struct {
	// carry holds a trailing partial that is a prefix of at least one pattern —
	// we can't yet tell whether it'll complete into a match (drop) or diverge
	// (emit), so we hold it until the next read resolves it.
	carry []byte
}

// Filter consumes data and returns the bytes to forward downstream. Noise
// occurrences are removed; everything else passes through unchanged and in
// order. A trailing prefix-of-some-pattern is held back as carry and resolved on
// the next call (or flushed by Flush).
func (f *solNoiseFilter) Filter(data []byte) []byte {
	work := data
	if len(f.carry) > 0 {
		// Fresh slice: don't append into carry's backing array (we null carry
		// just below, and work must not alias it).
		work = make([]byte, 0, len(f.carry)+len(data))
		work = append(work, f.carry...)
		work = append(work, data...)
		f.carry = nil
	}

	var out []byte
	for len(work) > 0 {
		// Find the earliest match of any pattern. Matching the earliest
		// occurrence keeps the scan left-to-right and order-preserving.
		idx, plen := earliestNoiseMatch(work)
		if idx >= 0 {
			out = append(out, work[:idx]...)
			work = work[idx+plen:]
			continue
		}
		// No complete pattern. If a suffix of work is a prefix of some pattern,
		// hold it as carry — the rest of that pattern may arrive next read.
		if hold := noisePrefixSuffix(work); hold > 0 {
			out = append(out, work[:len(work)-hold]...)
			f.carry = append(f.carry[:0:0], work[len(work)-hold:]...)
		} else {
			out = append(out, work...)
		}
		break
	}
	return out
}

// Flush returns any held carry (a partial that never completed into a match),
// for emission when the stream ends. After Flush the filter is empty.
func (f *solNoiseFilter) Flush() []byte {
	c := f.carry
	f.carry = nil
	return c
}

// earliestNoiseMatch returns the index of the earliest occurrence of any
// solNoisePatterns entry in b, plus the length of the matched pattern. Returns
// (-1, 0) if none. When two patterns could match at different positions, the
// one starting earliest wins; ties (same start index) can't happen here because
// no pattern is a prefix of another, but if they ever could, the longer match is
// preferred so we strip the maximal unit.
func earliestNoiseMatch(b []byte) (int, int) {
	bestIdx, bestLen := -1, 0
	for _, p := range solNoisePatterns {
		i := bytes.Index(b, p)
		if i < 0 {
			continue
		}
		if bestIdx < 0 || i < bestIdx || (i == bestIdx && len(p) > bestLen) {
			bestIdx, bestLen = i, len(p)
		}
	}
	return bestIdx, bestLen
}

// noisePrefixSuffix returns the length of the longest suffix of b that is a
// (proper, non-empty) prefix of some pattern — i.e. how many trailing bytes to
// hold back as a possible start-of-pattern. Returns 0 if none. Taking the max
// across all patterns guarantees that whichever pattern is straddling the read
// boundary, enough bytes are retained for it to complete next read.
func noisePrefixSuffix(b []byte) int {
	best := 0
	for _, p := range solNoisePatterns {
		max := min(len(b), len(p)-1)
		for n := max; n > best; n-- {
			if bytes.Equal(b[len(b)-n:], p[:n]) {
				if n > best {
					best = n
				}
				break
			}
		}
	}
	return best
}
