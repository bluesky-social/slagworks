package cliupdate

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/cli/clierr"
)

// Property-based coverage for the update notice and the major-version gate.
// The contracts are:
//   - the notice is offered only when the discovered release is strictly newer
//     than the running binary, and never for a dev or empty version
//   - the gate refuses exactly when the discovered major is ahead, never for a
//     dev build, always with the gate's exit code, and never for an exempt
//     command
//   - the rendered notice names the discovered tag and the running version,
//     and repeats no sooner than the nudge interval
//
// refSemver is an independent SemVer 2.0.0 parser/comparator (spec section
// 11), written from the spec rather than from Masterminds/semver, so the
// harness checks the package's decisions, not the library against itself.
// It accepts exactly the version shapes slagworks produces: release-please
// tags ("v0.5.0"), goreleaser {{.Version}} ("0.5.0"), prereleases and build
// metadata.
type refSemver struct {
	major, minor, patch int
	pre                 []string
}

func parseRef(s string) (refSemver, bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre []string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = strings.Split(s[i+1:], ".")
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return refSemver{}, false
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || (len(p) > 1 && p[0] == '0') {
			return refSemver{}, false
		}
		n[i] = v
	}
	return refSemver{n[0], n[1], n[2], pre}, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (a refSemver) cmp(b refSemver) int {
	if c := cmpInt(a.major, b.major); c != 0 {
		return c
	}
	if c := cmpInt(a.minor, b.minor); c != 0 {
		return c
	}
	if c := cmpInt(a.patch, b.patch); c != 0 {
		return c
	}
	// A version without prerelease outranks one with.
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0:
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		ai, aerr := strconv.Atoi(a.pre[i])
		bi, berr := strconv.Atoi(b.pre[i])
		switch {
		case aerr == nil && berr == nil:
			if c := cmpInt(ai, bi); c != 0 {
				return c
			}
		case aerr == nil:
			return -1 // numeric identifiers have lower precedence
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(a.pre[i], b.pre[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(a.pre), len(b.pre))
}

// genVersion draws from small pools so equal, adjacent and prerelease-vs-
// release pairs are common, plus the non-semver shapes slagctl really carries
// (a commit SHA from build-deb.sh, "dev", empty, garbage).
func genVersion(t *rapid.T, label string) string {
	kind := rapid.IntRange(0, 9).Draw(t, label+"_kind")
	switch kind {
	case 0:
		return rapid.SampledFrom([]string{"dev", "", "0123456789abcdef0123456789abcdef01234567", "garbage", "v", "1.2", "v01.2.3"}).Draw(t, label+"_odd")
	}
	v := fmt.Sprintf("%d.%d.%d",
		rapid.IntRange(0, 2).Draw(t, label+"_maj"),
		rapid.IntRange(0, 3).Draw(t, label+"_min"),
		rapid.IntRange(0, 3).Draw(t, label+"_pat"))
	if rapid.IntRange(0, 3).Draw(t, label+"_preRoll") == 0 {
		v += "-" + rapid.SampledFrom([]string{"rc.1", "rc.2", "rc.10", "alpha", "alpha.1", "beta", "1", "2", "next"}).Draw(t, label+"_pre")
	}
	if rapid.IntRange(0, 5).Draw(t, label+"_buildRoll") == 0 {
		v += "+" + rapid.SampledFrom([]string{"build.1", "sha.abc", "1"}).Draw(t, label+"_build")
	}
	if rapid.Bool().Draw(t, label+"_v") {
		v = "v" + v
	}
	return v
}

// TestHook_Property_UpgradeNoticeAndMajorGate checks the notice decision and
// the gate against refSemver for random pairs of running and discovered
// versions.
func TestHook_Property_UpgradeNoticeAndMajorGate(t *testing.T) {
	t.Setenv(envNoVersionGate, "")
	rapid.Check(t, func(t *rapid.T) {
		cur := genVersion(t, "cur")
		latest := genVersion(t, "latest")
		failClosed := rapid.Bool().Draw(t, "failClosed")
		h := &Hook{
			cfg: Config{Tool: "slagctl", CurrentVersion: cur, UpgradeCommand: "upgrade", FailClosedOnUnknown: failClosed},
			ts:  ToolState{LatestSeen: latest},
		}
		rc, okC := parseRef(cur)
		rl, okL := parseRef(latest)
		bothSemver := okC && okL && cur != "dev"

		// Notice: offered iff latest is strictly newer; never a downgrade or
		// a same-version nag.
		got := h.upgradeAvailable()
		if bothSemver {
			if want := rl.cmp(rc) > 0; got != want {
				t.Fatalf("upgradeAvailable(cur=%q, latest=%q) = %v, want %v", cur, latest, got, want)
			}
		} else if got && (cur == "dev" || cur == "" || latest == "") {
			t.Fatalf("upgradeAvailable(cur=%q, latest=%q) = true for a dev/empty version", cur, latest)
		}
		if got && okC && okL && rl.cmp(rc) <= 0 {
			t.Fatalf("notice would offer %q to a binary at %q (not newer)", latest, cur)
		}

		// Gate: refuses iff latest's major is ahead (definite case), and
		// never for dev builds.
		err := h.EnforceMajorGate(&cobra.Command{Use: "machine"})
		refused := err != nil
		if refused {
			if code, _ := clierr.Code(err); code != gateExitCode {
				t.Fatalf("gate refusal exit code = %d, want %d", code, gateExitCode)
			}
		}
		switch {
		case cur == "" || cur == "dev":
			if refused {
				t.Fatalf("gate refused a dev build (cur=%q latest=%q)", cur, latest)
			}
		case bothSemver:
			if want := rl.major > rc.major; refused != want {
				t.Fatalf("gate(cur=%q, latest=%q) refused=%v, want %v", cur, latest, refused, want)
			}
		}
		// Exempt commands always run.
		for _, name := range []string{"doctor", "version", "help", HelperCommandName} {
			if err := h.EnforceMajorGate(&cobra.Command{Use: name}); err != nil {
				t.Fatalf("exempt command %q refused: %v", name, err)
			}
		}
	})
}

// TestHook_Property_NoticeRateLimit: the rendered notice names exactly the
// discovered tag and the running version, and is rate limited.
func TestHook_Property_NoticeRateLimit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(envHelperGuard, "")
	rapid.Check(t, func(t *rapid.T) {
		lastNudgeAgo := time.Duration(rapid.IntRange(0, 14*24).Draw(t, "hoursAgo")) * time.Hour
		h := &Hook{
			cfg: Config{Tool: "slagctl-prop", CurrentVersion: "0.4.1", UpgradeCommand: "brew upgrade slagctl"},
			ts:  ToolState{LatestSeen: "v0.5.0", LastUpgradeNudge: time.Now().Add(-lastNudgeAgo)},
		}
		var buf bytes.Buffer
		h.PrintPendingNotice(&buf)
		printed := buf.Len() > 0
		if want := lastNudgeAgo >= nudgeInterval; printed != want {
			t.Fatalf("last nudge %s ago: printed=%v, want %v (%q)", lastNudgeAgo, printed, want, buf.String())
		}
		if printed && !strings.Contains(buf.String(), "v0.5.0 available (you have 0.4.1)") {
			t.Fatalf("notice text %q", buf.String())
		}
	})
}
