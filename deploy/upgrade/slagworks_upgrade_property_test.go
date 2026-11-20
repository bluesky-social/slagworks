package upgrade

// Property test for the auto-upgrade helper's argument gate.
//
// The helper runs as root through a sudoers entry that allows any
// arguments, and the two it expects come from whatever global the worker is
// connected to. The oracle below restates the documented argument grammar
// in Go, character by character, without a regular expression; the helper
// uses bash's. For every generated argument list:
//
//   - the oracle rejects it: the helper exits non-zero and never starts
//     systemd-run (the stub that stands in for the apt transaction);
//   - the oracle accepts it: the helper hands systemd-run exactly one apt
//     transaction that pins both packages to that version and nothing else.
//
// The generator starts from valid arguments and breaks them one way at a
// time (a metacharacter, a newline, an option-looking prefix, a second apt
// package, whitespace, a non-ASCII digit), so the near-valid strings a
// permissive pattern would let through are the common case.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// oracleSHA: 7 to 64 lower-case hex characters.
func oracleSHA(s string) bool { return len(s) >= 7 && len(s) <= 64 && isLowerHex(s) }

// oracleVersion: X.Y.Z, or X.Y.Z+git.N.g<hex>.
func oracleVersion(s string) bool {
	base, snap, hasSnap := strings.Cut(s, "+")
	parts := strings.Split(base, ".")
	if len(parts) != 3 || !isDigits(parts[0]) || !isDigits(parts[1]) || !isDigits(parts[2]) {
		return false
	}
	if !hasSnap {
		return true
	}
	sp := strings.Split(snap, ".")
	return len(sp) == 3 && sp[0] == "git" && isDigits(sp[1]) && strings.HasPrefix(sp[2], "g") && isLowerHex(sp[2][1:])
}

var breakers = []string{
	";", " ", "\n", "\t", "$(id)", "`id`", "|", "&", ">", "<", "'", "\"", "\\", "*", "?", "~", "#", "=", "..", "/",
	"-", "--", "-o", "A", "g", "G", "x", "+", ".", "\x00", "\r", "\u0660", "\uff11", "é", " slagworks-agent=0.0.1", "%s", "${IFS}",
}

func genSHA(t *rapid.T) string {
	n := rapid.SampledFrom([]int{6, 7, 8, 12, 40, 63, 64, 65}).Draw(t, "shaLen")
	return rapid.StringOfN(rapid.SampledFrom([]rune("0123456789abcdef")), n, n, -1).Draw(t, "sha")
}

func genVersion(t *rapid.T) string {
	num := func(label string) string {
		return rapid.SampledFrom([]string{"0", "1", "6", "12", "007", "4294967296"}).Draw(t, label)
	}
	v := num("major") + "." + num("minor") + "." + num("patch")
	if rapid.Bool().Draw(t, "snapshot") {
		v += "+git." + num("commits") + ".g" + rapid.StringOfN(rapid.SampledFrom([]rune("0123456789abcdef")), 1, 12, -1).Draw(t, "snapSHA")
	}
	return v
}

// mutate breaks s in one place, or leaves it alone.
func mutate(t *rapid.T, s, label string) string {
	switch rapid.IntRange(0, 5).Draw(t, label+"Mut") {
	case 0, 1:
		return s
	case 2: // insert
		at := rapid.IntRange(0, len(s)).Draw(t, label+"At")
		return s[:at] + rapid.SampledFrom(breakers).Draw(t, label+"Ins") + s[at:]
	case 3: // replace one byte
		if s == "" {
			return s
		}
		at := rapid.IntRange(0, len(s)-1).Draw(t, label+"At")
		return s[:at] + rapid.SampledFrom(breakers).Draw(t, label+"Rep") + s[at+1:]
	case 4: // truncate
		return s[:rapid.IntRange(0, len(s)).Draw(t, label+"Cut")]
	default: // arbitrary
		return rapid.String().Draw(t, label+"Any")
	}
}

func TestHelper_Property_ArgumentGate(t *testing.T) {
	var accepted, rejected, nearValid int
	rapid.Check(t, func(rt *rapid.T) {
		sha := mutate(rt, genSHA(rt), "sha")
		version := mutate(rt, genVersion(rt), "ver")
		args := []string{sha, version}
		switch rapid.IntRange(0, 9).Draw(rt, "argc") {
		case 0:
			args = args[:1]
		case 1:
			args = append(args, rapid.SampledFrom([]string{"", "extra", "--", validVersion}).Draw(rt, "third"))
		}
		// exec cannot pass a NUL inside an argument; such a list never
		// reaches the helper.
		for _, a := range args {
			if strings.ContainsRune(a, 0) {
				rt.Skip("argument holds a NUL")
			}
		}
		want := len(args) == 2 && oracleSHA(args[0]) && oracleVersion(args[1])

		dir, err := os.MkdirTemp("", "upgrade-prop-")
		if err != nil {
			rt.Fatalf("temp dir: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		for name, body := range map[string]string{
			"systemd-run": "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$STUB_DIR/systemd-run.args\"\n",
			"curl":        "#!/bin/sh\ntouch \"$STUB_DIR/fetched\"\nexit 1\n",
			"wget":        "#!/bin/sh\ntouch \"$STUB_DIR/fetched\"\nexit 1\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
				rt.Fatalf("stub: %v", err)
			}
		}
		stderr, code := runHelper(t, dir, args...)
		raw, statErr := os.ReadFile(filepath.Join(dir, "systemd-run.args"))
		reached := statErr == nil

		if !want {
			rejected++
			if len(args) == 2 && (oracleSHA(args[0]) || oracleVersion(args[1])) {
				nearValid++
			}
			if reached {
				rt.Fatalf("the helper started the apt transaction for arguments the documented grammar rejects: %q\nsystemd-run argv:\n%s", args, raw)
			}
			if code == 0 {
				rt.Fatalf("the helper exited 0 for arguments the documented grammar rejects: %q", args)
			}
			return
		}
		accepted++
		if code != 0 || !reached {
			rt.Fatalf("the helper refused valid arguments %q: exit %d, stderr:\n%s", args, code, stderr)
		}
		got := string(raw)
		for _, wantArg := range []string{"--wait", "slagworks=" + args[1] + " ", "slagworks-agent=" + args[1] + "\n"} {
			if !strings.Contains(got, wantArg) {
				rt.Fatalf("systemd-run argv for %q is missing %q:\n%s", args, wantArg, got)
			}
		}
		// The transaction names the two packages, pinned, and no third.
		if n := strings.Count(got, "="+args[1]); n != 2 {
			rt.Fatalf("systemd-run argv for %q pins %d packages to the version, want 2:\n%s", args, n, got)
		}
	})
	t.Logf("coverage: accepted=%d rejected=%d (near-valid, one argument good: %d)", accepted, rejected, nearValid)
}
