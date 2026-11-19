package imagepull

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

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

	// Verify files were extracted
	if _, err := os.Stat(filepath.Join(imagesDir, "noble-pxe", "vmlinuz")); err != nil {
		t.Errorf("vmlinuz not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(imagesDir, "noble-pxe", "initrd")); err != nil {
		t.Errorf("initrd not found: %v", err)
	}

	// Verify marker file
	marker, err := os.ReadFile(filepath.Join(imagesDir, "noble-pxe.sha256"))
	if err != nil {
		t.Fatalf("marker not found: %v", err)
	}
	if got := string(marker); got != sha+"\n" {
		t.Errorf("marker = %q, want %q", got, sha+"\n")
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

	// Second sync: should skip (marker matches)
	if err := SyncImages(slog.Default(), imagesDir, images); err != nil {
		t.Fatal(err)
	}
	if n := callCount.Load(); n != 1 {
		t.Fatalf("expected no second download, got %d total calls", n)
	}
}

// TestSyncImages_RollbackSurvivesDownloadFailure: if an attempted
// upgrade fails BEFORE the point-of-no-return (i.e. before we start
// mutating the target dir) — e.g. network error, SHA mismatch —
// the previous marker + target must remain intact. A subsequent
// rollback to the previous image's SHA should short-circuit on the
// still-valid marker without re-downloading.
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

	// Rollback to A. Pre-mutation failure means the marker is
	// still valid; this sync should short-circuit, NO download.
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
