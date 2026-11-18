package cliupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/internal/cli/clierr"
)

// --- Helpers ---

const testTool = "slagctl"

// setupStateDir points the XDG state home at a temp dir and clears the
// environment that would otherwise suppress or bypass the hook.
func setupStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("CI", "")
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("BSKY_CLI_NO_AUTOUPDATE", "")
	t.Setenv(envNoVersionGate, "")
	t.Setenv(envHelperGuard, "")
	return dir
}

func testCfg() Config {
	return Config{Tool: testTool, CurrentVersion: "v0.2.0", UpgradeCommand: "upgrade-me " + testTool}
}

// --- State file tests ---

func TestStateRoundTrip(t *testing.T) {
	setupStateDir(t)

	ts := ToolState{
		LastCheck:    time.Now().UTC().Truncate(time.Second),
		LatestSeen:   "v0.3.0",
		LatestSeenAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := writeToolState(testTool, ts); err != nil {
		t.Fatalf("writeToolState: %v", err)
	}
	got := readToolState(testTool)
	if got.LatestSeen != ts.LatestSeen {
		t.Errorf("LatestSeen: got %q, want %q", got.LatestSeen, ts.LatestSeen)
	}
	if !got.LatestSeenAt.Equal(ts.LatestSeenAt) {
		t.Errorf("LatestSeenAt: got %v, want %v", got.LatestSeenAt, ts.LatestSeenAt)
	}
}

// TestStateFileLocation pins the state file to <XDG state home>/<tool>/, the
// tool's own directory rather than one shared with other CLIs.
func TestStateFileLocation(t *testing.T) {
	dir := setupStateDir(t)
	if err := updateToolState(testTool, func(ts *ToolState) { ts.LatestSeen = "v0.3.0" }); err != nil {
		t.Fatalf("updateToolState: %v", err)
	}
	want := filepath.Join(dir, testTool, "update-check.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("state file not at %s: %v", want, err)
	}
	got, err := StateDir(testTool)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Dir(want) {
		t.Errorf("StateDir = %q, want %q", got, filepath.Dir(want))
	}
}

func TestCorruptStateFileToleratedAsEmpty(t *testing.T) {
	dir := setupStateDir(t)
	path := filepath.Join(dir, testTool, "update-check.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not valid json{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ts := readToolState(testTool); ts != (ToolState{}) {
		t.Errorf("expected zero state on corrupt file, got %+v", ts)
	}
}

func TestMissingStateFileToleratedAsEmpty(t *testing.T) {
	setupStateDir(t)
	if ts := readToolState(testTool); ts != (ToolState{}) {
		t.Errorf("expected zero state when file missing, got %+v", ts)
	}
}

func TestPerToolIsolation(t *testing.T) {
	setupStateDir(t)

	check := time.Now().UTC().Truncate(time.Second)
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.LastCheck = check
	})
	if other := readToolState("othertool"); !other.LastCheck.IsZero() {
		t.Error("one tool's check should not affect another tool's state")
	}
	if got := readToolState(testTool); !got.LastCheck.Equal(check) {
		t.Errorf("LastCheck: got %v, want %v", got.LastCheck, check)
	}
}

// --- Skip condition tests ---

func TestSkipOnNoAutoupdateEnv(t *testing.T) {
	setupStateDir(t)
	t.Setenv("BSKY_CLI_NO_AUTOUPDATE", "1")
	h := NewHook(testCfg())
	if h.ShouldSpawnChild() {
		t.Error("ShouldSpawnChild should be false with BSKY_CLI_NO_AUTOUPDATE=1")
	}
}

func TestSkipOnCIEnv(t *testing.T) {
	setupStateDir(t)
	t.Setenv("CI", "true")
	h := NewHook(testCfg())
	if h.ShouldSpawnChild() {
		t.Error("ShouldSpawnChild should be false under CI")
	}
}

func TestSkipInsideHelper(t *testing.T) {
	setupStateDir(t)
	t.Setenv(envHelperGuard, "1")
	h := NewHook(testCfg())
	if h.ShouldSpawnChild() {
		t.Error("the helper child must not spawn another child")
	}
}

func TestShouldNotSpawnChildWhenRecent(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.LastCheck = time.Now()
	})
	h := NewHook(testCfg())
	if h.ShouldSpawnChild() {
		t.Error("ShouldSpawnChild should be false when checked recently")
	}
}

func TestShouldSpawnChildWhenDue(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.LastCheck = time.Now().Add(-25 * time.Hour)
	})
	h := NewHook(testCfg())
	if !h.ShouldSpawnChild() {
		t.Error("ShouldSpawnChild should be true when the 24h check is due")
	}
}

func TestShouldSpawnChildWhenNeverChecked(t *testing.T) {
	setupStateDir(t)
	h := NewHook(testCfg())
	if !h.ShouldSpawnChild() {
		t.Error("ShouldSpawnChild should be true when no check has ever run")
	}
}

// --- Notice tests ---

func TestUpgradeNoticePrintsWhenNewerAvailable(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.LatestSeen = "v0.3.0"
	})
	h := NewHook(testCfg())
	var sb strings.Builder
	h.PrintPendingNotice(&sb)
	out := sb.String()
	if !strings.Contains(out, "v0.3.0 available") || !strings.Contains(out, "run 'upgrade-me slagctl'") {
		t.Errorf("expected upgrade notice naming the upgrade command, got %q", out)
	}
	if readToolState(testTool).LastUpgradeNudge.IsZero() {
		t.Error("printing the notice should stamp LastUpgradeNudge")
	}
}

func TestUpgradeNoticeRateLimited(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.LatestSeen = "v0.3.0"
		ts.LastUpgradeNudge = time.Now() // just nudged
	})
	h := NewHook(testCfg())
	var sb strings.Builder
	h.PrintPendingNotice(&sb)
	if sb.String() != "" {
		t.Errorf("expected no notice within nudgeInterval, got %q", sb.String())
	}
}

func TestNoUpgradeNoticeWhenCurrent(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.LatestSeen = "v0.2.0"
	})
	h := NewHook(testCfg())
	var sb strings.Builder
	h.PrintPendingNotice(&sb)
	if sb.String() != "" {
		t.Errorf("expected no notice when current, got %q", sb.String())
	}
}

func TestNoUpgradeNoticeForDevBuild(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.LatestSeen = "v0.3.0"
	})
	cfg := testCfg()
	cfg.CurrentVersion = "dev"
	h := NewHook(cfg)
	var sb strings.Builder
	h.PrintPendingNotice(&sb)
	if sb.String() != "" {
		t.Errorf("dev build should not nag, got %q", sb.String())
	}
}

func TestTokenNudgePrivateNoToken(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.SkipReason = "no-token"
	})
	cfg := testCfg()
	cfg.Visibility = RepoPrivate
	h := NewHook(cfg)
	var sb strings.Builder
	h.PrintPendingNotice(&sb)
	if !strings.Contains(sb.String(), "GitHub token required") {
		t.Errorf("expected token nudge, got %q", sb.String())
	}
	if readToolState(testTool).LastNudge.IsZero() {
		t.Error("printing the nudge should stamp LastNudge")
	}
}

func TestNoTokenNudgeForPublicRepo(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.SkipReason = "no-token"
	})
	cfg := testCfg()
	cfg.Visibility = RepoPublic
	h := NewHook(cfg)
	var sb strings.Builder
	h.PrintPendingNotice(&sb)
	if sb.String() != "" {
		t.Errorf("public repo should not emit the token nudge, got %q", sb.String())
	}
}

func TestHelperGuardSuppressesNotice(t *testing.T) {
	setupStateDir(t)
	t.Setenv(envHelperGuard, "1")
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.LatestSeen = "v0.3.0"
	})
	h := NewHook(testCfg())
	var sb strings.Builder
	h.PrintPendingNotice(&sb)
	if sb.String() != "" {
		t.Errorf("child (helper guard) should print nothing, got %q", sb.String())
	}
}

// --- Gate tests ---

func gateCmd(name string) *cobra.Command { return &cobra.Command{Use: name} }

func TestGateProceedsWhenCurrentMajorEqual(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) { ts.LatestSeen = "v0.9.0" })
	h := NewHook(testCfg())
	if err := h.EnforceMajorGate(gateCmd("get")); err != nil {
		t.Errorf("same-major (0.x) should proceed, got %v", err)
	}
}

func TestGateRefusesWhenMajorBehind(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) { ts.LatestSeen = "v1.0.0" })
	h := NewHook(testCfg())
	err := h.EnforceMajorGate(gateCmd("get"))
	if err == nil {
		t.Fatal("expected refusal when a full major behind")
	}
	if !strings.Contains(err.Error(), "major version behind") {
		t.Errorf("unexpected error text: %v", err)
	}
	if !strings.Contains(err.Error(), "'upgrade-me slagctl'") || !strings.Contains(err.Error(), envNoVersionGate+"=1") {
		t.Errorf("refusal should say how to upgrade and how to override, got %v", err)
	}
	if code, ok := clierr.Code(err); !ok || code != gateExitCode {
		t.Errorf("exit code = (%d, %t), want (%d, true)", code, ok, gateExitCode)
	}
}

func TestGateExemptCommandsProceedEvenWhenBehind(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) { ts.LatestSeen = "v1.0.0" })
	h := NewHook(testCfg())
	for _, name := range []string{HelperCommandName, "doctor", "version", "help", "completion"} {
		if err := h.EnforceMajorGate(gateCmd(name)); err != nil {
			t.Errorf("exempt command %q should proceed, got %v", name, err)
		}
	}
}

func TestGateDevBuildNeverRefuses(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) { ts.LatestSeen = "v1.0.0" })
	cfg := testCfg()
	cfg.CurrentVersion = "dev"
	h := NewHook(cfg)
	if err := h.EnforceMajorGate(gateCmd("get")); err != nil {
		t.Errorf("dev build should never be gated, got %v", err)
	}
}

func TestGateEnvOverride(t *testing.T) {
	setupStateDir(t)
	t.Setenv(envNoVersionGate, "1")
	_ = updateToolState(testTool, func(ts *ToolState) { ts.LatestSeen = "v1.0.0" })
	h := NewHook(testCfg())
	if err := h.EnforceMajorGate(gateCmd("get")); err != nil {
		t.Errorf("env override should bypass the gate, got %v", err)
	}
}

func TestGateIndeterminateFailOpenByDefault(t *testing.T) {
	setupStateDir(t) // no LatestSeen recorded
	h := NewHook(testCfg())
	if err := h.EnforceMajorGate(gateCmd("get")); err != nil {
		t.Errorf("fail-open default should proceed when latest unknown, got %v", err)
	}
}

func TestGateIndeterminateFailClosed(t *testing.T) {
	setupStateDir(t) // no LatestSeen recorded
	cfg := testCfg()
	cfg.FailClosedOnUnknown = true
	h := NewHook(cfg)
	if err := h.EnforceMajorGate(gateCmd("get")); err == nil {
		t.Error("fail-closed should refuse when latest unknown")
	}
}

func TestGateUnparseableLatestHonorsPosture(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) { ts.LatestSeen = "newest" })
	if err := NewHook(testCfg()).EnforceMajorGate(gateCmd("get")); err != nil {
		t.Errorf("fail-open should proceed on an unparseable latest, got %v", err)
	}
	cfg := testCfg()
	cfg.FailClosedOnUnknown = true
	if err := NewHook(cfg).EnforceMajorGate(gateCmd("get")); err == nil {
		t.Error("fail-closed should refuse on an unparseable latest")
	}
}

func TestGateStaleLatestStillTrusted(t *testing.T) {
	setupStateDir(t)
	// A month-old LatestSeen at the same major → proceed (no age horizon).
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.LatestSeen = "v0.5.0"
		ts.LatestSeenAt = time.Now().Add(-30 * 24 * time.Hour)
	})
	h := NewHook(testCfg())
	if err := h.EnforceMajorGate(gateCmd("get")); err != nil {
		t.Errorf("stale same-major LatestSeen should still proceed, got %v", err)
	}
}

// --- Discovery tests ---

// fakeReleasesLatest returns a server that serves releases/latest with the
// given tag, and rewires githubAPIBase to point at it for the test.
func fakeReleasesLatest(t *testing.T, tag string, status int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/fake-owner/fake-repo/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": tag})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	orig := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = orig })
}

func discoverTestCfg() Config {
	cfg := testCfg()
	cfg.Owner = "fake-owner"
	cfg.Repo = "fake-repo"
	cfg.Visibility = RepoPublic
	return cfg
}

func TestDiscoverLatestFound(t *testing.T) {
	fakeReleasesLatest(t, "v0.3.0", http.StatusOK)
	latest, found, err := discoverLatest(context.Background(), discoverTestCfg(), "tok")
	if err != nil {
		t.Fatalf("discoverLatest: %v", err)
	}
	if !found || latest != "v0.3.0" {
		t.Errorf("got (%q, %v), want (v0.3.0, true)", latest, found)
	}
}

func TestDiscoverLatestNotFound(t *testing.T) {
	fakeReleasesLatest(t, "", http.StatusNotFound)
	_, found, err := discoverLatest(context.Background(), discoverTestCfg(), "")
	if err != nil {
		t.Fatalf("404 should not be an error, got %v", err)
	}
	if found {
		t.Error("404 should yield found=false")
	}
}

func TestDiscoverLatestServerError(t *testing.T) {
	fakeReleasesLatest(t, "", http.StatusInternalServerError)
	_, found, err := discoverLatest(context.Background(), discoverTestCfg(), "tok")
	if err == nil {
		t.Error("500 should surface as an error")
	}
	if found {
		t.Error("found should be false on error")
	}
}

func TestRunHelperRecordsLatestSeen(t *testing.T) {
	setupStateDir(t)
	fakeReleasesLatest(t, "v0.3.0", http.StatusOK)
	runHelper(discoverTestCfg(), discoverLatest)
	ts := readToolState(testTool)
	if ts.LatestSeen != "v0.3.0" {
		t.Errorf("LatestSeen: got %q, want v0.3.0", ts.LatestSeen)
	}
	if ts.LastCheck.IsZero() {
		t.Error("LastCheck should be stamped")
	}
}

func TestRunHelperRecordsDiscoveryFailure(t *testing.T) {
	setupStateDir(t)
	fakeReleasesLatest(t, "", http.StatusInternalServerError)
	runHelper(discoverTestCfg(), discoverLatest)
	ts := readToolState(testTool)
	if !strings.Contains(ts.FailReason, "unexpected status 500") {
		t.Errorf("FailReason = %q, want the discovery error", ts.FailReason)
	}
	if ts.LastCheck.IsZero() {
		t.Error("LastCheck should be stamped on failure so the child does not retry every invocation")
	}
}

func TestRunHelperSkipsDiscoveryWhenRecent(t *testing.T) {
	setupStateDir(t)
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.LastCheck = time.Now()
		ts.LatestSeen = "v0.2.5"
	})
	called := false
	discover := func(_ context.Context, _ Config, _ string) (string, bool, error) {
		called = true
		return "v9.9.9", true, nil
	}
	runHelper(discoverTestCfg(), discover)
	if called {
		t.Error("discovery should be skipped when checked recently")
	}
	if readToolState(testTool).LatestSeen != "v0.2.5" {
		t.Error("LatestSeen should be unchanged when discovery is skipped")
	}
}

func TestRunHelperPrivateNoTokenRecordsSkip(t *testing.T) {
	setupStateDir(t)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir()) // no `gh` on PATH → resolveToken returns false
	cfg := discoverTestCfg()
	cfg.Visibility = RepoPrivate
	runHelper(cfg, discoverLatest)
	ts := readToolState(testTool)
	if ts.SkipReason != "no-token" {
		t.Errorf("expected no-token skip, got %q (fail=%q)", ts.SkipReason, ts.FailReason)
	}
}

// TestRunHelperPublicNoTokenDiscoversAnonymously pins the public-repo path
// with no GitHub token anywhere: the check still runs, sends no
// Authorization header, and records the latest release.
func TestRunHelperPublicNoTokenDiscoversAnonymously(t *testing.T) {
	setupStateDir(t)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", t.TempDir()) // no `gh` on PATH → resolveToken returns false

	var gotAuth []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/fake-owner/fake-repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.3.0"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	orig := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = orig })

	runHelper(discoverTestCfg(), discoverLatest)

	if len(gotAuth) != 1 {
		t.Fatalf("releases/latest requests = %d, want 1", len(gotAuth))
	}
	if gotAuth[0] != "" {
		t.Errorf("Authorization = %q, want none without a token", gotAuth[0])
	}
	ts := readToolState(testTool)
	if ts.LatestSeen != "v0.3.0" {
		t.Errorf("LatestSeen = %q, want v0.3.0 (skip=%q fail=%q)", ts.LatestSeen, ts.SkipReason, ts.FailReason)
	}
	if ts.SkipReason != "" {
		t.Errorf("SkipReason = %q, want none: a public repo needs no token", ts.SkipReason)
	}
}

func TestReadToolSummary(t *testing.T) {
	setupStateDir(t)
	if got := ReadToolSummary(testTool); got != "last check: never" {
		t.Errorf("summary with no state = %q", got)
	}
	_ = updateToolState(testTool, func(ts *ToolState) {
		ts.LastCheck = time.Now()
		ts.LatestSeen = "v0.3.0"
	})
	if got := ReadToolSummary(testTool); !strings.Contains(got, "latest: v0.3.0") {
		t.Errorf("summary = %q, want the latest version", got)
	}
	if latest, ok := LatestSeen(testTool); !ok || latest != "v0.3.0" {
		t.Errorf("LatestSeen = (%q, %t), want (v0.3.0, true)", latest, ok)
	}
}
