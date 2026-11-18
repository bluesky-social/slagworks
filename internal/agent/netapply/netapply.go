// Package netapply realizes the deployed host's network identity on
// /target: runs the site-matched network-config script and applies its
// file+unit bundle, writes the fallback DHCP netplan when no script
// matched, and sets the hostname from NetBox.
package netapply

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	securejoin "github.com/cyphar/filepath-securejoin"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/agent/execkit"
	"github.com/bluesky-social/slagworks/internal/agent/netlib"
)

// netBundle is the file+unit bundle a network-config script emits on
// stdout. slagworks never constructs this shape — the script produces it,
// the agent applies it. Contract: docs/dev/extensibility.md ("Netscript
// contract").
type netBundle struct {
	Files       []netBundleFile `json:"files"`
	EnableUnits []string        `json:"enable_units,omitempty"`
}

type netBundleFile struct {
	Path    string `json:"path"` // absolute, inside the target root
	Content string `json:"content"`
	Mode    string `json:"mode,omitempty"`  // octal string, default "0644"
	Owner   string `json:"owner,omitempty"` // resolved in the target's passwd db
	Group   string `json:"group,omitempty"`
}

// ApplyNetworkConfig configures the deployed host's network. It mirrors the
// storage-script seam: the site worker serves a pre-chosen network-config
// script (matched on NetBox identity) plus the machine's raw NetBox
// interface facts; the agent runs the script in the deploy initrd with
// SLAGWORKS_NETWORK_FACTS pointing at the facts JSON, and applies the
// emitted file+unit bundle to /target.
//
// Failure semantics mirror storageapply.ResolveLayout's hard-fail:
//   - 204 (no script matched) or empty machineID (dev/stdout path): write
//     the built-in all-en*-DHCP netplan and return. The un-modeled host
//     still comes up.
//   - script matched but execution fails / emits invalid JSON / a path
//     escapes /target: FAIL the deploy. A host that should get L3 netplan +
//     FRR silently coming up DHCP-only is a latent fabric break — the same
//     loud-fail-over-silent-wrong-state reasoning as the storage wrong-disk
//     class.
func ApplyNetworkConfig(ctx *agent.Context, machineID string) error {
	ctx.Log.Info("applying network config")

	if machineID == "" {
		ctx.Log.Info("no machine ID; using default all-en* DHCP netplan")
		return WriteDefaultNetplan(ctx)
	}

	scriptURL := fmt.Sprintf("http://%s/api/v1/machines/%s/network-script", ctx.Server, machineID)
	scriptContent, err := agent.HTTPGet(scriptURL)
	if err != nil {
		return fmt.Errorf("fetch network script for machine %s: %w "+
			"(check the site worker is reachable at %s)", machineID, err, ctx.Server)
	}
	if len(scriptContent) == 0 {
		ctx.Log.Info("no network script matched; using default all-en* DHCP netplan")
		return WriteDefaultNetplan(ctx)
	}

	// Fetch the machine's NetBox interface facts. The script does all
	// topology interpretation (/31 legs, loopback, FRR) from these.
	factsURL := fmt.Sprintf("http://%s/api/v1/machines/%s/network-facts", ctx.Server, machineID)
	facts, err := agent.HTTPGet(factsURL)
	if err != nil {
		return fmt.Errorf("fetch network facts for machine %s: %w", machineID, err)
	}
	if len(facts) == 0 {
		facts = []byte("[]")
	}

	// Write the script + facts to temp files and run it; the shared harness
	// injects SLAGWORKS_NETWORK_FACTS and returns stdout. The netlib sidecar
	// is co-located with the script so its `import slagworks_netlib` resolves
	// against the script's own directory.
	sidecars := map[string][]byte{netlib.ModuleName + ".py": netlib.Source}
	bundleJSON, err := execkit.RunConfigScript(ctx.Log, "network", scriptContent, "SLAGWORKS_NETWORK_FACTS", facts, sidecars)
	if err != nil {
		return err
	}

	var bundle netBundle
	if err := json.Unmarshal(bundleJSON, &bundle); err != nil {
		return fmt.Errorf("parse bundle JSON from network script: %w", err)
	}
	if err := applyNetBundle(ctx.Log, bundle, "/target", ctx.Exec.Run); err != nil {
		return fmt.Errorf("apply network bundle: %w", err)
	}
	ctx.Log.Info("network config applied")
	return nil
}

// parseBundleMode parses a bundle file's octal mode string into an
// os.FileMode, defaulting to 0644 when empty. Go's os.FileMode places the
// setuid/setgid/sticky bits at high flag positions (1<<23 / 1<<22 / 1<<20),
// NOT at the Unix octal positions (0o4000 / 0o2000 / 0o1000) — a plain
// os.FileMode(parsed) cast silently drops them, so they're mapped explicitly.
func parseBundleMode(s string) (os.FileMode, error) {
	if s == "" {
		return 0644, nil
	}
	parsed, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid octal mode %q: %w", s, err)
	}
	mode := os.FileMode(parsed & 0o777)
	if parsed&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if parsed&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if parsed&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	return mode, nil
}

// applyNetBundle writes each bundle file into the target root (symlink-
// resolved and contained), applies mode/owner/group, and enables each unit
// via the target's own systemctl through chroot. The command runner is
// injected so the decision logic (path containment, mode parsing, chown spec,
// unit enable, bind-mount gating) is unit-testable without root or a real
// chroot — runner sees exactly the chroot/mount/umount commands that would run.
func applyNetBundle(log logger, bundle netBundle, target string, runner func(string, ...string) error) error {
	// Write all files (and chown them) FIRST, before any bind mounts exist.
	// This is deliberate ordering, not just convenience: systemctl enable
	// below bind-mounts the host's /dev /proc /sys into the target, and once
	// those are live a bundle path like /sys/kernel/core_pattern would
	// SecureJoin to /target/sys/... — lexically contained, but the bind mount
	// makes that the host's live sysfs, so the write would escape containment.
	// Doing every os.WriteFile before the mounts go up removes that window
	// entirely. (chown runs through chroot but needs no pseudo-fs.)
	for _, f := range bundle.Files {
		if !filepath.IsAbs(f.Path) {
			return fmt.Errorf("bundle file path %q is not absolute", f.Path)
		}
		// Resolve under the target root WITH symlink evaluation, then confirm
		// containment. Lexical Join+Clean is insufficient: the stock image
		// ships /etc symlinks a buggy script's path could ride out of /target.
		resolved, err := securejoin.SecureJoin(target, f.Path)
		if err != nil {
			return fmt.Errorf("resolve %q under target: %w", f.Path, err)
		}

		mode, err := parseBundleMode(f.Mode)
		if err != nil {
			return fmt.Errorf("bundle file %q: %w", f.Path, err)
		}

		if err := os.MkdirAll(filepath.Dir(resolved), 0755); err != nil {
			return fmt.Errorf("mkdir parent of %q: %w", f.Path, err)
		}
		if err := os.WriteFile(resolved, []byte(f.Content), mode); err != nil {
			return fmt.Errorf("write %q: %w", f.Path, err)
		}

		// owner/group resolve against the TARGET's passwd/group db, so chown
		// runs through chroot. The frr user is baked into the image at build
		// time (Phase 3 prereq); a missing user fails loudly here rather than
		// silently leaving root-owned config. The "--" stops chown parsing an
		// owner that starts with "-" (e.g. "--reference=...") as an option.
		if f.Owner != "" || f.Group != "" {
			spec := f.Owner + ":" + f.Group
			if f.Group == "" {
				spec = f.Owner
			}
			if err := runner("chroot", target, "chown", "--", spec, f.Path); err != nil {
				return fmt.Errorf("chown %s %q: %w", spec, f.Path, err)
			}
		}

		// Chmod LAST, after any chown. The kernel strips setuid/setgid on the
		// create open(2) AND on chown(2) (even for root, since Linux 2.2.13),
		// so the special bits the bundle asked for must be re-applied after
		// the owner change or they're silently lost. Doing it unconditionally
		// also pins regular bits against the process umask on os.WriteFile.
		if err := os.Chmod(resolved, mode); err != nil {
			return fmt.Errorf("chmod %q to %v: %w", f.Path, mode, err)
		}
		log.Info("wrote bundle file", "path", f.Path, "mode", fmt.Sprintf("%o", mode))
	}

	// systemctl enable needs the target's /proc /sys /dev. Bind them in for
	// the enable phase only; the agent already uses this pattern in
	// target.ConfigureRAID. A files-only bundle (the common netplan-only
	// case) touches no chroot and needs no bind mounts.
	if len(bundle.EnableUnits) > 0 {
		for _, m := range []struct{ src, dst string }{
			{"/dev", target + "/dev"},
			{"/proc", target + "/proc"},
			{"/sys", target + "/sys"},
		} {
			if err := os.MkdirAll(m.dst, 0755); err != nil {
				return fmt.Errorf("create bind-mount target %s: %w", m.dst, err)
			}
			if err := runner("mount", "--bind", m.src, m.dst); err != nil {
				return fmt.Errorf("bind-mount %s: %w", m.dst, err)
			}
		}
		defer func() {
			for _, dst := range []string{target + "/sys", target + "/proc", target + "/dev"} {
				_ = runner("umount", dst)
			}
		}()
	}

	for _, unit := range bundle.EnableUnits {
		// Run the target OS's own systemctl against its own units so the
		// [Install] section's WantedBy resolves correctly — not systemctl
		// --root from the initrd (no guarantee systemctl is present there,
		// and version skew is a hazard). The "enable --" stops a unit name
		// beginning with "-" being parsed as a systemctl option.
		if err := runner("chroot", target, "systemctl", "enable", "--", unit); err != nil {
			return fmt.Errorf("enable unit %q: %w", unit, err)
		}
		log.Info("enabled unit", "unit", unit)
	}
	return nil
}

// logger is the slice of *slog.Logger applyNetBundle needs; an
// interface so tests can pass slog.Default() or any compatible logger
// without importing slog machinery here.
type logger interface {
	Info(msg string, args ...any)
}

// WriteDefaultNetplan drops a static netplan config that brings every real
// ethernet up via DHCP. It is the fallback when no network script matched
// (204) and for the dev/stdout path. Replaces cloud-init's fallback
// rendering, which was a coincidental side effect we relied on without
// realizing it.
//
// `match: name: en*` is broad on purpose: an un-modeled deployed system has
// no operator-specific NIC layout we want to encode here, and
// systemd-networkd-style match-by-name is stable across reboots (unlike
// "first carrier up" heuristics). Loopback is implicit.
func WriteDefaultNetplan(ctx *agent.Context) error {
	dir := "/target/etc/netplan"
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir netplan: %w", err)
	}
	// 0600: netplan emits a CLI warning ("file too open") if the
	// config is world-readable. Cosmetic but pollutes logs.
	if err := os.WriteFile(dir+"/99-slagworks.yaml", []byte(defaultNetplan), 0600); err != nil {
		return fmt.Errorf("write netplan: %w", err)
	}
	ctx.Log.Info("default netplan written")
	return nil
}

// defaultNetplan is the fallback when no network script matched (204 from the
// network-script endpoint), and for the dev/stdout path. Brings every real
// ethernet up via DHCP — the original behavior. cloud-init
// network rendering is disabled (target.SeedCloudInit), so this file is the
// sole network config on an un-modeled deployed host.
const defaultNetplan = `# slagworks deploy agent — replaces cloud-init's fallback
# network rendering. See internal/agent/netapply WriteDefaultNetplan.
network:
  version: 2
  renderer: networkd
  ethernets:
    slagworks-en:
      match:
        name: "en*"
      dhcp4: true
      dhcp6: false
`

// WriteHostname sets the target's hostname from NetBox. The server derives the
// hostname from the NetBox device name through its configured template (e.g.
// "kworker-l34-1.pop1" -> "kworker-l34-1.internal.pop1.example.net", or the
// device name unchanged with no template) and serves it at /hostname; the
// agent writes /etc/hostname (FQDN) and a /etc/hosts 127.0.1.1 line (the
// Debian/Ubuntu convention so `hostname -f` resolves before DNS is up). A 204
// (empty body) means no FQDN is modeled — leave the image default. Empty
// machineID is the dev/stdout path; skip.
func WriteHostname(ctx *agent.Context, machineID string) error {
	if machineID == "" {
		return nil
	}
	url := fmt.Sprintf("http://%s/api/v1/machines/%s/hostname", ctx.Server, machineID)
	body, err := agent.HTTPGet(url)
	if err != nil {
		return fmt.Errorf("fetch hostname: %w", err)
	}
	fqdn := strings.TrimSpace(string(body))
	if fqdn == "" {
		ctx.Log.Info("no FQDN modeled; leaving image default hostname")
		return nil
	}
	ctx.Log.Info("setting hostname", "fqdn", fqdn)

	if err := os.WriteFile("/target/etc/hostname", []byte(fqdn+"\n"), 0644); err != nil {
		return fmt.Errorf("write /etc/hostname: %w", err)
	}

	// /etc/hosts: map 127.0.1.1 to the FQDN + short name (Debian convention),
	// so name resolution works before networkd/DNS is up. Rewrite any existing
	// 127.0.1.1 line; preserve the rest of the file.
	short := fqdn
	if i := strings.IndexByte(fqdn, '.'); i > 0 {
		short = fqdn[:i]
	}
	hostsLine := fmt.Sprintf("127.0.1.1\t%s %s", fqdn, short)
	existing, _ := os.ReadFile("/target/etc/hosts")
	var out []string
	replaced := false
	for ln := range strings.SplitSeq(string(existing), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "127.0.1.1") {
			out = append(out, hostsLine)
			replaced = true
		} else {
			out = append(out, ln)
		}
	}
	if !replaced {
		// Insert after the 127.0.0.1 localhost line if present, else append.
		body := strings.Join(out, "\n")
		if strings.Contains(body, "127.0.0.1") {
			out = nil
			for ln := range strings.SplitSeq(body, "\n") {
				out = append(out, ln)
				if strings.HasPrefix(strings.TrimSpace(ln), "127.0.0.1") {
					out = append(out, hostsLine)
				}
			}
		} else {
			out = append(out, hostsLine)
		}
	}
	// gosec G703 flags this as path-traversal because `out` is derived from
	// the fetched FQDN — but the PATH is a hardcoded constant; only the file
	// CONTENT comes from the server (the FQDN, same trust model as the
	// netplan/kernel-params we already write). The path is never tainted.
	if err := os.WriteFile("/target/etc/hosts", []byte(strings.Join(out, "\n")), 0644); err != nil { //nolint:gosec // constant path; content is the server-provided FQDN
		return fmt.Errorf("write /etc/hosts: %w", err)
	}
	ctx.Log.Info("hostname written")
	return nil
}
