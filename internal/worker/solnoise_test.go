package worker

import (
	"bytes"
	"testing"
)

// keepalive / teardown noise lines ipmitool itself prints to the SOL pty when a
// session dies because the host reset/rebooted. Verbatim from a real pop1 deploy
// capture (/private/tmp/l56-1-console.txt, 2026-06-11): both carry the "Error: "
// prefix and a trailing CRLF.
const (
	noiseKeepalive  = "Error: No response to keepalive - Terminating session\r\n"
	noiseDeactivate = "Error: No response de-activating SOL payload\r\n"
)

// feedNoise runs a sequence of reads through one solNoiseFilter and returns the
// concatenated kept output plus whatever Flush yields at the end.
func feedNoise(reads ...string) string {
	var f solNoiseFilter
	var out bytes.Buffer
	for _, r := range reads {
		out.Write(f.Filter([]byte(r)))
	}
	out.Write(f.Flush())
	return out.String()
}

// TestSOLNoise_StripsKeepalive: a standalone keepalive line is dropped whole,
// including its trailing CRLF; surrounding real content survives in order.
func TestSOLNoise_StripsKeepalive(t *testing.T) {
	got := feedNoise("real output\r\n" + noiseKeepalive + "more real\r\n")
	want := "real output\r\nmore real\r\n"
	if got != want {
		t.Errorf("keepalive not stripped:\n got %q\nwant %q", got, want)
	}
}

// TestSOLNoise_StripsDeactivate: the de-activating-payload line is dropped whole.
func TestSOLNoise_StripsDeactivate(t *testing.T) {
	got := feedNoise(noiseDeactivate + "after\r\n")
	if got != "after\r\n" {
		t.Errorf("deactivate not stripped: %q", got)
	}
}

// TestSOLNoise_StripsRebootCycle: the real reboot-cycle interleaving — a chunk
// of console output, then both noise lines back to back (as ipmitool emits them
// when the host resets), then more console. Both noise lines vanish, both real
// chunks remain.
func TestSOLNoise_StripsRebootCycle(t *testing.T) {
	in := "[   87.0] XFS (md0): Ending clean mount\r\n" +
		noiseKeepalive + noiseDeactivate +
		"slagworks: rebooting (deploy)\r\n"
	want := "[   87.0] XFS (md0): Ending clean mount\r\n" +
		"slagworks: rebooting (deploy)\r\n"
	if got := feedNoise(in); got != want {
		t.Errorf("reboot cycle wrong:\n got %q\nwant %q", got, want)
	}
}

// TestSOLNoise_SplitAcrossReads: a noise line straddling a read boundary must
// still be dropped — neither read contains it whole. This is the case that
// justifies the stateful carry; a per-read substring strip would miss it.
func TestSOLNoise_SplitAcrossReads(t *testing.T) {
	mid := len(noiseKeepalive) / 2
	got := feedNoise(noiseKeepalive[:mid], noiseKeepalive[mid:]+"after\r\n")
	if got != "after\r\n" {
		t.Errorf("split noise not stripped: %q", got)
	}
}

// TestSOLNoise_SplitEveryByte: the pathological one-byte-per-read split.
func TestSOLNoise_SplitEveryByte(t *testing.T) {
	var f solNoiseFilter
	var out bytes.Buffer
	for _, b := range []byte(noiseDeactivate + "x") {
		out.Write(f.Filter([]byte{b}))
	}
	out.Write(f.Flush())
	if out.String() != "x" {
		t.Errorf("byte-split noise not stripped: %q", out.String())
	}
}

// TestSOLNoise_SubstringNotWholeLine is the false-positive guard: a REAL console
// line that merely CONTAINS a noise message as a substring (no trailing CRLF
// right after the message) must pass through untouched. The trailing CRLF is
// part of the pattern, so the message-without-its-own-line-terminator does not
// match.
func TestSOLNoise_SubstringNotWholeLine(t *testing.T) {
	// Real-ish: a log line quoting the ipmitool error mid-sentence.
	in := "Error: No response to keepalive - Terminating session was observed earlier\r\n"
	if got := feedNoise(in); got != in {
		t.Errorf("substring (not whole noise line) was eaten:\n got %q\nwant %q", got, in)
	}
}

// TestSOLNoise_PrefixThatDiverges: a partial that looks like the start of a noise
// line but then diverges must be emitted intact, not dropped.
func TestSOLNoise_PrefixThatDiverges(t *testing.T) {
	in := "Error: No response from the disk controller\r\n"
	if got := feedNoise(in); got != in {
		t.Errorf("divergent prefix dropped bytes:\n got %q\nwant %q", got, in)
	}
}

// TestSOLNoise_TrailingPartialFlushed: a stream that ends on a genuine
// prefix-of-noise (no more reads coming) must flush those held bytes.
func TestSOLNoise_TrailingPartialFlushed(t *testing.T) {
	in := "output Error: No response to keep"
	if got := feedNoise(in); got != in {
		t.Errorf("trailing partial eaten: %q", got)
	}
}

// TestSOLNoise_NoiseAfterANSIReset reproduces the real line-123 case: ipmitool's
// stderr message lands directly after a complete BMC ANSI reset sequence, with NO
// intervening newline. The noise must still be removed, and the ANSI bytes before
// it must survive byte-for-byte (the filter must not nibble into the escape run).
func TestSOLNoise_NoiseAfterANSIReset(t *testing.T) {
	ansi := "\x1b[24;75H32\x1b[0;37;40m"
	in := ansi + noiseDeactivate + "next\r\n"
	want := ansi + "next\r\n"
	if got := feedNoise(in); got != want {
		t.Errorf("noise-after-ANSI wrong:\n got %q\nwant %q", got, want)
	}
}

// TestSOLNoise_PreservesSlagworksStatus: slagworks' own status lines (which the
// pump MUST keep) are not noise and pass through, even one mentioning reboot.
func TestSOLNoise_PreservesSlagworksStatus(t *testing.T) {
	in := "slagworks: rebooting (deploy)\r\n"
	if got := feedNoise(in); got != in {
		t.Errorf("slagworks status line altered:\n got %q\nwant %q", got, in)
	}
}

// --- banner cases preserved: the activation banner is now one pattern in the
// same multi-pattern filter, so its behavior must be unchanged. ---

const bannerLine = "[SOL Session operational.  Use ~? for help]\r\n"

func TestSOLNoise_StripsBanner(t *testing.T) {
	if got := feedNoise(bannerLine + "real output\r\n"); got != "real output\r\n" {
		t.Errorf("banner not stripped: %q", got)
	}
}

func TestSOLNoise_StripsRepeatedBanner(t *testing.T) {
	if got := feedNoise(bannerLine + bannerLine + bannerLine); got != "" {
		t.Errorf("repeated banners should collapse to empty, got %q", got)
	}
}

func TestSOLNoise_BannerSplitAcrossReads(t *testing.T) {
	mid := len(bannerLine) / 2
	if got := feedNoise(bannerLine[:mid], bannerLine[mid:]+"after\r\n"); got != "after\r\n" {
		t.Errorf("split banner not stripped: %q", got)
	}
}

// TestSOLNoise_MixedBannerAndKeepalive: banner and keepalive interleaved with
// real output — both kinds of noise removed, real content intact and ordered.
func TestSOLNoise_MixedBannerAndKeepalive(t *testing.T) {
	in := bannerLine + "boot\r\n" + noiseKeepalive + "kernel\r\n" + bannerLine + noiseDeactivate
	want := "boot\r\nkernel\r\n"
	if got := feedNoise(in); got != want {
		t.Errorf("mixed strip wrong:\n got %q\nwant %q", got, want)
	}
}

// TestSOLNoise_NoNoise: ordinary kernel output passes through untouched.
func TestSOLNoise_NoNoise(t *testing.T) {
	in := "[    5.231876] sd 0:0:0:0: [sda] 7814037168 512-byte\r\n"
	if got := feedNoise(in); got != in {
		t.Errorf("plain output altered:\n got %q\nwant %q", got, in)
	}
}
