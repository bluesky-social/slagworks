package imagepull

// Stateful property-based coverage for SyncImages: a history of snapshot
// image sets (small name pool, changing versions, download faults) is
// replayed against SyncImages and against an independent model of the images
// directory.
//
// Model: imagesDir maps an image name to the content of the last tarball
// that synced successfully for it, reached through <name>, and to the
// version before it, which stays in its version directory <name>@<sha16>
// for boots that started on it. Everything else on disk is a bystander
// and must never change: other images' directories, the
// systemd-bootx64.efi the site-worker package installs into imagesDir
// (served at /files/bin/systemd-bootx64.efi and copied onto every UEFI
// target), and anything outside imagesDir.
//
// Image names reach the site worker through the site snapshot. A path-shaped
// name ("..", "a/b") is refused: it is reported in the returned error, puts
// nothing on disk, and does not stop the other images from syncing.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

func propTarball(t interface{ Fatalf(string, ...any) }, top string, files map[string]string) ([]byte, string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatalf("tar: %v", err)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[n]))}); err != nil {
			t.Fatalf("tar: %v", err)
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
			t.Fatalf("tar: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// treeDigest returns path -> content for every regular file under root,
// and path -> "-> target" for every symlink (not followed).
func treeDigest(root string) map[string]string {
	out := map[string]string{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if info.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			out[rel] = "-> " + target
			return nil
		}
		b, _ := os.ReadFile(p)
		out[rel] = string(b)
		return nil
	})
	return out
}

// imgPath is the tree key of an entry in imagesDir.
func imgPath(elem ...string) string {
	return filepath.Join(append([]string{"site", "images"}, elem...)...)
}

// served returns the content a request for <name>/<file> gets: file in the
// version directory <name> links to.
func served(tree map[string]string, name, file string) string {
	target, ok := strings.CutPrefix(tree[imgPath(name)], "-> ")
	if !ok {
		return ""
	}
	return tree[imgPath(target, file)]
}

// ownedBy reports whether tree key k belongs to image name: its link or one
// of its version directories.
func ownedBy(k, name string) bool {
	return k == imgPath(name) || strings.HasPrefix(k, imgPath(name)+"@")
}

func diffTrees(before, after map[string]string) string {
	var d []string
	for k, v := range before {
		if a, ok := after[k]; !ok {
			d = append(d, "removed "+k)
		} else if a != v {
			d = append(d, "changed "+k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			d = append(d, "added "+k)
		}
	}
	sort.Strings(d)
	return strings.Join(d, ", ")
}

type syncPropServer struct {
	mu    sync.Mutex
	blobs map[string][]byte // url path -> body
	fail  map[string]bool   // url path -> respond 500
	hits  map[string]int
}

func (s *syncPropServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hits[r.URL.Path]++
	if s.fail[r.URL.Path] {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	b, ok := s.blobs[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(b)
}

var (
	benignImageNames = []string{"noble-pxe", "jammy-pxe", "noble"}
	// Path-shaped names: each would resolve outside its own directory under
	// imagesDir, or to imagesDir itself.
	hostileImageNames = []string{"..", ".", "../victim", "../../victim", "noble-pxe/..", "a/b", "./noble-pxe", "noble-pxe/../../victim"}
)

func runSyncHistory(t *rapid.T, allowHostile bool, stats map[string]int) {
	base, err := os.MkdirTemp("", "imgprop")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	defer func() { _ = os.RemoveAll(base) }()
	// base/site/images is imagesDir; base/site/victim and base/victim are
	// bystanders outside it.
	imagesDir := filepath.Join(base, "site", "images")
	for p, c := range map[string]string{
		filepath.Join(imagesDir, "systemd-bootx64.efi"):      "PACKAGE-INSTALLED-EFI",
		filepath.Join(base, "site", "victim", "precious"):    "SITE-STATE",
		filepath.Join(base, "victim", "precious"):            "HOST-STATE",
		filepath.Join(base, "site", "worker.db"):             "LOCALDB",
		filepath.Join(imagesDir, "operator-notes", "README"): "bystander dir inside imagesDir",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	srv := &syncPropServer{blobs: map[string][]byte{}, fail: map[string]bool{}, hits: map[string]int{}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	model_ := map[string]map[string]string{} // image name -> files of last good sync
	modelSHA := map[string]string{}
	prevFiles := map[string]map[string]string{} // image name -> files of the version before it
	prevSHA := map[string]string{}

	steps := rapid.IntRange(1, 6).Draw(t, "steps")
	for step := 0; step < steps; step++ {
		pool := benignImageNames
		if allowHostile {
			pool = append(append([]string{}, benignImageNames...), hostileImageNames...)
		}
		nImgs := rapid.IntRange(1, 3).Draw(t, "n_images")
		images := map[string]*model.Image{}
		wantFiles := map[string]map[string]string{}
		wantSHA := map[string]string{}
		failing := map[string]bool{}
		for i := 0; i < nImgs; i++ {
			name := rapid.SampledFrom(pool).Draw(t, "name")
			if _, dup := images[name]; dup {
				continue
			}
			version := rapid.IntRange(1, 3).Draw(t, "version")
			files := map[string]string{
				"vmlinuz":       fmt.Sprintf("kernel-%s-v%d", name, version),
				"initrd":        fmt.Sprintf("initrd-%s-v%d", name, version),
				"initrd-local":  fmt.Sprintf("initrd-local-%s-v%d", name, version),
				"root.squashfs": fmt.Sprintf("root-%s-v%d", name, version),
			}
			// The tarball's top-level dir is whatever the build named it;
			// it need not equal the image row name.
			top := rapid.SampledFrom([]string{"noble-pxe", "image", "x"}).Draw(t, "tar_top")
			blob, sum := propTarball(t, top, files)
			path := fmt.Sprintf("/img/%d/%d-%d.tar.gz", step, i, version)
			srv.mu.Lock()
			srv.blobs[path] = blob
			fault := rapid.IntRange(0, 5).Draw(t, "fault")
			switch fault {
			case 0:
				srv.fail[path] = true // transient 500
				// A version already on disk (current or previous) is not
				// fetched, so the fault is never seen.
				if _, err := os.Stat(filepath.Join(imagesDir, name+"@"+sum[:16])); err != nil {
					failing[name] = true
				}
			case 1:
				sum = strings.Repeat("0", 64) // operator pinned the wrong digest
				failing[name] = true
			}
			srv.mu.Unlock()
			if !slices.Contains(benignImageNames, name) {
				delete(failing, name) // a refused name is never fetched
			}
			images[name] = &model.Image{Name: name, Version: fmt.Sprint(version), SHA256: sum, URL: ts.URL + path}
			wantFiles[name], wantSHA[name] = files, sum
		}

		before := treeDigest(base)
		syncErr := SyncImages(log, imagesDir, images)
		after := treeDigest(base)
		stats["sync_calls"]++
		if syncErr != nil {
			stats["sync_errors"]++
		}

		// Expected tree: start from before, apply the model update for every
		// benign image that could have synced.
		anyFailing := len(failing) > 0
		anyHostile := false
		for name := range wantFiles {
			if !slices.Contains(benignImageNames, name) {
				anyHostile = true
			}
		}
		// The error reports exactly a failed pull or a refused name.
		if (syncErr != nil) != (anyFailing || anyHostile) {
			t.Fatalf("step %d: SyncImages(images=%v) returned %v; failing pulls=%v, path-shaped names=%v",
				step, imageNames(images), syncErr, anyFailing, anyHostile)
		}
		for name, files := range wantFiles {
			if !slices.Contains(benignImageNames, name) {
				stats["hostile_names"]++
				continue
			}
			if failing[name] {
				stats["failed_pulls"]++
				// Documented: a failure before the switch leaves the link and
				// every version directory intact, and installs nothing.
				entry := func(tree map[string]string) map[string]string {
					out := map[string]string{}
					for k, v := range tree {
						if ownedBy(k, name) {
							out[k] = v
						}
					}
					return out
				}
				if d := diffTrees(entry(before), entry(after)); d != "" {
					t.Fatalf("step %d: failed pull of %q changed its cache entry: %s", step, name, d)
				}
				continue
			}
			synced := true
			for fn, content := range files {
				if served(after, name, fn) != content {
					synced = false
				}
			}
			link := after[imgPath(name)]
			if synced && link == "-> "+name+"@"+wantSHA[name][:16] {
				if old, ok := modelSHA[name]; ok && old != wantSHA[name] {
					prevFiles[name], prevSHA[name] = model_[name], old
					stats["version_switches"]++
				}
				model_[name], modelSHA[name] = files, wantSHA[name]
				stats["good_syncs"]++
				// The version before it stays whole for boots that
				// started on it.
				if p, ok := prevFiles[name]; ok {
					for fn, content := range p {
						if got := after[imgPath(name+"@"+prevSHA[name][:16], fn)]; got != content {
							t.Fatalf("step %d: image %q switched to %s but its previous version's %s reads %q, want %q",
								step, name, wantSHA[name][:16], fn, got, content)
						}
					}
				}
				continue
			}
			// Not synced. Only acceptable when another image's pull failed
			// (SyncImages stops at the first failing pull, in map order); a
			// refused name stops nothing.
			if !anyFailing {
				t.Fatalf("step %d: image %q not synced (link %q, want the %s version) and SyncImages returned %v", step, name, link, wantSHA[name][:16], syncErr)
			}
			// It must then still hold the previous good content, if any.
			if prev, ok := model_[name]; ok {
				for fn, content := range prev {
					if served(after, name, fn) != content {
						t.Fatalf("step %d: image %q lost its previous good content %s without a successful resync", step, name, fn)
					}
				}
			}
		}

		// Bystanders: nothing outside the link and version directories of
		// the valid images in this snapshot may change, whatever other names
		// it carried. A cached image the snapshot no longer lists is a
		// bystander too.
		filter := func(tree map[string]string) map[string]string {
			out := map[string]string{}
			for k, v := range tree {
				owned := false
				for b := range images {
					if !slices.Contains(benignImageNames, b) {
						continue
					}
					if ownedBy(k, b) {
						owned = true
					}
				}
				if !owned {
					out[k] = v
				}
			}
			return out
		}
		bBefore, bAfter := filter(before), filter(after)
		if d := diffTrees(bBefore, bAfter); d != "" {
			t.Fatalf("step %d: SyncImages(images=%v) changed files that belong to no synced image: %s",
				step, imageNames(images), d)
		}

		// Idempotence: an immediate re-sync of a set whose pulls all
		// succeeded downloads nothing.
		if !anyFailing {
			srv.mu.Lock()
			h0 := 0
			for _, n := range srv.hits {
				h0 += n
			}
			srv.mu.Unlock()
			if err := SyncImages(log, imagesDir, images); (err != nil) != anyHostile {
				t.Fatalf("step %d: re-sync returned %v; path-shaped names=%v", step, err, anyHostile)
			}
			srv.mu.Lock()
			h1 := 0
			for _, n := range srv.hits {
				h1 += n
			}
			srv.mu.Unlock()
			if h1 != h0 {
				t.Fatalf("step %d: re-sync of an up-to-date image set downloaded again (%d requests)", step, h1-h0)
			}
			stats["idempotent_checks"]++
		}
	}
}

func imageNames(images map[string]*model.Image) []string {
	var n []string
	for k := range images {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func reportSyncStats(t *testing.T, stats map[string]int) {
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%d", k, stats[k])
	}
	t.Logf("coverage:%s", b.String())
}

// TestProp_SyncImages_ModelAndBystanders: with valid names only, each image
// directory holds the last tarball that synced for it and nothing else on
// disk changes.
func TestProp_SyncImages_ModelAndBystanders(t *testing.T) {
	stats := map[string]int{}
	rapid.Check(t, func(t *rapid.T) { runSyncHistory(t, false, stats) })
	reportSyncStats(t, stats)
}

// TestProp_SyncImages_PathShapedNamesNeverTouchBystanders: the same history
// with path-shaped names mixed into the snapshot.
func TestProp_SyncImages_PathShapedNamesNeverTouchBystanders(t *testing.T) {
	stats := map[string]int{}
	rapid.Check(t, func(t *rapid.T) { runSyncHistory(t, true, stats) })
	reportSyncStats(t, stats)
}
