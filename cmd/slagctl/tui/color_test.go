package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// TestColor_AppliedUnderANSI forces the ANSI color profile, renders a
// mid-deploy frame, and asserts (a) escape codes are present — color actually
// applies in a real terminal — and (b) every line still fits the terminal
// width measured by lipgloss.Width (which ignores escapes). (b) is the
// load-bearing invariant: coloring is a post-layout wrap, so it must not
// perturb visible widths. Restores the ASCII profile after so it can't leak
// into the goldens of other tests.
func TestColor_AppliedUnderANSI(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	defer lipgloss.SetColorProfile(termenv.Ascii)

	frame := renderFrame(midDeployFrame(minTermCols, minRows))

	if !strings.Contains(frame, "\x1b[") {
		t.Fatal("forced-ANSI frame has no escape codes — color not applied")
	}
	for line := range strings.SplitSeq(frame, "\n") {
		if w := lipgloss.Width(line); w > minTermCols {
			t.Errorf("colored line exceeds width %d: visible=%d\n%q", minTermCols, w, line)
		}
	}
}

// TestColor_StrippedOnNonTTY is the goldens' safety net: the default test
// profile renders no escape codes, so snapshots stay byte-stable regardless of
// the dev's terminal. (lipgloss strips on non-TTY; forceNoColor pins it.)
func TestColor_StrippedOnNonTTY(t *testing.T) {
	forceNoColor()
	frame := renderFrame(midDeployFrame(minTermCols, minRows))
	if strings.Contains(frame, "\x1b[") {
		t.Error("non-TTY frame contains escape codes — goldens would be unstable")
	}
}
