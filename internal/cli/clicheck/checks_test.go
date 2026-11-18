package clicheck

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/cli/cliupdate"
)

func TestBinaryProvenanceCheckUpgradeAvailable(t *testing.T) {
	tt := []struct {
		name    string
		current string
		latest  string
		want    bool
	}{
		{name: "same version with tag prefix", current: "1.0.10", latest: "v1.0.10", want: false},
		{name: "same spelling", current: "v1.0.10", latest: "v1.0.10", want: false},
		{name: "newer patch", current: "1.0.10", latest: "v1.0.11", want: true},
		{name: "older discovered version", current: "1.0.10", latest: "v1.0.9", want: false},
		{name: "unparseable current version", current: "dev-build", latest: "v1.0.11", want: false},
		{name: "unparseable latest version", current: "1.0.10", latest: "newest", want: false},
		{name: "no discovered version", current: "1.0.10", latest: "", want: false},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("XDG_STATE_HOME", tmp)
			if err := os.MkdirAll(filepath.Join(tmp, "slagctl"), 0o700); err != nil {
				t.Fatal(err)
			}
			state := fmt.Appendf(nil, `{"latest_seen":%q}`, tc.latest)
			if err := os.WriteFile(filepath.Join(tmp, "slagctl", "update-check.json"), state, 0o600); err != nil {
				t.Fatal(err)
			}

			cfg := cliupdate.Config{Tool: "slagctl", CurrentVersion: tc.current, UpgradeCommand: "upgrade-me slagctl"}
			got := BinaryProvenanceCheck(cfg).Run(context.Background())
			if v := strings.Contains(got.Detail, "available"); v != tc.want {
				t.Errorf("detail = %q, available = %t, want %t", got.Detail, v, tc.want)
			}
			if tc.want && !strings.Contains(got.Detail, "run 'upgrade-me slagctl'") {
				t.Errorf("detail = %q, want it to name the upgrade command", got.Detail)
			}
		})
	}
}

func TestBinaryProvenanceCheckDevBuildWarns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	got := BinaryProvenanceCheck(cliupdate.Config{Tool: "slagctl", CurrentVersion: "dev"}).Run(context.Background())
	if got.Status != StatusWarn {
		t.Errorf("dev build status = %v, want StatusWarn (detail %q)", got.Status, got.Detail)
	}
}

func TestStateDirWritableCheck(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", tmp)
	got := StateDirWritableCheck("slagctl").Run(context.Background())
	if got.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK (detail %q)", got.Status, got.Detail)
	}
	if want := filepath.Join(tmp, "slagctl"); got.Detail != want {
		t.Errorf("detail = %q, want the state dir %q", got.Detail, want)
	}
}
