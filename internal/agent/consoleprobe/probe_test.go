package consoleprobe

import "testing"

// TestParseConsoleParams pins the kernel-cmdline parsing for
// Probe. The big subtle point is "last match wins" —
// the kernel maps /dev/console to the LAST `console=` token, and
// the BMC SOL is wired to whatever winds up as /dev/console. A
// first-match implementation would probe `console=tty0` (VGA) and
// never reach the BMC SOL stream.
func TestParseConsoleParams(t *testing.T) {
	cases := []struct {
		name     string
		kp       string
		wantTTY  string
		wantBaud int
		wantErr  bool
	}{
		{
			name:     "ttyS1_with_baud",
			kp:       "console=ttyS1,115200n8",
			wantTTY:  "ttyS1",
			wantBaud: 115200,
		},
		{
			name:     "tty0_then_ttyS1_last_wins",
			kp:       "console=tty0 console=ttyS1,115200n8",
			wantTTY:  "ttyS1",
			wantBaud: 115200,
		},
		{
			name:     "alt_baud",
			kp:       "console=ttyS0,57600n8",
			wantTTY:  "ttyS0",
			wantBaud: 57600,
		},
		{
			name:     "no_baud_defaults_115200",
			kp:       "console=ttyS1",
			wantTTY:  "ttyS1",
			wantBaud: 115200,
		},
		{
			name:    "empty",
			kp:      "",
			wantErr: true,
		},
		{
			name:    "no_console_directive",
			kp:      "ro quiet",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tty, baud, err := parseConsoleParams(tc.kp)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got tty=%q baud=%d", tty, baud)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tty != tc.wantTTY {
				t.Errorf("tty: got %q, want %q", tty, tc.wantTTY)
			}
			if baud != tc.wantBaud {
				t.Errorf("baud: got %d, want %d", baud, tc.wantBaud)
			}
		})
	}
}
