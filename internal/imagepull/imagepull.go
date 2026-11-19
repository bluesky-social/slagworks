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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bluesky-social/slagworks/internal/model"
)

// SyncImages checks cached image metadata against what's on disk and downloads
// any missing or outdated images. Each image tarball is downloaded,
// SHA256-verified, extracted, and atomically renamed into place.
//
// The .sha256 marker is written ONLY after the full pipeline succeeds.
// An earlier version wrote the marker inside pullImage, which would
// have left a broken image behind with a valid-looking marker on
// failure. The repack-on-pull step that this comment originally
// documented is gone: build.sh now bakes the slagworks BOOT hook
// into the initrd at image-build time, so what we download is what
// we serve.
//
// Each name becomes <imagesDir>/<name>, which pullImage removes and
// replaces, so a name that fails model.ValidateImageName is skipped
// before anything is downloaded or touched: "." or ".." would otherwise
// point that removal at imagesDir or its parent. A skipped name does not
// stop the other images from syncing; it is reported in the returned
// error.
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
		target := filepath.Join(imagesDir, name)
		markerPath := filepath.Join(imagesDir, name+".sha256")

		if marker, err := os.ReadFile(markerPath); err == nil {
			if strings.TrimSpace(string(marker)) == img.SHA256 {
				log.Debug("image up to date", "name", name)
				continue
			}
		}

		log.Info("downloading image", "name", name, "url", img.URL)
		if err := pullImage(log, img, target); err != nil {
			return errors.Join(append(refused, fmt.Errorf("image %s: %w", name, err))...)
		}
		// Marker is the last write — a transient failure before this
		// point leaves the previous (intact) cache for rollback.
		if err := os.WriteFile(markerPath, []byte(img.SHA256+"\n"), 0644); err != nil {
			return errors.Join(append(refused, fmt.Errorf("image %s marker: %w", name, err))...)
		}
		log.Info("image ready", "name", name, "sha256", img.SHA256)
	}
	return errors.Join(refused...)
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

func pullImage(log *slog.Logger, img *model.Image, targetDir string) error {
	markerPath := targetDir + ".sha256"
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

	// Point of no return: we're about to delete the existing
	// target dir. Clear the marker at the same moment so a
	// mid-pipeline failure doesn't leave a stale marker pointing
	// at a half-overwritten target dir. The caller writes the
	// marker back at the end of a successful pipeline. Placing
	// the removal HERE (not at the top of SyncImages) means a
	// transient download failure before this point leaves the
	// entire previous cache — marker and target — intact for
	// rollback.
	_ = os.Remove(markerPath)
	_ = os.RemoveAll(targetDir) // best-effort, may not exist
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
