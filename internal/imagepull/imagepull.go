// Package imagepull downloads and extracts boot images (vmlinuz,
// initrd, initrd-local, squashfs) into a local cache dir. Both site
// workers (on startup/sync) and slagctl use this package; it has no
// dependencies beyond internal/model so slagctl doesn't drag in the full
// worker runtime.
//
// Initrd-overlay injection used to live here too. It now happens at
// build time (`scripts/image-build/build.sh` runs update-initramfs
// inside the chroot via mmdebstrap's --customize-hook), so the
// downloaded initrd is already slagworks-aware.
package imagepull

import (
	"archive/tar"
	"compress/gzip"
	"context"
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
	"syscall"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

// versionLen is how many hex digits of a tarball's SHA256 name its version
// directory. 64 bits keeps two pins of one image apart, and the short form
// keeps slagworks.image= small on the kernel command line.
const versionLen = 16

// digestRE is the shape of an image row's sha256 once SyncImages has
// lowercased and trimmed it. Part of it becomes a
// directory name, so anything else is refused before it reaches a path.
var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// versionDir is the directory name one version of image name is extracted
// into: <name>@<first 16 hex digits of its tarball's SHA256>, sha being the
// lowercase digest. '@' cannot
// appear in an image name (model.ValidateImageName), so a version directory
// never collides with an image's name.
func versionDir(name, sha string) string {
	return name + "@" + sha[:versionLen]
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
// SHA256-verified, extracted, checked for every file in imageFiles and
// renamed into its version directory, and
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
// could match a download anyway).
//
// Images are independent (each has its own link and version directories),
// so a skipped image, a failed download, a digest mismatch or a failed link
// switch costs only that image: the other images still sync, and every
// failure is reported in the returned error. Stopping at the first failure
// would make which images synced depend on map iteration order.
//
// The row's SHA256 is whatever string was written to the API, which
// checks only that it is non-empty. A hex digest names the same tarball in
// either case, so SyncImages uses its lowercase form, trimmed, for the
// version directory and the download check: the spelling
// hex.EncodeToString produces for the downloaded bytes.
//
// SyncImages holds an exclusive lock on imagesDir for its whole run (see
// lockImagesDir), so syncs of one dir run one at a time across processes,
// and first removes the temp files that a sync which exited mid-pull left
// behind (see removeLeftovers). If the lock cannot be taken it logs a warning
// and syncs without removing anything.
func SyncImages(log *slog.Logger, imagesDir string, images map[string]*model.Image) error {
	if unlock, err := lockImagesDir(log, imagesDir); err != nil {
		log.Warn("could not lock the images dir: syncing without removing leftovers of interrupted syncs, and without waiting for any other sync of this dir; check that the dir exists and its filesystem supports flock",
			"dir", imagesDir, "err", err)
	} else {
		defer unlock()
		removeLeftovers(log, imagesDir)
	}

	var errs []error
	for name, img := range images {
		if err := model.ValidateImageName(name); err != nil {
			errs = append(errs, fmt.Errorf("image not synced: %w; rename or delete the image row", err))
			continue
		}
		if img.URL == "" {
			continue
		}
		want := strings.ToLower(strings.TrimSpace(img.SHA256))
		if !digestRE.MatchString(want) {
			errs = append(errs, fmt.Errorf("image %s not synced: sha256 %q is not 64 hex digits; correct the image row's sha256", name, img.SHA256))
			continue
		}
		version := versionDir(name, want)
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
			if err := pullImage(log, img, want, versionPath); err != nil {
				errs = append(errs, fmt.Errorf("image %s: %w", name, err))
				continue
			}
		}
		if err := activate(imagesDir, name, version); err != nil {
			errs = append(errs, fmt.Errorf("image %s: %w", name, err))
			continue
		}
		keep := []string{version}
		if linked {
			keep = append(keep, current)
		}
		prune(log, imagesDir, name, keep)
		log.Info("image ready", "name", name, "sha256", want, "version", version)
	}
	return errors.Join(errs...)
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
	tmp := filepath.Join(imagesDir, linkTempPrefix+name)
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clearing stale link %s: %w", tmp, err)
	}
	if err := os.Symlink(version, tmp); err != nil {
		return fmt.Errorf("creating link to %s: %w", version, err)
	}
	var aside string
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&fs.ModeSymlink == 0 {
		if aside, err = os.MkdirTemp(imagesDir, oldTempPrefix+"*"); err != nil {
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
		aside, err := os.MkdirTemp(imagesDir, oldTempPrefix+"*")
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

// fetchError is a failure to fetch an image from its URL: a transport error,
// a response other than 200, a body cut short, or a download abandoned
// because it stalled.
type fetchError struct{ err error }

func (e *fetchError) Error() string { return e.err.Error() }
func (e *fetchError) Unwrap() error { return e.err }

// Retryable reports whether a SyncImages error includes a failure to fetch
// an image from its URL (a transport error, a response other than 200, a
// body cut short, or a stalled download), which the same URL may not repeat
// on a later attempt.
// Every other failure repeats until the image row or the host changes:
// bytes that do not match the row's digest or do not extract (a later
// attempt downloads the same bytes again), a refused name, or an error
// writing to the images directory.
func Retryable(err error) bool {
	var fe *fetchError
	return errors.As(err, &fe)
}

// readErrRecorder remembers the last non-EOF error its reader returned, so
// a failed copy can tell a body cut short from a failed write to disk.
type readErrRecorder struct {
	r   io.Reader
	err error
}

func (rr *readErrRecorder) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	if err != nil && err != io.EOF {
		rr.err = err
	}
	return n, err
}

// Temp names a sync creates directly in imagesDir and removes itself when
// it runs to the end: pullImage downloads into a file named
// downloadTempPrefix+"*" and extracts into a dir named extractTempPrefix+"*",
// activate builds the new link at linkTempPrefix+<name>, and activate and
// prune move what they delete into a dir named oldTempPrefix+"*" first. No
// image link or version directory can start with any of them:
// model.ValidateImageName requires a leading letter or digit.
const (
	downloadTempPrefix = ".image-download-"
	extractTempPrefix  = ".image-extract-"
	linkTempPrefix     = ".image-link-"
	oldTempPrefix      = ".image-old-"
)

var tempPrefixes = []string{downloadTempPrefix, extractTempPrefix, linkTempPrefix, oldTempPrefix}

// msgWaitingForLock is logged when SyncImages finds another sync holding the
// images dir lock and starts waiting for it.
const msgWaitingForLock = "another image sync is running in this images dir; waiting for it to finish"

// lockImagesDir takes an exclusive flock on imagesDir itself and returns the
// function that releases it. A sync in another process (the site worker and
// `slagctl image sync --dir` pointed at the same dir) or another goroutine
// holding it makes this wait, after logging msgWaitingForLock. The kernel
// releases the lock when its holder exits, however it exits, so a temp file
// seen while holding the lock belongs to a sync that is no longer running.
// The directory itself is locked rather than a lock file in it so the sync
// adds nothing to the dir the site worker serves.
//
// It waits rather than returning "busy" because the site worker retries
// only a sync that failed to fetch, so a skipped sync could leave it serving
// a stale image until its next snapshot change. The wait is normally no
// longer than the download the worker would otherwise run itself (both sides
// pull the same rows), accepting that a holder that hangs or is stopped (an
// operator's slagctl image sync) also holds up the worker's image sync, and
// the commission and deploy ops waiting on it, until it exits.
func lockImagesDir(log *slog.Logger, imagesDir string) (unlock func(), err error) {
	d, err := os.Open(imagesDir)
	if err != nil {
		return nil, err
	}
	fd := int(d.Fd())
	err = flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		log.Info(msgWaitingForLock, "dir", imagesDir)
		err = flock(fd, syscall.LOCK_EX)
	}
	if err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("flock %s: %w", imagesDir, err)
	}
	// Closing the only descriptor for the open file releases the lock.
	return func() { _ = d.Close() }, nil
}

func flock(fd, how int) error {
	for {
		if err := syscall.Flock(fd, how); !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// removeLeftovers removes the temp entries (tempPrefixes) a sync left in
// imagesDir when its process exited before it removed them itself
// (SIGKILL, OOM, or a SIGTERM that ends the process mid-pull). The caller
// holds the images dir lock, so none of them belongs to a running sync. A
// failure is logged and does not stop the sync.
func removeLeftovers(log *slog.Logger, imagesDir string) {
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		log.Warn("could not list the images dir to remove leftovers of interrupted syncs", "dir", imagesDir, "err", err)
		return
	}
	for _, e := range entries {
		if !slices.ContainsFunc(tempPrefixes, func(p string) bool { return strings.HasPrefix(e.Name(), p) }) {
			continue
		}
		p := filepath.Join(imagesDir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			log.Warn("could not remove a leftover of an interrupted image sync; remove it by hand", "path", p, "err", err)
			continue
		}
		log.Info("removed a leftover of an interrupted image sync", "path", p)
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

// stallTimeout is how long a download may go without receiving any data,
// response headers included, before pullImage abandons it. A slow download
// that keeps moving is never cut off. Without it, a connection that stopped
// delivering would hold the site worker's image sync, and with it every
// later image set and the worker's commission and deploy ops, until the
// process restarted. A var so tests can shorten it.
var stallTimeout = time.Minute

// stallReader re-arms the stall timer each time a read returns data.
type stallReader struct {
	r     io.Reader
	timer *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 {
		s.timer.Reset(stallTimeout)
	}
	return n, err
}

// imageFiles are the files every image tarball carries in its top-level
// directory (docs/user/images.md), all of which the site worker serves
// under /files/images/<version directory>/. build.sh's sanity-check loop
// and scripts/image-build/manifest_test.go keep the same list.
var imageFiles = []string{"vmlinuz", "initrd", "initrd-local", "root.squashfs"}

// pullImage downloads img.URL, checks it against wantSHA256 (lowercase hex,
// as SyncImages normalizes it), and extracts it into targetDir, which must
// not exist yet. On any failure targetDir is not created.
func pullImage(log *slog.Logger, img *model.Image, wantSHA256, targetDir string) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(targetDir), downloadTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	stall := time.AfterFunc(stallTimeout, func() {
		cancel(fmt.Errorf("download stalled: no data received for %s; check that the image URL is reachable from this host", stallTimeout))
	})
	defer stall.Stop()
	// downloadErr reports the stall rather than the context cancellation it
	// caused.
	downloadErr := func(err error) error {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, img.URL, nil)
	if err != nil {
		tmpFile.Close()
		return fmt.Errorf("downloading: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tmpFile.Close()
		return &fetchError{fmt.Errorf("downloading: %w", downloadErr(err))}
	}
	defer resp.Body.Close()
	stall.Reset(stallTimeout) // the response headers are data
	if resp.StatusCode != http.StatusOK {
		tmpFile.Close()
		return &fetchError{fmt.Errorf("download returned %d", resp.StatusCode)}
	}

	hasher := sha256.New()
	pw := &progressWriter{name: img.Name, total: resp.ContentLength, log: log}
	body := &readErrRecorder{r: &stallReader{r: resp.Body, timer: stall}}
	if _, err := io.Copy(tmpFile, io.TeeReader(body, io.MultiWriter(hasher, pw))); err != nil {
		tmpFile.Close()
		if body.err != nil {
			return &fetchError{fmt.Errorf("reading download: %w", downloadErr(err))}
		}
		return fmt.Errorf("writing download: %w", err)
	}
	tmpFile.Close()

	got := hex.EncodeToString(hasher.Sum(nil))
	if got != wantSHA256 {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, wantSHA256)
	}

	tmpDir, err := os.MkdirTemp(filepath.Dir(targetDir), extractTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }() // best-effort cleanup

	if err := extractTarGz(tmpPath, tmpDir); err != nil {
		return fmt.Errorf("extracting: %w", err)
	}

	// The tarball contains <name>/{vmlinuz,initrd,initrd-local,root.squashfs}.
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

	// A tarball that matches its digest can still lack a file the worker
	// serves. Installed, it would look synced and the gap would show only
	// when a boot fetched the missing file: for initrd-local, a UEFI
	// deploy's installing_bootloader, after the target's disks were
	// partitioned and rsynced. Refuse it here, before the version directory
	// exists, so the link stays on the previous version.
	for _, f := range imageFiles {
		if fi, err := os.Lstat(filepath.Join(extractedDir, f)); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("tarball has no regular file %s/%s (an image carries %s); the cached image, if any, is unchanged: pin a complete image",
				filepath.Base(extractedDir), f, strings.Join(imageFiles, ", "))
		}
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
			// A path can appear more than once in a tarball (tar -r appends
			// a replacement); as with tar(1), the later entry is the file.
			// Remove the earlier entry's file so this one is created with
			// its own bytes and mode: writing into the old file would keep
			// its tail when this entry is shorter, keep its mode, and fail
			// for a non-root caller when it is read-only.
			if fi, err := os.Lstat(target); err == nil && fi.Mode().IsRegular() {
				if err := os.Remove(target); err != nil {
					return err
				}
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(hdr.Mode))
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
