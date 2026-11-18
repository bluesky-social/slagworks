package smcipmitool

import (
	"slices"
	"strings"
	"testing"
)

// These tests pin the exact argument vectors we hand to the SMCIPMITool
// binary. They are the load-bearing part of the wrapper: a wrong verb or a
// wrong boot-option index silently mis-drives real hardware, so we assert
// the bytes rather than trusting the mapping by inspection. Command syntax
// is taken from the SMCIPMITool 2.30.0 User's Guide §3.6.2 (ipmi power) and
// §3.6.5 (ipmi fru); the positional credential shape is §2 ("SMCIPMITool
// <IP> <username> <password> [commands ...]").

func conn() Conn {
	return Conn{Host: "192.0.2.9", User: "operator", Pass: "s3cr3t!"}
}

func TestArgvPositionalCredentials(t *testing.T) {
	// The credential contract: host, user, pass are the first three
	// positional args, in that order, ahead of the subcommand tokens.
	got := conn().argv("ipmi", "power", "status")
	want := []string{"192.0.2.9", "operator", "s3cr3t!", "ipmi", "power", "status"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestPowerVerbArgv(t *testing.T) {
	c := conn()
	cases := []struct {
		name string
		got  []string
		tail []string
	}{
		{"status", c.PowerStatusArgs(), []string{"ipmi", "power", "status"}},
		{"on", c.PowerOnArgs(), []string{"ipmi", "power", "up"}},
		{"off", c.PowerOffArgs(), []string{"ipmi", "power", "down"}},
		{"cycle", c.PowerCycleArgs(), []string{"ipmi", "power", "cycle"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := append([]string{c.Host, c.User, c.Pass}, tc.tail...)
			if !slices.Equal(tc.got, want) {
				t.Fatalf("argv mismatch\n got: %q\nwant: %q", tc.got, want)
			}
		})
	}
}

func TestBootOptionArgv(t *testing.T) {
	// Slagworks boots UEFI (BMCController.SetBootDevice passes efi=true), so
	// PXE is index 14 (UEFI PXE) and Disk is index 9 (UEFI Hard-drive), NOT
	// the legacy 1/2. Persistent is 0: a one-time override, matching the
	// existing non-persistent SetBootDevice semantics. Getting this wrong
	// either no-ops the deploy (legacy PXE on a UEFI box) or wedges the host
	// into a boot loop (persistent PXE), so it is asserted explicitly.
	c := conn()

	t.Run("pxe one-time uefi", func(t *testing.T) {
		got := c.BootPXEArgs()
		want := append([]string{c.Host, c.User, c.Pass}, "ipmi", "power", "bootoption", "14", "0")
		if !slices.Equal(got, want) {
			t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("disk one-time uefi", func(t *testing.T) {
		got := c.BootDiskArgs()
		want := append([]string{c.Host, c.User, c.Pass}, "ipmi", "power", "bootoption", "9", "0")
		if !slices.Equal(got, want) {
			t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, want)
		}
	})
}

func TestChassisSerialArgv(t *testing.T) {
	c := conn()
	got := c.ChassisSerialArgs()
	want := append([]string{c.Host, c.User, c.Pass}, "ipmi", "fru")
	if !slices.Equal(got, want) {
		t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestRedactMasksPasswordOnly(t *testing.T) {
	// Redact is the one hard secret-hygiene rule: the
	// password (3rd positional) must never reach a log line, but everything
	// else — host, user, verb — stays visible so the invocation is
	// debuggable. We assert the password byte-string appears nowhere in the
	// redacted form and that nothing else was altered.
	c := conn()
	full := c.PowerStatusArgs()
	red := Redact(full)

	if slices.Contains(red, c.Pass) {
		t.Fatalf("redacted argv still contains the password: %q", red)
	}
	joined := strings.Join(red, " ")
	if strings.Contains(joined, c.Pass) {
		t.Fatalf("password leaked into redacted string: %q", joined)
	}
	// Host, user, and the verb survive.
	for _, keep := range []string{c.Host, c.User, "ipmi", "power", "status"} {
		if !slices.Contains(red, keep) {
			t.Fatalf("redacted argv dropped %q: %q", keep, red)
		}
	}
	// Redact must not mutate its input.
	if !slices.Contains(full, c.Pass) {
		t.Fatalf("Redact mutated the source argv, removing the password: %q", full)
	}
}

func TestRedactHandlesShortArgv(t *testing.T) {
	// Defensive: a malformed argv shorter than the positional credential
	// triple must not panic. It should return the input unchanged (there is
	// no password slot to mask).
	for _, in := range [][]string{nil, {}, {"host"}, {"host", "user"}} {
		out := Redact(in)
		if !slices.Equal(out, in) {
			t.Fatalf("Redact(%q) = %q, want unchanged", in, out)
		}
	}
}

func TestScrubSecret(t *testing.T) {
	cases := []struct {
		name string
		in   string
		pass string
		want string
	}{
		{"masks all occurrences", "a P b P c", "P", "a *** b *** c"},
		{"no match", "nothing here", "P", "nothing here"},
		{"empty pass is no-op", "a b c", "", "a b c"},
		{"empty input", "", "P", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scrubSecret(tc.in, tc.pass); got != tc.want {
				t.Fatalf("scrubSecret(%q,%q) = %q, want %q", tc.in, tc.pass, got, tc.want)
			}
		})
	}
}

func TestStringRoundTripsThroughRedact(t *testing.T) {
	// String() is what callers log; it must be the redacted form.
	c := conn()
	s := c.String("ipmi", "power", "status")
	if strings.Contains(s, c.Pass) {
		t.Fatalf("Conn.String leaked the password: %q", s)
	}
	if !strings.Contains(s, c.Host) || !strings.Contains(s, "ipmi power status") {
		t.Fatalf("Conn.String dropped non-secret tokens: %q", s)
	}
}
