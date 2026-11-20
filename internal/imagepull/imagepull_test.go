package imagepull

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func buildTestTarball(t *testing.T, name string) (path string, sha string) {
	t.Helper()
	dir := t.TempDir()
	tarPath := filepath.Join(dir, name+".tar.gz")

	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}

	hasher := sha256.New()
	gzw := gzip.NewWriter(f)
	tw := tar.NewWriter(gzw)

	// Directory entry
	tw.WriteHeader(&tar.Header{Name: name + "/", Typeflag: tar.TypeDir, Mode: 0755})

	// Fake vmlinuz
	content := []byte("fake-kernel-content")
	tw.WriteHeader(&tar.Header{Name: name + "/vmlinuz", Size: int64(len(content)), Mode: 0644, Typeflag: tar.TypeReg})
	tw.Write(content)

	// Fake initrd
	content2 := []byte("fake-initrd-content")
	tw.WriteHeader(&tar.Header{Name: name + "/initrd", Size: int64(len(content2)), Mode: 0644, Typeflag: tar.TypeReg})
	tw.Write(content2)

	tw.Close()
	gzw.Close()
	f.Close()

	// Compute SHA256
	raw, _ := os.ReadFile(tarPath)
	hasher.Write(raw)
	return tarPath, hex.EncodeToString(hasher.Sum(nil))
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

// versionTarball builds a noble-pxe tarball whose vmlinuz reads content.
func versionTarball(t *testing.T, content string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Name: "noble-pxe/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "noble-pxe/vmlinuz", Size: int64(len(content)), Mode: 0o644, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
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
	for _, bad := range []string{"", "abc", "../../../../victim/" + strings.Repeat("0", 64), strings.Repeat("g", 64), sum1 + "\n"} {
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
			case <-time.After(5 * time.Second):
				t.Fatal("SyncImages still waiting on a server that stopped sending 5s ago (stall timeout 200ms)")
			}
			if v, ok := Installed(imagesDir, "noble-pxe"); ok {
				t.Errorf("image installed (%s) from an abandoned download", v)
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
