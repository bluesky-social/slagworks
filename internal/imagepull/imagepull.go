// Package imagepull downloads and extracts boot images (vmlinuz,
// initrd, squashfs) into a local cache dir. Both site workers (on
// startup/sync) and slagctl use this package; it has no dependencies
// beyond internal/model so slagctl doesn't drag in the full worker
// runtime.
//
// Initrd-overlay injection used to live here too. It now happens at
// build time (`scripts/image-build/build.sh` runs update-initramfs
// inside the chroot via mmdebstrap's --customize-hook), so the
// downloaded initrd is already slagworks-aware.
package imagepull

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/bluesky-social/slagworks/internal/model"
)

// versionLen is how many hex digits of a tarball's SHA256 name its version
// directory. 64 bits keeps two pins of one image apart, and the short form
// keeps slagworks.image= small on the kernel command line.
const versionLen = 16

// digestRE is the shape of an image row's sha256. Part of it becomes a
// directory name, so anything else is refused before it reaches a path.
var digestRE = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// versionDir is the directory name one version of image name is extracted
// into: <name>@<first 16 hex digits of its tarball's SHA256>. '@' cannot
// appear in an image name (model.ValidateImageName), so a version directory
// never collides with an image's name.
func versionDir(name, sha string) string {
	return name + "@" + strings.ToLower(sha[:versionLen])
}

// Installed returns the version directory that <imagesDir>/<name> links to,
// and false when <name> is not such a link: missing, a plain directory, or a
// link whose target is not one of name's version directories. The boot
// handler puts the version directory, not the name, into each boot script,
// so every file one boot fetches comes from the version that was current
// when its script was served.
func Installed(imagesDir, name string) (string, bool) {
	target, err := os.Readlink(filepath.Join(imagesDir, name))
	if err != nil || filepath.Base(target) != target || !strings.HasPrefix(target, name+"@") {
		return "", false
	}
	return target, true
}

// SyncImages checks cached image metadata against what's on disk and
// downloads any missing or outdated images.
//
// Each version of an image lives in its own directory,
// <imagesDir>/<name>@<sha16> (versionDir), and <imagesDir>/<name> is a
// symlink to the current one. A new version is downloaded,
// SHA256-verified, extracted and renamed into its version directory, and
// only then does the link switch to it (activate). The previous version's
// directory stays on disk, because a boot that started before the switch
// fetches its remaining files (root.squashfs from the BOOT hook, vmlinuz and
// initrd-local from the agent's bootloader step) from the version directory
// its boot script named. Older version directories are removed (prune), so
// a boot that spans two re-pins finds its files gone and fails instead of
// mixing versions. The link is the record of what is installed: an image is
// up to date when its link names the row's version directory and that
// directory exists. A failure before the switch leaves the link and every
// version directory as they were.
//
// Re-pinning the version the link was switched away from (a rollback) reuses
// its directory without downloading it again.
//
// Each name becomes <imagesDir>/<name>, which SyncImages replaces, so a
// name that fails model.ValidateImageName is skipped before anything is
// downloaded or touched: "." or ".." would otherwise point that replacement
// at imagesDir or its parent. A sha256 that is not 64 hex digits is skipped
// the same way, since part of it becomes a directory name (no such digest
// could match a download anyway). A skipped image does not stop the other
// images from syncing; it is reported in the returned error.
func SyncImages(log *slog.Logger, imagesDir string, images map[string]*model.Image) error {
	var refused []error
	for name, img := range images {
		if err := model.ValidateImageName(name); err != nil {
			refused = append(refused, fmt.Errorf("image not synced: %w; rename or delete the image row", err))
			continue
		}
		if img.URL == "" {
			continue
		}
		if !digestRE.MatchString(img.SHA256) {
			refused = append(refused, fmt.Errorf("image %s not synced: sha256 %q is not 64 hex digits; correct the image row's sha256", name, img.SHA256))
			continue
		}
		version := versionDir(name, img.SHA256)
		versionPath := filepath.Join(imagesDir, version)
		current, linked := Installed(imagesDir, name)
		if linked && current == version && isDir(versionPath) {
			log.Debug("image up to date", "name", name, "version", version)
			continue
		}

		if isDir(versionPath) {
			log.Info("image version already on disk, switching to it", "name", name, "version", version)
		} else {
			log.Info("downloading image", "name", name, "url", img.URL)
			if err := pullImage(log, img, versionPath); err != nil {
				return errors.Join(append(refused, fmt.Errorf("image %s: %w", name, err))...)
			}
		}
		if err := activate(imagesDir, name, version); err != nil {
			return errors.Join(append(refused, fmt.Errorf("image %s: %w", name, err))...)
		}
		keep := []string{version}
		if linked {
			keep = append(keep, current)
		}
		prune(log, imagesDir, name, keep)
		log.Info("image ready", "name", name, "sha256", img.SHA256, "version", version)
	}
	return errors.Join(refused...)
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// activate points <imagesDir>/<name> at the version directory version by
// renaming a new symlink over it. rename(2) replaces the link in one step,
// so a request resolving <name> meanwhile gets the old version or the new
// one, never a missing directory. A plain directory at <name> (put there by
// hand, or by a release that extracted images in place) cannot be renamed
// over, so it is moved aside first and removed once the link is in place.
func activate(imagesDir, name, version string) error {
	link := filepath.Join(imagesDir, name)
	tmp := filepath.Join(imagesDir, ".image-link-"+name)
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clearing stale link %s: %w", tmp, err)
	}
	if err := os.Symlink(version, tmp); err != nil {
		return fmt.Errorf("creating link to %s: %w", version, err)
	}
	var aside string
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
		if aside, err = os.MkdirTemp(imagesDir, ".image-old-*"); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("creating temp dir: %w", err)
		}
		if err := os.Rename(link, filepath.Join(aside, name)); err != nil {
			_ = os.Remove(tmp)
			_ = os.Remove(aside)
			return fmt.Errorf("moving aside %s: %w", link, err)
		}
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		if aside != "" {
			_ = os.Rename(filepath.Join(aside, name), link)
			_ = os.Remove(aside)
		}
		return fmt.Errorf("switching %s to %s: %w", link, version, err)
	}
	if aside != "" {
		_ = os.RemoveAll(aside) // best-effort; the link is already switched
	}
	return nil
}

// prune removes name's version directories other than keep. Each is renamed
// aside before it is deleted, so a version directory is complete or absent
// and a later pin of that version downloads it again rather than switching
// to a half-deleted tree. Failures are logged, not returned: the new version
// is already being served, and the next switch retries.
func prune(log *slog.Logger, imagesDir, name string, keep []string) {
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		log.Warn("could not list images dir to remove old image versions", "dir", imagesDir, "err", err)
		return
	}
	for _, e := range entries {
		old := e.Name()
		if !strings.HasPrefix(old, name+"@") || slices.Contains(keep, old) {
			continue
		}
		aside, err := os.MkdirTemp(imagesDir, ".image-old-*")
		if err != nil {
			log.Warn("could not remove old image version", "name", name, "version", old, "err", err)
			continue
		}
		if err := os.Rename(filepath.Join(imagesDir, old), filepath.Join(aside, old)); err != nil {
			log.Warn("could not remove old image version", "name", name, "version", old, "err", err)
		}
		if err := os.RemoveAll(aside); err != nil {
			log.Warn("could not delete removed image version", "name", name, "dir", aside, "err", err)
		}
	}
}

// minProgressBytes is the smallest download that emits progress snapshots
// (1 MiB). Smaller artifacts and unknown-length downloads, where the total
// byte count is unavailable, produce no progress logs. Completion is always
// signalled by the "image ready" log emitted by SyncImages.
const minProgressBytes int64 = 1 << 20

type progressWriter struct {
	name    string
	total   int64
	written int64
	log     *slog.Logger
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	prev := pw.written
	pw.written += int64(len(p))
	if pw.total < minProgressBytes {
		return len(p), nil
	}
	// Emit one snapshot per 20% threshold (20/40/60/80) crossed by this
	// write, at the exact threshold byte count and pct. Completion is
	// represented by the "image ready" log, so no 100% snapshot is emitted.
	// Because each threshold is checked against the bytes written *before*
	// this write, a write that skips past several thresholds emits each one
	// exactly once, and a write ending on an already-emitted threshold is
	// not repeated.
	for _, pct := range []int{20, 40, 60, 80} {
		thresholdBytes := pw.total * int64(pct) / 100
		if prev < thresholdBytes && thresholdBytes <= pw.written {
			pw.log.Info("image download progress",
				"name", pw.name,
				"pct", pct,
				"bytes_so_far", thresholdBytes,
				"bytes_total", pw.total,
			)
		}
	}
	return len(p), nil
}

// pullImage downloads img, verifies its SHA256, and extracts it into
// targetDir, which must not exist yet. On any failure targetDir is not
// created.
func pullImage(log *slog.Logger, img *model.Image, targetDir string) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(targetDir), ".image-download-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	resp, err := http.Get(img.URL) //nolint:gosec // URL comes from trusted global controller
	if err != nil {
		tmpFile.Close()
		return fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmpFile.Close()
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}

	hasher := sha256.New()
	pw := &progressWriter{name: img.Name, total: resp.ContentLength, log: log}
	if _, err := io.Copy(tmpFile, io.TeeReader(resp.Body, io.MultiWriter(hasher, pw))); err != nil {
		tmpFile.Close()
		return fmt.Errorf("writing download: %w", err)
	}
	tmpFile.Close()

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != img.SHA256 {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, img.SHA256)
	}

	tmpDir, err := os.MkdirTemp(filepath.Dir(targetDir), ".image-extract-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }() // best-effort cleanup

	if err := extractTarGz(tmpPath, tmpDir); err != nil {
		return fmt.Errorf("extracting: %w", err)
	}

	// The tarball contains <name>/{vmlinuz,initrd,root.squashfs}.
	// Find the extracted directory (skip any non-directory entries like PaxHeaders).
	entries, _ := os.ReadDir(tmpDir)
	var extractedDir string
	for _, e := range entries {
		if e.IsDir() {
			extractedDir = filepath.Join(tmpDir, e.Name())
			break
		}
	}
	if extractedDir == "" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		return fmt.Errorf("no directory found in tarball, got: %v", names)
	}

	if err := os.Rename(extractedDir, targetDir); err != nil {
		return fmt.Errorf("moving into place: %w", err)
	}
	return nil
}

func extractTarGz(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, hdr.Name)

		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(destDir)+string(os.PathSeparator)) &&
			filepath.Clean(target) != filepath.Clean(destDir) {
			return fmt.Errorf("path traversal in tarball: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
	return nil
}
