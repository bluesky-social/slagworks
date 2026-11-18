package tui

import (
	"bytes"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// downsample converts styled output to what a terminal with color profile p
// receives. Lip Gloss always renders full color; Bubble Tea downsamples each
// frame to the profile it detects for its output.
func downsample(t *testing.T, s string, p colorprofile.Profile) string {
	t.Helper()
	var buf bytes.Buffer
	w := colorprofile.Writer{Forward: &buf, Profile: p}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatalf("downsample to %v: %v", p, err)
	}
	return buf.String()
}

// TestColor_AppliedUnderANSI renders a mid-deploy frame for an ANSI (16-color)
// terminal and asserts (a) escape codes are present — color actually applies
// in a real terminal — and (b) every line still fits the terminal width
// measured by lipgloss.Width (which ignores escapes). (b) is the load-bearing
// invariant: coloring is a post-layout wrap, so it must not perturb visible
// widths.
func TestColor_AppliedUnderANSI(t *testing.T) {
	frame := downsample(t, renderFrame(midDeployFrame(minTermCols, minRows)), colorprofile.ANSI)

	if !strings.Contains(frame, "\x1b[") {
		t.Fatal("ANSI frame has no escape codes — color not applied")
	}
	for line := range strings.SplitSeq(frame, "\n") {
		if w := lipgloss.Width(line); w > minTermCols {
			t.Errorf("colored line exceeds width %d: visible=%d\n%q", minTermCols, w, line)
		}
	}
}

// TestColor_StrippedOnNonTTY is the goldens' safety net: a frame downsampled
// for a terminal without color support carries no escape codes, and is exactly
// the stripped text the goldens pin.
func TestColor_StrippedOnNonTTY(t *testing.T) {
	frame := renderFrame(midDeployFrame(minTermCols, minRows))
	got := downsample(t, frame, colorprofile.NoTTY)
	if strings.Contains(got, "\x1b[") {
		t.Error("non-TTY frame contains escape codes — goldens would be unstable")
	}
	if got != plain(frame) {
		t.Errorf("non-TTY frame differs from the stripped frame the goldens pin\n--- non-TTY ---\n%s\n--- plain ---\n%s",
			got, plain(frame))
	}
}
