package smcipmitool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := map[string]Mode{
		"":         ModeAuto,
		"auto":     ModeAuto,
		"required": ModeRequired,
		" Off ":    ModeOff,
		"REQUIRED": ModeRequired,
	}
	for in, want := range cases {
		got, err := ParseMode(in)
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ParseMode("yes"); err == nil {
		t.Error("ParseMode(\"yes\") = nil error, want an invalid-value error")
	}
}

// installLauncher writes an executable stand-in launcher into a fresh dir
// and returns its path.
func installLauncher(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestToolStatus(t *testing.T) {
	t.Run("installed on PATH", func(t *testing.T) {
		p := installLauncher(t, DefaultBin)
		t.Setenv("PATH", filepath.Dir(p))
		tool := Tool{Mode: ModeAuto}
		if got := tool.Status(); got != StatusInstalled {
			t.Fatalf("Status() = %q, want %q", got, StatusInstalled)
		}
		if got, ok := tool.Lookup(); !ok || got != p {
			t.Fatalf("Lookup() = %q, %v; want %q, true", got, ok, p)
		}
	})
	t.Run("installed at an explicit path", func(t *testing.T) {
		p := installLauncher(t, "smc-launcher")
		t.Setenv("PATH", t.TempDir())
		tool := Tool{Mode: ModeRequired, Bin: p}
		if got := tool.Status(); got != StatusInstalled {
			t.Fatalf("Status() = %q, want %q", got, StatusInstalled)
		}
	})
	t.Run("missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		tool := Tool{Mode: ModeAuto}
		if got := tool.Status(); got != StatusMissing {
			t.Fatalf("Status() = %q, want %q", got, StatusMissing)
		}
		if _, ok := tool.Lookup(); ok {
			t.Fatal("Lookup() ok = true with an empty PATH")
		}
	})
	t.Run("missing in required mode", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		tool := Tool{Mode: ModeRequired}
		if got := tool.Status(); got != StatusMissingRequired {
			t.Fatalf("Status() = %q, want %q", got, StatusMissingRequired)
		}
	})
	t.Run("off ignores an installed tool", func(t *testing.T) {
		p := installLauncher(t, DefaultBin)
		t.Setenv("PATH", filepath.Dir(p))
		tool := Tool{Mode: ModeOff}
		if got := tool.Status(); got != StatusDisabled {
			t.Fatalf("Status() = %q, want %q", got, StatusDisabled)
		}
		if _, ok := tool.Lookup(); ok {
			t.Fatal("Lookup() ok = true in off mode")
		}
	})
}

func TestMissingHintNamesWorkerAndDoc(t *testing.T) {
	h := MissingHint("worker-a.example")
	for _, want := range []string{"worker-a.example", InstallDoc} {
		if !strings.Contains(h, want) {
			t.Errorf("MissingHint() = %q, missing %q", h, want)
		}
	}
}
