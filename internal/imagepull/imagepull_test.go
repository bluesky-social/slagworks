package imagepull

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

// captureHandler records every slog record for inspection.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) snapshot() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]slog.Record, len(h.records))
	copy(out, h.records)
	return out
}

// progressAttrs flattens a record's attributes into a map for assertions.
func progressAttrs(r slog.Record) map[string]any {
	attrs := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	return attrs
}

var progressThresholds = []int{20, 40, 60, 80}

// progressRecords returns only the "image download progress" records.
func progressRecords(cap *captureHandler) []slog.Record {
	var out []slog.Record
	for _, r := range cap.snapshot() {
		if r.Message == "image download progress" {
			out = append(out, r)
		}
	}
	return out
}

func TestProgressWriter_EmitsThresholds(t *testing.T) {
	cap := &captureHandler{}
	const total int64 = 1 << 20 // 1 MiB
	pw := &progressWriter{name: "noble-pxe", total: total, log: slog.New(cap)}

	// A single write that skips past all four thresholds must emit each
	// of 20/40/60/80 exactly once at its exact byte count.
	if _, err := pw.Write(make([]byte, total)); err != nil {
		t.Fatal(err)
	}

	records := progressRecords(cap)
	if len(records) != len(progressThresholds) {
		t.Fatalf("got %d progress records, want %d (thresholds only, no 100%%): %+v",
			len(records), len(progressThresholds), records)
	}
	seenPcts := map[int]int{}
	for _, r := range records {
		a := progressAttrs(r)
		pct, ok := a["pct"].(int64)
		if !ok {
			t.Fatalf("pct attr %v is not numeric", a["pct"])
		}
		seenPcts[int(pct)]++
		wantBytes := total * int64(pct) / 100
		if a["bytes_so_far"] != wantBytes {
			t.Errorf("pct=%d bytes_so_far=%v, want %d", pct, a["bytes_so_far"], wantBytes)
		}
		if a["bytes_total"] != total {
			t.Errorf("pct=%d bytes_total=%v, want %d", pct, a["bytes_total"], total)
		}
		if a["name"] != "noble-pxe" {
			t.Errorf("pct=%d name=%v, want noble-pxe", pct, a["name"])
		}
	}
	for _, pct := range progressThresholds {
		if seenPcts[pct] != 1 {
			t.Errorf("threshold %d emitted %d times, want exactly 1", pct, seenPcts[pct])
		}
	}
}

func TestProgressWriter_NoDuplicateOnBoundaryWrite(t *testing.T) {
	cap := &captureHandler{}
	const total int64 = 1 << 20
	pw := &progressWriter{name: "noble-pxe", total: total, log: slog.New(cap)}

	// Write up to exactly the 20% boundary, then exactly to the 40%
	// boundary. The 20% snapshot must not repeat when 40% is reached.
	first := total * 20 / 100
	if _, err := pw.Write(make([]byte, first)); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(make([]byte, first)); err != nil {
		t.Fatal(err)
	}

	records := progressRecords(cap)
	if len(records) != 2 {
		t.Fatalf("got %d progress records, want 2 (one per boundary, no duplicate): %+v",
			len(records), records)
	}
	if got := progressAttrs(records[0])["pct"]; got != int64(20) {
		t.Errorf("first record pct=%v, want 20", got)
	}
	if got := progressAttrs(records[1])["pct"]; got != int64(40) {
		t.Errorf("second record pct=%v, want 40", got)
	}
}

func TestProgressWriter_NoProgressOnCompletionOnly(t *testing.T) {
	cap := &captureHandler{}
	const total int64 = 1 << 20
	pw := &progressWriter{name: "noble-pxe", total: total, log: slog.New(cap)}

	// Write past 80% to completion: thresholds fire in ascending order but
	// no 100% snapshot does (completion is the "image ready" log upstream).
	upTo80 := total * 80 / 100
	if _, err := pw.Write(make([]byte, upTo80)); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Write(make([]byte, total-upTo80)); err != nil {
		t.Fatal(err)
	}

	records := progressRecords(cap)
	if len(records) != len(progressThresholds) {
		t.Fatalf("got %d progress records, want %d (no 100%% snapshot): %+v",
			len(records), len(progressThresholds), records)
	}
	if got := progressAttrs(records[len(records)-1])["pct"]; got != int64(80) {
		t.Errorf("pct=%v, want 80", got)
	}
}

func TestProgressWriter_TinyFileSuppressesProgress(t *testing.T) {
	cap := &captureHandler{}
	const total int64 = 1<<20 - 1 // just under 1 MiB
	pw := &progressWriter{name: "noble-pxe", total: total, log: slog.New(cap)}

	if _, err := pw.Write(make([]byte, total)); err != nil {
		t.Fatal(err)
	}
	if records := progressRecords(cap); len(records) != 0 {
		t.Fatalf("tiny file emitted %d progress records, want 0: %+v", len(records), records)
	}
}

func TestProgressWriter_UnknownLengthSuppressesProgress(t *testing.T) {
	cap := &captureHandler{}
	pw := &progressWriter{name: "noble-pxe", total: -1, log: slog.New(cap)} // unknown Content-Length

	if _, err := pw.Write(make([]byte, 10<<20)); err != nil {
		t.Fatal(err)
	}
	if records := progressRecords(cap); len(records) != 0 {
		t.Fatalf("unknown-length download emitted %d progress records, want 0: %+v",
			len(records), records)
	}
}

// completeImage returns the four files docs/user/images.md says an image
// carries, with contents tagged so two builds can be told apart on disk.
func completeImage(tag string) map[string]string {
	return map[string]string{
		"vmlinuz":       "kernel-" + tag,
		"initrd":        "initrd-" + tag,
		"initrd-local":  "initrd-local-" + tag,
		"root.squashfs": "root-" + tag,
	}
}

// buildTestTarball writes a complete image tarball whose top-level
// directory is name.
func buildTestTarball(t *testing.T, name string) (path string, sha string) {
	t.Helper()
	return buildTestTarballFiles(t, name, completeImage(name))
}

// buildTestTarballFiles writes a tarball holding files under one top-level
// directory, and returns its path and SHA-256.
func buildTestTarballFiles(t *testing.T, top string, files map[string]string) (path string, sha string) {
	t.Helper()
	tarPath := filepath.Join(t.TempDir(), top+".tar.gz")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	gzw := gzip.NewWriter(f)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		content := []byte(files[name])
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + name, Size: int64(len(content)), Mode: 0644, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return tarPath, hex.EncodeToString(sum[:])
}

func TestSyncImages_Downloads(t *testing.T) {
	tarPath, sha := buildTestTarball(t, "noble-pxe")

	tarData, _ := os.ReadFile(tarPath)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarData)
	}))
	defer srv.Close()

	imagesDir := t.TempDir()
	images := map[string]*model.Image{
		"noble-pxe": {
			Name:   "noble-pxe",
			SHA256: sha,
			URL:    srv.URL + "/noble-pxe.tar.gz",
		},
	}

	if err := SyncImages(slog.Default(), imagesDir, images); err != nil {
		t.Fatal(err)
	}

	// The version is extracted into its own directory and the image name
	// links to it.
	wantVersion := "noble-pxe@" + sha[:16]
	if got, ok := Installed(imagesDir, "noble-pxe"); !ok || got != wantVersion {
		t.Fatalf("Installed = %q, %v; want %q, true", got, ok, wantVersion)
	}
	for _, f := range []string{"vmlinuz", "initrd"} {
		if _, err := os.Stat(filepath.Join(imagesDir, wantVersion, f)); err != nil {
			t.Errorf("%s not in the version directory: %v", f, err)
		}
		if _, err := os.Stat(filepath.Join(imagesDir, "noble-pxe", f)); err != nil {
			t.Errorf("%s not reachable through the image name: %v", f, err)
		}
	}
}

// pinServer serves each body in blobs at its key and counts requests.
func pinServer(t *testing.T, blobs map[string][]byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, ok := blobs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// versionTarball builds a complete noble-pxe tarball whose vmlinuz reads
// content.
func versionTarball(t *testing.T, content string) ([]byte, string) {
	t.Helper()
	files := completeImage(content)
	files["vmlinuz"] = content
	path, sha := buildTestTarballFiles(t, "noble-pxe", files)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data, sha
}

func readOrEmpty(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// TestSyncImages_RepinKeepsPreviousVersion: a boot that started before a
// re-pin fetches its remaining files from the version directory its boot
// script named, so a re-pin must leave the previous version's directory
// untouched while the image name switches to the new one. Before the fix
// SyncImages removed the image directory and renamed the new one into its
// place, so those fetches got the new version's kernel (or a 404 between the
// two steps). Only the current and previous versions are kept: a third pin
// removes the first.
func TestSyncImages_RepinKeepsPreviousVersion(t *testing.T) {
	v1, sum1 := versionTarball(t, "kernel-v1")
	v2, sum2 := versionTarball(t, "kernel-v2")
	v3, sum3 := versionTarball(t, "kernel-v3")
	srv, _ := pinServer(t, map[string][]byte{"/v1.tar.gz": v1, "/v2.tar.gz": v2, "/v3.tar.gz": v3})
	imagesDir := t.TempDir()
	pin := func(sum, path string) {
		t.Helper()
		if err := SyncImages(slog.Default(), imagesDir, map[string]*model.Image{
			"noble-pxe": {Name: "noble-pxe", SHA256: sum, URL: srv.URL + path},
		}); err != nil {
			t.Fatalf("SyncImages: %v", err)
		}
	}
	dir1, dir2, dir3 := "noble-pxe@"+sum1[:16], "noble-pxe@"+sum2[:16], "noble-pxe@"+sum3[:16]

	pin(sum1, "/v1.tar.gz")
	pin(sum2, "/v2.tar.gz")
	if got := readOrEmpty(filepath.Join(imagesDir, dir1, "vmlinuz")); got != "kernel-v1" {
		t.Errorf("after re-pin to v2, %s/vmlinuz = %q, want kernel-v1 (a boot that started on v1 still fetches from it)", dir1, got)
	}
	if got := readOrEmpty(filepath.Join(imagesDir, "noble-pxe", "vmlinuz")); got != "kernel-v2" {
		t.Errorf("after re-pin to v2, noble-pxe/vmlinuz = %q, want kernel-v2", got)
	}

	pin(sum3, "/v3.tar.gz")
	if _, err := os.Stat(filepath.Join(imagesDir, dir1)); !os.IsNotExist(err) {
		t.Errorf("after a third pin, %s still exists (err %v); only the current and previous versions are kept", dir1, err)
	}
	if got := readOrEmpty(filepath.Join(imagesDir, dir2, "vmlinuz")); got != "kernel-v2" {
		t.Errorf("after re-pin to v3, %s/vmlinuz = %q, want kernel-v2", dir2, got)
	}
	if got, ok := Installed(imagesDir, "noble-pxe"); !ok || got != dir3 {
		t.Errorf("Installed = %q, %v; want %q, true", got, ok, dir3)
	}
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("SyncImages left %s behind in the images dir", e.Name())
		}
	}
}

// TestSyncImages_RollbackToPreviousVersionReusesIt: re-pinning the version
// the image was just switched away from switches back without downloading
// it again, and without replacing a directory an in-flight boot may be
// reading.
func TestSyncImages_RollbackToPreviousVersionReusesIt(t *testing.T) {
	v1, sum1 := versionTarball(t, "kernel-v1")
	v2, sum2 := versionTarball(t, "kernel-v2")
	srv, hits := pinServer(t, map[string][]byte{"/v1.tar.gz": v1, "/v2.tar.gz": v2})
	imagesDir := t.TempDir()
	for _, step := range []struct{ sum, path string }{{sum1, "/v1.tar.gz"}, {sum2, "/v2.tar.gz"}, {sum1, "/v1.tar.gz"}} {
		if err := SyncImages(slog.Default(), imagesDir, map[string]*model.Image{
			"noble-pxe": {Name: "noble-pxe", SHA256: step.sum, URL: srv.URL + step.path},
		}); err != nil {
			t.Fatalf("SyncImages: %v", err)
		}
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("downloads = %d, want 2 (the rollback to v1 reuses its directory)", n)
	}
	if got := readOrEmpty(filepath.Join(imagesDir, "noble-pxe", "vmlinuz")); got != "kernel-v1" {
		t.Errorf("after rollback, noble-pxe/vmlinuz = %q, want kernel-v1", got)
	}
	if got := readOrEmpty(filepath.Join(imagesDir, "noble-pxe@"+sum2[:16], "vmlinuz")); got != "kernel-v2" {
		t.Errorf("after rollback, the v2 directory holds %q, want kernel-v2 (kept as the previous version)", got)
	}
}

// TestSyncImages_ReplacesPlainDirectory: an images dir whose image is a
// plain directory (populated by hand, or extracted in place by a release
// before version directories) switches to a version directory on the next
// sync, and the plain directory is removed.
func TestSyncImages_ReplacesPlainDirectory(t *testing.T) {
	v1, sum1 := versionTarball(t, "kernel-v1")
	srv, _ := pinServer(t, map[string][]byte{"/v1.tar.gz": v1})
	imagesDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(imagesDir, "noble-pxe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imagesDir, "noble-pxe", "vmlinuz"), []byte("kernel-old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SyncImages(slog.Default(), imagesDir, map[string]*model.Image{
		"noble-pxe": {Name: "noble-pxe", SHA256: sum1, URL: srv.URL + "/v1.tar.gz"},
	}); err != nil {
		t.Fatalf("SyncImages: %v", err)
	}
	if got, ok := Installed(imagesDir, "noble-pxe"); !ok || got != "noble-pxe@"+sum1[:16] {
		t.Errorf("Installed = %q, %v; want the v1 version directory", got, ok)
	}
	if got := readOrEmpty(filepath.Join(imagesDir, "noble-pxe", "vmlinuz")); got != "kernel-v1" {
		t.Errorf("noble-pxe/vmlinuz = %q, want kernel-v1", got)
	}
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("images dir holds %v, want only the link and the v1 version directory", entries)
	}
}

// TestSyncImages_RefusesMalformedDigest: part of the row's sha256 becomes a
// directory name, so a digest that is not 64 hex digits is refused before
// anything is downloaded or created, and the other images still sync. No
// such digest could match a download, so nothing that synced before is
// refused now.
func TestSyncImages_RefusesMalformedDigest(t *testing.T) {
	v1, sum1 := versionTarball(t, "kernel-v1")
	for _, bad := range []string{"", "abc", "../../../../victim/" + strings.Repeat("0", 64), strings.Repeat("g", 64), sum1 + "0"} {
		t.Run(bad, func(t *testing.T) {
			srv, hits := pinServer(t, map[string][]byte{"/v1.tar.gz": v1})
			base := t.TempDir()
			imagesDir := filepath.Join(base, "images")
			if err := os.MkdirAll(imagesDir, 0o755); err != nil {
				t.Fatal(err)
			}
			err := SyncImages(slog.Default(), imagesDir, map[string]*model.Image{
				"jammy-pxe": {Name: "jammy-pxe", SHA256: bad, URL: srv.URL + "/v1.tar.gz"},
				"noble-pxe": {Name: "noble-pxe", SHA256: sum1, URL: srv.URL + "/v1.tar.gz"},
			})
			if err == nil {
				t.Errorf("SyncImages accepted sha256 %q, want an error", bad)
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("downloads = %d, want 1 (the valid image only)", n)
			}
			if _, ok := Installed(imagesDir, "noble-pxe"); !ok {
				t.Error("the valid image did not sync alongside the refused one")
			}
			entries, _ := os.ReadDir(base)
			if len(entries) != 1 {
				t.Errorf("SyncImages created %v beside the images dir", entries)
			}
			if _, err := os.Lstat(filepath.Join(imagesDir, "jammy-pxe")); !os.IsNotExist(err) {
				t.Errorf("refused image jammy-pxe has an entry on disk (err %v)", err)
			}
		})
	}
}

func TestSyncImages_SkipsUpToDate(t *testing.T) {
	tarPath, sha := buildTestTarball(t, "noble-pxe")

	tarData, _ := os.ReadFile(tarPath)
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Write(tarData)
	}))
	defer srv.Close()

	imagesDir := t.TempDir()
	images := map[string]*model.Image{
		"noble-pxe": {
			Name:   "noble-pxe",
			SHA256: sha,
			URL:    srv.URL + "/noble-pxe.tar.gz",
		},
	}

	// First sync: should download
	if err := SyncImages(slog.Default(), imagesDir, images); err != nil {
		t.Fatal(err)
	}
	if n := callCount.Load(); n != 1 {
		t.Fatalf("expected 1 download, got %d", n)
	}

	// Second sync: should skip (the link already names this version)
	if err := SyncImages(slog.Default(), imagesDir, images); err != nil {
		t.Fatal(err)
	}
	if n := callCount.Load(); n != 1 {
		t.Fatalf("expected no second download, got %d total calls", n)
	}
}

// TestSyncImages_RollbackSurvivesDownloadFailure: if an attempted
// upgrade fails before the link is switched (network error, SHA
// mismatch), the link and the version it names must remain intact. A
// subsequent rollback to the previous image's SHA should short-circuit
// on the unchanged link without re-downloading.
func TestSyncImages_RollbackSurvivesDownloadFailure(t *testing.T) {
	tarPath, shaA := buildTestTarball(t, "noble-rollback")
	tarData, _ := os.ReadFile(tarPath)

	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Write(tarData)
	}))
	defer srv.Close()

	imagesDir := t.TempDir()
	images := map[string]*model.Image{
		"noble-rollback": {
			Name:   "noble-rollback",
			SHA256: shaA,
			URL:    srv.URL + "/noble-rollback.tar.gz",
		},
	}

	// First sync: image A lands cleanly.
	if err := SyncImages(slog.Default(), imagesDir, images); err != nil {
		t.Fatal(err)
	}
	if n := downloads.Load(); n != 1 {
		t.Fatalf("first sync: downloads = %d, want 1", n)
	}

	// Attempted upgrade fails at SHA check (pre-mutation).
	shaB := "ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000ffff0000"
	images["noble-rollback"].SHA256 = shaB
	if err := SyncImages(slog.Default(), imagesDir, images); err == nil {
		t.Fatal("expected upgrade to fail on SHA mismatch")
	}

	// Rollback to A. The failed upgrade never switched the link,
	// so this sync should short-circuit, NO download.
	images["noble-rollback"].SHA256 = shaA
	downloadsBefore := downloads.Load()
	if err := SyncImages(slog.Default(), imagesDir, images); err != nil {
		t.Fatalf("rollback sync: %v", err)
	}
	if downloads.Load() != downloadsBefore {
		t.Errorf("rollback triggered an unnecessary re-download: %d -> %d",
			downloadsBefore, downloads.Load())
	}
}

// TestSyncImages_RemovesLeftoversOfAnInterruptedSync: pullImage downloads
// into <imagesDir>/.image-download-* and extracts into
// <imagesDir>/.image-extract-*, and removes both with defers. A process that
// exits mid-sync (SIGKILL, OOM, or a SIGTERM from systemctl restart or the
// unit's start timeout, since nothing waits for the sync to unwind) runs no
// defers, so the partial tarball (up to the full image size) and the partial
// tree stay in imagesDir. Before the fix nothing ever removed them: each
// interrupted sync leaked another copy onto the worker's data filesystem.
// The next sync must remove them.
func TestSyncImages_RemovesLeftoversOfAnInterruptedSync(t *testing.T) {
	tarPath, sha := buildTestTarball(t, "noble-pxe")
	tarData, _ := os.ReadFile(tarPath)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarData)
	}))
	defer srv.Close()

	imagesDir := t.TempDir()
	leftovers := []string{
		filepath.Join(imagesDir, ".image-download-1234567"),
		filepath.Join(imagesDir, ".image-extract-7654321", "noble-pxe", "root.squashfs"),
		filepath.Join(imagesDir, ".image-old-2468", "noble-pxe@0123456789abcdef", "vmlinuz"),
		filepath.Join(imagesDir, ".image-link-jammy-pxe"),
	}
	for _, p := range leftovers {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := SyncImages(slog.Default(), imagesDir, map[string]*model.Image{
		"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: srv.URL + "/noble-pxe.tar.gz"},
	}); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{leftovers[0], filepath.Join(imagesDir, ".image-extract-7654321"),
		filepath.Join(imagesDir, ".image-old-2468"), leftovers[3]} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s, left by an interrupted sync, is still in the images dir after a successful sync (stat err %v)", filepath.Base(p), err)
		}
	}
	if _, err := os.Stat(filepath.Join(imagesDir, "noble-pxe", "vmlinuz")); err != nil {
		t.Errorf("noble-pxe was not synced: %v", err)
	}
}

// TestSyncImages_WaitsForAnotherSyncInTheSameDir: the leftover sweep must
// only remove temp files whose sync is dead, never those of a sync still
// running in another process (slagctl image sync --dir pointed at a running
// worker's images dir). Removing a live sync's extract dir mid-extraction
// would let it rename a tree missing its earlier files into place and stamp
// it with a valid marker. SyncImages therefore holds an exclusive flock on
// imagesDir, which the kernel releases when the holder exits however it
// exits, and waits for it before sweeping or downloading. The test holds the
// lock the way a live sync in another process would.
func TestSyncImages_WaitsForAnotherSyncInTheSameDir(t *testing.T) {
	tarPath, sha := buildTestTarball(t, "noble-pxe")
	tarData, _ := os.ReadFile(tarPath)
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Write(tarData)
	}))
	defer srv.Close()

	imagesDir := t.TempDir()
	unlock, err := lockImagesDir(slog.Default(), imagesDir)
	if err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(imagesDir, ".image-extract-1111111", "noble-pxe", "vmlinuz")
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("being extracted"), 0o644); err != nil {
		t.Fatal(err)
	}

	waiting := make(chan struct{})
	logger := slog.New(&signalHandler{captureHandler: &captureHandler{}, msg: msgWaitingForLock, ch: waiting})
	done := make(chan error, 1)
	go func() {
		done <- SyncImages(logger, imagesDir, map[string]*model.Image{
			"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: srv.URL + "/noble-pxe.tar.gz"},
		})
	}()

	select {
	case <-waiting:
	case err := <-done:
		unlock()
		t.Fatalf("SyncImages returned %v while another sync held the images dir lock, want it to wait", err)
	}
	// SyncImages has found the lock held and is blocked on it: nothing it
	// does before acquiring the lock touches the dir or the network.
	if _, err := os.Stat(live); err != nil {
		t.Errorf("a waiting SyncImages removed the running sync's temp file: %v", err)
	}
	if n := downloads.Load(); n != 0 {
		t.Errorf("a waiting SyncImages downloaded %d times before the running sync finished", n)
	}

	unlock() // the other sync exits; its temp file is now a leftover
	if err := <-done; err != nil {
		t.Fatalf("SyncImages after the lock was released: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(imagesDir, ".image-extract-1111111")); !os.IsNotExist(err) {
		t.Errorf("leftover of the finished sync not removed once the lock was free (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(imagesDir, "noble-pxe", "vmlinuz")); err != nil {
		t.Errorf("noble-pxe was not synced after the lock was released: %v", err)
	}
}

// signalHandler closes ch the first time a record with message msg is
// handled, so a test can wait for a specific log line without sleeping.
type signalHandler struct {
	*captureHandler
	msg  string
	ch   chan struct{}
	once sync.Once
}

func (h *signalHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == h.msg {
		h.once.Do(func() { close(h.ch) })
	}
	return h.captureHandler.Handle(ctx, r)
}

// serveTarball serves the file at tarPath for every request and returns
// the server's URL.
func serveTarball(t *testing.T, tarPath string) string {
	t.Helper()
	data, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestSyncImages_IncompleteImageKeepsPrevious: a tarball whose digest
// matches the row but which lacks one of the four image files (say, a
// hand-assembled tarball without initrd-local) must be refused before the
// previous image is removed. Before the fix it replaced a complete image
// and was marked synced: PXE still booted, and every UEFI deploy then
// failed at installing_bootloader with a 404 on initrd-local, after the
// target's disks had been partitioned and rsynced.
func TestSyncImages_IncompleteImageKeepsPrevious(t *testing.T) {
	cases := []struct {
		name    string
		missing string // the file the error must name
		edit    func(files map[string]string)
	}{
		{"without vmlinuz", "vmlinuz", func(f map[string]string) { delete(f, "vmlinuz") }},
		{"without initrd", "initrd", func(f map[string]string) { delete(f, "initrd") }},
		{"without initrd-local", "initrd-local", func(f map[string]string) { delete(f, "initrd-local") }},
		{"without root.squashfs", "root.squashfs", func(f map[string]string) { delete(f, "root.squashfs") }},
		// A directory where a file belongs is no more servable than no entry.
		{"initrd-local is a directory", "initrd-local", func(f map[string]string) {
			delete(f, "initrd-local")
			f["initrd-local/stray"] = "x"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			imagesDir := t.TempDir()
			goodPath, goodSHA := buildTestTarballFiles(t, "noble-pxe", completeImage("good"))
			if err := SyncImages(slog.Default(), imagesDir, map[string]*model.Image{
				"noble-pxe": {Name: "noble-pxe", SHA256: goodSHA, URL: serveTarball(t, goodPath)},
			}); err != nil {
				t.Fatalf("initial sync of a complete image: %v", err)
			}

			files := completeImage("bad")
			tc.edit(files)
			badPath, badSHA := buildTestTarballFiles(t, "noble-pxe", files)
			err := SyncImages(slog.Default(), imagesDir, map[string]*model.Image{
				"noble-pxe": {Name: "noble-pxe", SHA256: badSHA, URL: serveTarball(t, badPath)},
			})
			if err == nil || !strings.Contains(err.Error(), "noble-pxe/"+tc.missing) {
				t.Errorf("SyncImages of a tarball without a regular file %s returned %v; want an error naming noble-pxe/%s", tc.missing, err, tc.missing)
			}
			for file, want := range completeImage("good") {
				got, rerr := os.ReadFile(filepath.Join(imagesDir, "noble-pxe", file))
				if rerr != nil || string(got) != want {
					t.Errorf("noble-pxe/%s = %q (%v) after the refused sync; want the previous image's %q", file, got, rerr, want)
				}
			}
			if got, _ := Installed(imagesDir, "noble-pxe"); got != "noble-pxe@"+goodSHA[:16] {
				t.Errorf("installed version = %q, want the previous image's noble-pxe@%s", got, goodSHA[:16])
			}
			if _, serr := os.Lstat(filepath.Join(imagesDir, "noble-pxe@"+badSHA[:16])); !os.IsNotExist(serr) {
				t.Errorf("the refused tarball left a version directory behind (stat err %v)", serr)
			}
		})
	}
}

// TestSyncImages_RefusesPathShapedName: an image's name becomes a path
// under imagesDir, and the name arrives from the image row via the site
// snapshot (or from slagctl image sync --images-json). A name that is not
// a plain single path element must be refused before anything is
// downloaded or removed. Before the fix, "" and "." made pullImage
// RemoveAll imagesDir itself (every image and the package-installed
// systemd-bootx64.efi), ".." removed imagesDir's parent (the worker's
// data dir), and "../victim" replaced a sibling directory with the
// tarball's contents. A valid image in the same snapshot must still sync.
func TestSyncImages_RefusesPathShapedName(t *testing.T) {
	tarPath, sha := buildTestTarball(t, "noble-pxe")
	tarData, _ := os.ReadFile(tarPath)
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Write(tarData)
	}))
	defer srv.Close()

	for _, name := range []string{"", ".", "..", "../victim", "noble-pxe/.."} {
		t.Run(name, func(t *testing.T) {
			downloads.Store(0)
			base := t.TempDir()
			imagesDir := filepath.Join(base, "site", "images")
			bystanders := map[string]string{
				filepath.Join(imagesDir, "systemd-bootx64.efi"):   "package-installed efi",
				filepath.Join(imagesDir, "jammy-pxe", "vmlinuz"):  "another image",
				filepath.Join(base, "site", "worker.db"):          "site worker state",
				filepath.Join(base, "site", "victim", "precious"): "sibling of imagesDir",
			}
			for p, content := range bystanders {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			err := SyncImages(slog.Default(), imagesDir, map[string]*model.Image{
				name:        {Name: name, SHA256: sha, URL: srv.URL + "/bad.tar.gz"},
				"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: srv.URL + "/noble-pxe.tar.gz"},
			})

			for p, want := range bystanders {
				got, rerr := os.ReadFile(p)
				if rerr != nil || string(got) != want {
					t.Errorf("SyncImages with image name %q destroyed %s (read %q, %v)", name, p, got, rerr)
				}
			}
			if err == nil {
				t.Errorf("SyncImages accepted image name %q, want an error", name)
			}
			if n := downloads.Load(); n != 1 {
				t.Errorf("downloads = %d, want 1 (the valid image only; the refused name must not be fetched)", n)
			}
			if _, serr := os.Stat(filepath.Join(imagesDir, "noble-pxe", "vmlinuz")); serr != nil {
				t.Errorf("valid image noble-pxe was not synced alongside refused name %q: %v", name, serr)
			}
		})
	}
}

// TestSyncImages_DigestSpellingDoesNotMatter: the row's sha256 is whatever
// string was written to the API (slagctl image create --sha256 passes the
// flag through, and the API checks only that it is non-empty), and a hex
// digest names the same tarball in either case. Before the fix SyncImages
// compared the row's value with the lowercase hex it computed as an exact
// string, so a row holding the tarball's digest in upper case, or with a
// stray space or newline around it, failed with "sha256 mismatch" on every
// snapshot apply, after downloading the whole tarball each time. The
// image must sync, the marker must record the lowercase digest, and the
// next sync of the same row must hit the marker instead of downloading.
func TestSyncImages_DigestSpellingDoesNotMatter(t *testing.T) {
	tarPath, sha := buildTestTarball(t, "noble-pxe")
	tarData, _ := os.ReadFile(tarPath)
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Write(tarData)
	}))
	defer srv.Close()

	for _, tc := range []struct{ name, digest string }{
		{"upper case", strings.ToUpper(sha)},
		{"mixed case", strings.ToUpper(sha[:32]) + sha[32:]},
		{"surrounding whitespace", " " + sha + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			downloads.Store(0)
			imagesDir := t.TempDir()
			images := map[string]*model.Image{
				"noble-pxe": {Name: "noble-pxe", SHA256: tc.digest, URL: srv.URL + "/noble-pxe.tar.gz"},
			}

			if err := SyncImages(slog.Default(), imagesDir, images); err != nil {
				t.Fatalf("SyncImages with digest %q for a tarball whose digest is %s: %v", tc.digest, sha, err)
			}
			if got, err := os.ReadFile(filepath.Join(imagesDir, "noble-pxe", "vmlinuz")); err != nil || string(got) != "kernel-noble-pxe" {
				t.Fatalf("vmlinuz = %q, %v; want the tarball's content", got, err)
			}
			if got, _ := Installed(imagesDir, "noble-pxe"); got != "noble-pxe@"+sha[:16] {
				t.Errorf("installed version = %q, want the lowercase digest's %q", got, "noble-pxe@"+sha[:16])
			}

			if err := SyncImages(slog.Default(), imagesDir, images); err != nil {
				t.Fatalf("second SyncImages: %v", err)
			}
			if n := downloads.Load(); n != 1 {
				t.Errorf("downloads = %d after two syncs of the same row, want 1 (the second must find the version installed)", n)
			}
		})
	}
}

// orderedEntry is one regular-file entry under noble-pxe/ in an
// orderedTarball, which keeps entries in the given order and allows a path
// to appear more than once.
type orderedEntry struct {
	name string
	body string
	mode int64
}

func orderedTarball(t *testing.T, entries []orderedEntry) (blob []byte, sha string) {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Name: "noble-pxe/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: "noble-pxe/" + e.name, Typeflag: tar.TypeReg, Mode: e.mode, Size: int64(len(e.body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// TestSyncImages_DuplicateTarEntryLaterWins: when a tarball holds the same
// path twice (for example after `tar -r` appended a replacement file), the
// later entry is the file, as with tar(1). Before the fix, extraction
// opened the file the earlier entry had left without truncating it, so a
// shorter later copy kept the earlier copy's tail: vmlinuz held bytes that
// appear in neither entry, SyncImages reported success, and the marker was
// written, so the image was never pulled again. The earlier file was also
// opened in place, so the later entry's mode was ignored and a read-only
// earlier copy made extraction fail for a non-root caller.
func TestSyncImages_DuplicateTarEntryLaterWins(t *testing.T) {
	others := []orderedEntry{
		{name: "initrd", body: "i", mode: 0o644},
		{name: "initrd-local", body: "l", mode: 0o644},
		{name: "root.squashfs", body: "r", mode: 0o644},
	}
	cases := []struct {
		name           string
		earlier, later orderedEntry
	}{
		{
			name:    "shorter later copy",
			earlier: orderedEntry{name: "vmlinuz", body: "old-kernel-bytes", mode: 0o644},
			later:   orderedEntry{name: "vmlinuz", body: "new", mode: 0o644},
		},
		{
			// Passed before the fix too: the later entry already won when it
			// was at least as long, which pins the failure above to the
			// missing truncation rather than to first-entry-wins.
			name:    "longer later copy",
			earlier: orderedEntry{name: "vmlinuz", body: "new", mode: 0o644},
			later:   orderedEntry{name: "vmlinuz", body: "old-kernel-bytes", mode: 0o644},
		},
		{
			name:    "read-only earlier copy",
			earlier: orderedEntry{name: "vmlinuz", body: "old-kernel-bytes", mode: 0o444},
			later:   orderedEntry{name: "vmlinuz", body: "new", mode: 0o644},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := append([]orderedEntry{tc.earlier}, others...)
			entries = append(entries, tc.later)
			blob, sha := orderedTarball(t, entries)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(blob)
			}))
			defer srv.Close()

			imagesDir := t.TempDir()
			images := map[string]*model.Image{
				"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: srv.URL + "/noble-pxe.tar.gz"},
			}
			if err := SyncImages(slog.Default(), imagesDir, images); err != nil {
				t.Fatalf("SyncImages: %v", err)
			}

			path := filepath.Join(imagesDir, "noble-pxe", "vmlinuz")
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.later.body {
				t.Errorf("vmlinuz = %q, want %q (the later entry; the earlier one was %q)", got, tc.later.body, tc.earlier.body)
			}
			fi, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			// Owner bits only, so the check holds under any umask that
			// leaves the owner's permissions alone.
			want := os.FileMode(tc.later.mode) & 0o700
			if perm := fi.Mode().Perm() & 0o700; perm != want {
				t.Errorf("vmlinuz owner permissions = %#o, want %#o (the later entry's mode %#o, not the earlier %#o)",
					perm, want, tc.later.mode, tc.earlier.mode)
			}
		})
	}
}

func TestSyncImages_RejectsBADSHA(t *testing.T) {
	tarPath, _ := buildTestTarball(t, "noble-bad")

	tarData, _ := os.ReadFile(tarPath)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(tarData)
	}))
	defer srv.Close()

	imagesDir := t.TempDir()
	images := map[string]*model.Image{
		"noble-bad": {
			Name:   "noble-bad",
			SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
			URL:    srv.URL + "/noble-bad.tar.gz",
		},
	}

	err := SyncImages(slog.Default(), imagesDir, images)
	if err == nil {
		t.Fatal("expected error for SHA mismatch")
	}

	// Should NOT have extracted anything
	if _, err := os.Stat(filepath.Join(imagesDir, "noble-bad", "vmlinuz")); err == nil {
		t.Error("expected no extraction on SHA mismatch")
	}
}

// shortStallTimeout shortens stallTimeout for one test.
func shortStallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := stallTimeout
	stallTimeout = d
	t.Cleanup(func() { stallTimeout = old })
}

// TestSyncImages_AbandonsStalledDownload: a download whose server stops
// sending, before the response headers or partway through the body, fails
// once no data has arrived for stallTimeout. Without the timeout it would
// hold the site worker's image sync, and the commission and deploy ops
// waiting on it, until the process restarted.
func TestSyncImages_AbandonsStalledDownload(t *testing.T) {
	shortStallTimeout(t, 200*time.Millisecond)
	for _, tc := range []struct {
		name  string
		start func(w http.ResponseWriter) // what the server sends before it stops
	}{
		{"no response headers", func(http.ResponseWriter) {}},
		{"body stops", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "1048576")
			_, _ = w.Write(make([]byte, 4096))
			w.(http.Flusher).Flush()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tc.start(w)
				select {
				case <-r.Context().Done():
				case <-stop:
				}
			}))
			defer srv.Close()
			defer close(stop)

			imagesDir := t.TempDir()
			images := map[string]*model.Image{
				"noble-pxe": {Name: "noble-pxe", SHA256: strings.Repeat("ab", 32), URL: srv.URL + "/noble-pxe.tar.gz"},
			}
			errc := make(chan error, 1)
			go func() { errc <- SyncImages(slog.Default(), imagesDir, images) }()
			select {
			case err := <-errc:
				if err == nil || !strings.Contains(err.Error(), "download stalled") {
					t.Fatalf("SyncImages = %v, want a download stalled error", err)
				}
				if !Retryable(err) {
					t.Errorf("a stalled download is not Retryable: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("SyncImages still waiting on a server that stopped sending 5s ago (stall timeout 200ms)")
			}
			if v, ok := Installed(imagesDir, "noble-pxe"); ok {
				t.Errorf("image installed (%s) from an abandoned download", v)
			}
		})
	}
}

// TestSyncImages_RetryableClassifiesFailures: a site worker retries a failed
// image sync on a backoff only when Retryable says so. A failure to fetch
// from the URL may not repeat on the next attempt and must be retryable; a
// digest mismatch downloads the whole tarball before failing and repeats on
// every attempt, so it, and the other failures a later attempt would
// repeat, must not be.
func TestSyncImages_RetryableClassifiesFailures(t *testing.T) {
	tarPath, sha := buildTestTarball(t, "noble-pxe")
	tarData, _ := os.ReadFile(tarPath)
	serve := func(h http.HandlerFunc) string {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv.URL + "/noble-pxe.tar.gz"
	}
	status := func(code int) string {
		return serve(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", code) })
	}
	good := serve(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tarData) })
	cutShort := serve(func(w http.ResponseWriter, _ *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1048576\r\n\r\n")
		_, _ = buf.Write(tarData[:len(tarData)/2])
		_ = buf.Flush()
	})
	closed := httptest.NewServer(http.NotFoundHandler())
	refusedURL := closed.URL + "/noble-pxe.tar.gz"
	closed.Close()

	for _, tc := range []struct {
		name      string
		imagesDir string // "" means a fresh directory
		images    map[string]*model.Image
		retryable bool
	}{
		{name: "503", retryable: true, images: map[string]*model.Image{
			"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: status(http.StatusServiceUnavailable)}}},
		{name: "404", retryable: true, images: map[string]*model.Image{
			"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: status(http.StatusNotFound)}}},
		{name: "connection refused", retryable: true, images: map[string]*model.Image{
			"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: refusedURL}}},
		{name: "body cut short", retryable: true, images: map[string]*model.Image{
			"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: cutShort}}},
		{name: "503 beside a refused name", retryable: true, images: map[string]*model.Image{
			"..":        {Name: "..", SHA256: sha, URL: good},
			"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: status(http.StatusServiceUnavailable)}}},
		{name: "digest mismatch", retryable: false, images: map[string]*model.Image{
			"noble-pxe": {Name: "noble-pxe", SHA256: strings.Repeat("0", 64), URL: good}}},
		{name: "refused name", retryable: false, images: map[string]*model.Image{
			"..": {Name: "..", SHA256: sha, URL: good}}},
		{name: "images directory missing", retryable: false, imagesDir: filepath.Join(t.TempDir(), "absent"),
			images: map[string]*model.Image{"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: good}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.imagesDir
			if dir == "" {
				dir = t.TempDir()
			}
			err := SyncImages(slog.Default(), dir, tc.images)
			if err == nil {
				t.Fatal("SyncImages succeeded, want an error")
			}
			if got := Retryable(err); got != tc.retryable {
				t.Errorf("Retryable(%v) = %v, want %v", err, got, tc.retryable)
			}
		})
	}
}

// TestSyncImages_SlowDownloadIsNotAbandoned: the stall timeout measures time
// without data, not the length of the download. A tarball that arrives in
// pieces over longer than stallTimeout, each piece (the response headers
// included) sooner than it, syncs.
func TestSyncImages_SlowDownloadIsNotAbandoned(t *testing.T) {
	shortStallTimeout(t, 500*time.Millisecond)
	tarPath, sha := buildTestTarball(t, "noble-pxe")
	tarData, _ := os.ReadFile(tarPath)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Headers after 300ms and the first byte 300ms later: together
		// longer than the stall timeout, each gap shorter.
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Length", strconv.Itoa(len(tarData)))
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		piece := len(tarData)/6 + 1
		for off := 0; off < len(tarData); off += piece {
			_, _ = w.Write(tarData[off:min(off+piece, len(tarData))])
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	imagesDir := t.TempDir()
	images := map[string]*model.Image{
		"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: srv.URL + "/noble-pxe.tar.gz"},
	}
	if err := SyncImages(slog.Default(), imagesDir, images); err != nil {
		t.Fatalf("slow but steady download failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(imagesDir, "noble-pxe", "vmlinuz")); err != nil {
		t.Errorf("vmlinuz not found: %v", err)
	}
}

// TestSyncImages_FailingImageDoesNotBlockOthers: each image has its own
// link and version directories, so an image whose download or digest check
// fails must not stop the other images in the same call from syncing;
// its error is reported in the returned error. SyncImages ranges over a map,
// so the failing image is reached first in only some iteration orders. Each
// case therefore re-runs the sync in a fresh directory until the server
// sees the failing image requested first, then checks that call. Before the
// fix SyncImages returned on the first failure, so in that order the good
// image was never fetched.
func TestSyncImages_FailingImageDoesNotBlockOthers(t *testing.T) {
	tarPath, sha := buildTestTarball(t, "noble-pxe")
	tarData, _ := os.ReadFile(tarPath)

	var (
		mu       sync.Mutex
		requests []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/missing.tar.gz" {
			http.NotFound(w, r)
			return
		}
		w.Write(tarData)
	}))
	defer srv.Close()

	cases := []struct {
		name    string
		sha     string // the failing image's row digest
		path    string // the failing image's URL path
		wantErr string
	}{
		{
			name:    "sha256 mismatch",
			sha:     strings.Repeat("0", 64),
			path:    "/bad.tar.gz",
			wantErr: "image jammy-pxe: sha256 mismatch",
		},
		{
			name:    "object missing",
			sha:     sha,
			path:    "/missing.tar.gz",
			wantErr: "image jammy-pxe: download returned 404",
		},
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const maxTries = 1000
			for try := 0; try < maxTries; try++ {
				imagesDir := t.TempDir()
				mu.Lock()
				requests = nil
				mu.Unlock()

				err := SyncImages(quiet, imagesDir, map[string]*model.Image{
					"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: srv.URL + "/good.tar.gz"},
					"jammy-pxe": {Name: "jammy-pxe", SHA256: tc.sha, URL: srv.URL + tc.path},
				})
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("try %d: SyncImages returned %v, want an error containing %q", try, err, tc.wantErr)
				}

				mu.Lock()
				first := ""
				if len(requests) > 0 {
					first = requests[0]
				}
				mu.Unlock()
				if first != tc.path {
					continue // the good image was reached first; not the order under test
				}

				if got, _ := Installed(imagesDir, "noble-pxe"); got != "noble-pxe@"+sha[:16] {
					t.Fatalf("noble-pxe did not sync after jammy-pxe failed first (installed %q; SyncImages returned %v)", got, err)
				}
				if _, serr := os.Stat(filepath.Join(imagesDir, "noble-pxe", "vmlinuz")); serr != nil {
					t.Fatalf("noble-pxe installed but image missing: %v", serr)
				}
				return
			}
			t.Fatalf("jammy-pxe was never requested first in %d tries", maxTries)
		})
	}
}

// TestSyncImages_RefusesNameOfTheBootloader: an image's link shares
// imagesDir with an entry no image owns, the systemd-bootx64.efi the
// slagworks package links there (served to every UEFI target as its
// bootloader). An image named after it must be refused like a path-shaped
// name: reported in the error, never downloaded, and the entry left alone.
// Before the fix the name passed model.ValidateImageName and SyncImages
// replaced the bootloader with the image, so the bootloader step of every
// UEFI deploy at the site failed.
func TestSyncImages_RefusesNameOfTheBootloader(t *testing.T) {
	const name = "systemd-bootx64.efi"
	tarPath, sha := buildTestTarball(t, "noble-pxe")
	tarData, _ := os.ReadFile(tarPath)
	var downloads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Write(tarData)
	}))
	defer srv.Close()

	imagesDir := t.TempDir()
	efi := filepath.Join(imagesDir, name)
	if err := os.WriteFile(efi, []byte("package-installed efi"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := map[string]*model.Image{
		"noble-pxe": {Name: "noble-pxe", SHA256: sha, URL: srv.URL + "/noble-pxe.tar.gz"},
	}
	if err := SyncImages(slog.Default(), imagesDir, good); err != nil {
		t.Fatalf("setup sync of noble-pxe: %v", err)
	}
	before, err := os.ReadDir(imagesDir)
	if err != nil {
		t.Fatal(err)
	}
	downloads.Store(0)

	err = SyncImages(slog.Default(), imagesDir, map[string]*model.Image{
		name:        {Name: name, SHA256: sha, URL: srv.URL + "/bad.tar.gz"},
		"noble-pxe": good["noble-pxe"],
	})
	if err == nil || !strings.Contains(err.Error(), name) {
		t.Errorf("SyncImages with image name %q returned %v, want an error naming it", name, err)
	}
	if got, rerr := os.ReadFile(efi); rerr != nil || string(got) != "package-installed efi" {
		t.Errorf("SyncImages with image name %q replaced the bootloader (read %q, %v)", name, got, rerr)
	}
	after, err := os.ReadDir(imagesDir)
	if err != nil {
		t.Fatal(err)
	}
	names := func(es []os.DirEntry) (out []string) {
		for _, e := range es {
			out = append(out, e.Name())
		}
		return out
	}
	if !slices.Equal(names(before), names(after)) {
		t.Errorf("SyncImages with image name %q changed imagesDir: %v, was %v", name, names(after), names(before))
	}
	if n := downloads.Load(); n != 0 {
		t.Errorf("downloads = %d, want 0 (noble-pxe is up to date and the refused name must not be fetched)", n)
	}
}
