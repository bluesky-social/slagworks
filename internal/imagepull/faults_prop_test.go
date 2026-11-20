package imagepull

// Fault, crash and concurrency properties for SyncImages, extending
// sync_prop_test.go (valid tarballs, transient 500s, wrong pinned digests).
//
// Reference model (dirModel): for each image name the images directory
// holds a link <name> -> <name>@<sha16>, and one directory per version on
// disk. A sync of a row either changes nothing (already linked, or the row
// failed) or installs the row's version, switches the link and keeps exactly
// the new version and the one the link was switched away from. Before its
// first sync under this layout a name can also be a plain directory (the
// layout of earlier releases, or an images dir filled by hand) or a symlink
// an operator made; the first successful sync replaces either with the link.
// The model is checked against the whole tree after every sync: every file,
// directory and symlink under the test root, so anything a sync adds,
// removes or changes outside the model fails.
//
// The content of a version directory comes from an independent tar model
// (modelExtract): regular files only, the later of two entries for one path
// wins, the single top-level directory is stripped, and an image must carry
// the four boot files.
//
// The properties generate:
//
//   - tarball shapes: the canonical four-file image, other top-level names,
//     "./" prefixes, no directory entry, AppleDouble files, symlinks and hard
//     links pointing inside and outside, path traversal, duplicate entries, a
//     missing boot file, a boot file that is a symlink, two top-level
//     directories, a flat archive, nested files;
//   - row and transport faults: 500, 404, a body cut short, bytes that are
//     not a tarball under a matching digest, a wrong digest, another build's
//     digest, a digest in upper case with whitespace, a digest that is not a
//     digest, a row with no URL, a slow body;
//   - stalls: the bucket stops sending, before the response headers or
//     partway through the body, and the sync that follows retries only the
//     rows that failed to fetch, as the site worker's retry does;
//   - crashes: the sync runs in a child process that is SIGKILLed while its
//     download is in flight or just after it finished, and the parent then
//     syncs again as a restarted worker would;
//   - concurrency: several syncs of one directory at once, in this process
//     and in child processes, with different rows (a site worker and
//     `slagctl image sync --dir` on the same host).

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

var (
	faultLog       = slog.New(slog.NewTextHandler(io.Discard, nil))
	bootFiles      = []string{"vmlinuz", "initrd", "initrd-local", "root.squashfs"}
	faultPoolNames = []string{"noble-pxe", "jammy-pxe", "noble"}
)

// --- tarballs and the tar model ---

type tarEntry struct {
	Name string
	Type byte
	Body string
	Link string
}

func buildTar(entries []tarEntry) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.Name, Typeflag: e.Type, Mode: 0o644, Linkname: e.Link}
		switch e.Type {
		case tar.TypeDir:
			h.Mode = 0o755
		case tar.TypeReg:
			h.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(h); err != nil {
			panic(err)
		}
		if e.Type == tar.TypeReg {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				panic(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		panic(err)
	}
	if err := gz.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func digestOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// modelExtract returns what a tarball may install, independently of
// extractTarGz: one file set per top-level directory, in order of first
// appearance, or refused for an archive no part of which may be installed.
// docs/user/images.md says a tarball has exactly one top-level directory;
// for an archive with several, the model accepts any one of them.
func modelExtract(entries []tarEntry) (candidates []map[string]string, refused bool) {
	tops := map[string]map[string]string{}
	var order []string
	for _, e := range entries {
		clean := path.Clean(strings.TrimPrefix(e.Name, "/"))
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, true
		}
		if clean == "." {
			continue
		}
		top, rest, nested := strings.Cut(clean, "/")
		if e.Type == tar.TypeDir || (e.Type == tar.TypeReg && nested) {
			if _, ok := tops[top]; !ok {
				tops[top] = map[string]string{}
				order = append(order, top)
			}
		}
		if e.Type == tar.TypeReg && nested {
			tops[top][rest] = e.Body
		}
	}
	if len(order) == 0 {
		return nil, true
	}
	for _, top := range order {
		candidates = append(candidates, tops[top])
	}
	return candidates, false
}

func completeFiles(files map[string]string) bool {
	for _, f := range bootFiles {
		if _, ok := files[f]; !ok {
			return false
		}
	}
	return true
}

// build is one tarball an operator could pin for an image.
type build struct {
	shape      string
	blob       []byte
	sum        string
	candidates []map[string]string
	refused    bool
}

var buildShapes = []string{
	"canonical", "canonical", "canonical", "canonical",
	"missing-file", "boot-file-is-symlink", "appledouble", "symlink", "hardlink", "traversal",
	"duplicate-shorter", "duplicate-longer", "two-tops", "flat", "nested", "no-dir-entry",
}

func genBuild(t *rapid.T, name string, idx int, size int) build {
	shape := rapid.SampledFrom(buildShapes).Draw(t, "shape")
	top := rapid.SampledFrom([]string{name, "image", "noble-pxe"}).Draw(t, "top")
	prefix := rapid.SampledFrom([]string{"", "./"}).Draw(t, "prefix")
	body := func(f string) string {
		return fmt.Sprintf("%s:%s:build%d:", f, name, idx) + strings.Repeat("x", size)
	}
	reg := func(f string) tarEntry {
		return tarEntry{Name: prefix + top + "/" + f, Type: tar.TypeReg, Body: body(f)}
	}
	files := slices.Clone(bootFiles)
	var dropped string
	if shape == "missing-file" || shape == "boot-file-is-symlink" {
		i := rapid.IntRange(0, len(files)-1).Draw(t, "drop")
		dropped = files[i]
		files = slices.Delete(files, i, i+1)
	}
	var es []tarEntry
	if prefix == "./" && rapid.Bool().Draw(t, "dot_entry") {
		es = append(es, tarEntry{Name: "./", Type: tar.TypeDir})
	}
	if shape != "no-dir-entry" {
		es = append(es, tarEntry{Name: prefix + top + "/", Type: tar.TypeDir})
	}
	for _, f := range files {
		es = append(es, reg(f))
	}
	switch shape {
	case "boot-file-is-symlink":
		es = append(es, tarEntry{Name: prefix + top + "/" + dropped, Type: tar.TypeSymlink, Link: files[0]})
	case "appledouble":
		es = append([]tarEntry{{Name: prefix + "._" + top, Type: tar.TypeReg, Body: "meta"}}, es...)
	case "symlink":
		target := rapid.SampledFrom([]string{"/etc/passwd", "../../victim/precious", "vmlinuz"}).Draw(t, "link_target")
		es = append(es, tarEntry{Name: prefix + top + "/link", Type: tar.TypeSymlink, Link: target})
	case "hardlink":
		target := rapid.SampledFrom([]string{"/etc/passwd", prefix + top + "/vmlinuz"}).Draw(t, "link_target")
		es = append(es, tarEntry{Name: prefix + top + "/hl", Type: tar.TypeLink, Link: target})
	case "traversal":
		evil := rapid.SampledFrom([]string{"../victim/precious", top + "/../../victim/precious", "../../victim/precious"}).Draw(t, "evil")
		at := rapid.IntRange(0, len(es)).Draw(t, "evil_at")
		es = slices.Insert(es, at, tarEntry{Name: prefix + evil, Type: tar.TypeReg, Body: "OVERWRITTEN"})
	case "duplicate-shorter":
		f := rapid.SampledFrom(bootFiles).Draw(t, "dup")
		es = append(es, tarEntry{Name: prefix + top + "/" + f, Type: tar.TypeReg, Body: "x"})
	case "duplicate-longer":
		f := rapid.SampledFrom(bootFiles).Draw(t, "dup")
		es = append(es, tarEntry{Name: prefix + top + "/" + f, Type: tar.TypeReg, Body: body(f) + "-replacement"})
	case "two-tops":
		other := rapid.SampledFrom([]string{"aaa", "zzz"}).Draw(t, "other_top")
		es = append(es, tarEntry{Name: prefix + other + "/", Type: tar.TypeDir},
			tarEntry{Name: prefix + other + "/vmlinuz", Type: tar.TypeReg, Body: "other-kernel"})
		if rapid.Bool().Draw(t, "other_complete") {
			for _, f := range bootFiles[1:] {
				es = append(es, tarEntry{Name: prefix + other + "/" + f, Type: tar.TypeReg, Body: "other-" + f})
			}
		}
	case "flat":
		es = nil
		for _, f := range files {
			es = append(es, tarEntry{Name: prefix + f, Type: tar.TypeReg, Body: body(f)})
		}
	case "nested":
		es = append(es, tarEntry{Name: prefix + top + "/modules/x.ko", Type: tar.TypeReg, Body: "ko"})
	}
	b := build{shape: shape, blob: buildTar(es)}
	b.sum = digestOf(b.blob)
	b.candidates, b.refused = modelExtract(es)
	return b
}

// canonicalBuild is a complete image whose files name the build.
func canonicalBuild(name string, idx, size int) build {
	es := []tarEntry{{Name: name + "/", Type: tar.TypeDir}}
	for _, f := range bootFiles {
		es = append(es, tarEntry{Name: name + "/" + f, Type: tar.TypeReg,
			Body: fmt.Sprintf("%s:%s:build%d:", f, name, idx) + strings.Repeat("x", size)})
	}
	b := build{shape: "canonical", blob: buildTar(es)}
	b.sum = digestOf(b.blob)
	b.candidates, b.refused = modelExtract(es)
	return b
}

// --- the images directory model ---

// dirModel is the reference model of what image syncs leave in an images
// directory. Keys of dirs are version directory names, <name>@<sha16>.
type dirModel struct {
	link    map[string]string            // image name -> version directory its link names
	plain   map[string]map[string]string // image name -> files of a plain directory at <name>
	foreign map[string]string            // image name -> target of a symlink that is not a version link
	dirs    map[string]map[string]string // version directory -> files
}

func newDirModel() *dirModel {
	return &dirModel{link: map[string]string{}, plain: map[string]map[string]string{}, foreign: map[string]string{}, dirs: map[string]map[string]string{}}
}

func (m *dirModel) clone() *dirModel {
	return &dirModel{link: maps.Clone(m.link), plain: maps.Clone(m.plain), foreign: maps.Clone(m.foreign), dirs: maps.Clone(m.dirs)}
}

const (
	treeDir     = "/"
	treeLinkPfx = "-> "
)

func addTreeFiles(out map[string]string, dir string, files map[string]string) {
	out[dir] = treeDir
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		out[p] = content
		for d := filepath.Dir(p); d != dir; d = filepath.Dir(d) {
			out[d] = treeDir
		}
	}
}

// render is the tree the model expects under the images directory for the
// entries images own (see ownedEntry).
func (m *dirModel) render() map[string]string {
	out := map[string]string{}
	for name, ver := range m.link {
		out[name] = treeLinkPfx + ver
	}
	for name, target := range m.foreign {
		out[name] = treeLinkPfx + target
	}
	for name, files := range m.plain {
		addTreeFiles(out, name, files)
	}
	for ver, files := range m.dirs {
		addTreeFiles(out, ver, files)
	}
	return out
}

// row is one image row handed to a sync, with what the bucket does for it.
type row struct {
	name   string
	pinned string // the row's sha256 as written
	url    bool   // false: the row has no URL
	// What a download of the row's URL gets:
	transport  string // "", "500", "404", "truncate"
	servedSum  string // digest of the bytes served
	candidates []map[string]string
	refused    bool
}

type rowResult struct {
	failed      bool // the sync reports an error for this image
	fetchFailed bool // and FetchFailed names it
	fetched     bool // the sync requested the row's URL
}

func isDigest(s string) bool {
	return len(s) == 64 && strings.Trim(s, "0123456789abcdef") == ""
}

// sync applies one row to the model. pick chooses among the top-level
// directories of a tarball that has more than one.
func (m *dirModel) sync(r row, pick func(ver string, candidates []map[string]string) map[string]string) rowResult {
	if !r.url {
		return rowResult{}
	}
	want := strings.ToLower(strings.TrimSpace(r.pinned))
	if !isDigest(want) {
		return rowResult{failed: true}
	}
	ver := r.name + "@" + want[:16]
	if m.link[r.name] == ver && m.dirs[ver] != nil {
		return rowResult{}
	}
	var res rowResult
	if m.dirs[ver] == nil {
		res.fetched = true
		if r.transport != "" {
			return rowResult{failed: true, fetchFailed: true, fetched: true}
		}
		if want != r.servedSum || r.refused {
			return rowResult{failed: true, fetched: true}
		}
		files := r.candidates[0]
		if len(r.candidates) > 1 {
			files = pick(ver, r.candidates)
		}
		if files == nil || !completeFiles(files) {
			return rowResult{failed: true, fetched: true}
		}
		m.dirs[ver] = files
	}
	old, linked := m.link[r.name]
	m.link[r.name] = ver
	delete(m.plain, r.name)
	delete(m.foreign, r.name)
	for d := range m.dirs {
		if strings.HasPrefix(d, r.name+"@") && d != ver && (!linked || d != old) {
			delete(m.dirs, d)
		}
	}
	return res
}

// --- reading the directory ---

// diskTree returns every entry under root by relative path: the content of a
// regular file, "-> target" for a symlink (not followed), "/" for a
// directory. An entry that disappears during the walk reads as absent.
func diskTree(root string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // removed mid-walk
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = treeLinkPfx + target
		case d.IsDir():
			out[rel] = treeDir
		default:
			b, err := os.ReadFile(p)
			if err == nil {
				out[rel] = string(b)
			}
		}
		return nil
	})
	return out
}

func isTempEntry(top string) bool {
	return slices.ContainsFunc([]string{".image-download-", ".image-extract-", ".image-link-", ".image-old-"},
		func(p string) bool { return strings.HasPrefix(top, p) })
}

// ownedEntry reports whether the images-dir entry top belongs to one of
// names: its link or one of its version directories.
func ownedEntry(top string, names []string) bool {
	for _, n := range names {
		if top == n || strings.HasPrefix(top, n+"@") {
			return true
		}
	}
	return false
}

// splitTree divides a tree of the images directory into the entries images
// own, the temp entries of a sync, and everything else.
func splitTree(tree map[string]string, names []string) (owned, temps, bystanders map[string]string) {
	owned, temps, bystanders = map[string]string{}, map[string]string{}, map[string]string{}
	for k, v := range tree {
		top, _, _ := strings.Cut(k, string(filepath.Separator))
		switch {
		case isTempEntry(top):
			temps[k] = v
		case ownedEntry(top, names):
			owned[k] = v
		default:
			bystanders[k] = v
		}
	}
	return owned, temps, bystanders
}

// subTree returns the entries of tree under dir, relative to it.
func subTree(tree map[string]string, dir string) map[string]string {
	out := map[string]string{}
	for k, v := range tree {
		if rest, ok := strings.CutPrefix(k, dir+string(filepath.Separator)); ok {
			out[rest] = v
		}
	}
	return out
}

func filesTree(files map[string]string) map[string]string {
	out := map[string]string{}
	addTreeFiles(out, ".", files)
	delete(out, ".")
	return out
}

// --- the bucket ---

type faultServer struct {
	mu    sync.Mutex
	blobs map[string][]byte
	mode  map[string]string // "", "500", "404", "truncate", "slow", "stall:<k>" (stop after k bytes and hold the connection)
	hits  map[string]int
	// stallCh receives the path once a stall-mode request has sent its k
	// bytes; doneCh receives the path of every body served in full.
	stallCh chan string
	doneCh  chan string
}

func newFaultServer() (*faultServer, *httptest.Server) {
	s := &faultServer{blobs: map[string][]byte{}, mode: map[string]string{}, hits: map[string]int{},
		stallCh: make(chan string, 16), doneCh: make(chan string, 16)}
	return s, httptest.NewServer(s)
}

func (s *faultServer) put(p string, blob []byte, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[p] = blob
	s.mode[p] = mode
}

func (s *faultServer) hitCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.hits)
}

func (s *faultServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.hits[r.URL.Path]++
	b, ok := s.blobs[r.URL.Path]
	mode := s.mode[r.URL.Path]
	s.mu.Unlock()
	if !ok || mode == "404" {
		http.NotFound(w, r)
		return
	}
	switch {
	case mode == "500":
		http.Error(w, "boom", http.StatusInternalServerError)
	case mode == "truncate":
		// Returning with less than the declared length written makes
		// net/http close the connection.
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b[:len(b)/2])
	case mode == "slow":
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		for i := 0; i < len(b); i += 97 {
			_, _ = w.Write(b[i:min(i+97, len(b))])
			w.(http.Flusher).Flush()
		}
	case strings.HasPrefix(mode, "stall:"):
		// A negative k stalls before the response headers.
		if k, _ := strconv.Atoi(strings.TrimPrefix(mode, "stall:")); k >= 0 {
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b[:k])
			w.(http.Flusher).Flush()
		}
		select {
		case s.stallCh <- r.URL.Path:
		default:
		}
		<-r.Context().Done()
	default:
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
		w.(http.Flusher).Flush()
		select {
		case s.doneCh <- r.URL.Path:
		default:
		}
	}
}

// --- the world one history runs in ---

type faultWorld struct {
	base, imagesDir string
	srv             *faultServer
	url             string
	names           []string
	m               *dirModel
	// versions holds, for every version directory a generated build could
	// install, the file sets it may hold.
	versions map[string][]map[string]string
	// bystanders is everything under base that no image owns, as seeded.
	bystanders map[string]string
	stats      map[string]int
}

func (w *faultWorld) imagesTree() map[string]string { return diskTree(w.imagesDir) }

// outsideTree is everything under base except the images an image owns and
// sync temp entries: the entries no sync may ever change.
func (w *faultWorld) outsideTree() map[string]string {
	out := map[string]string{}
	imgRel, _ := filepath.Rel(w.base, w.imagesDir)
	for k, v := range diskTree(w.base) {
		if rest, ok := strings.CutPrefix(k, imgRel+string(filepath.Separator)); ok {
			top, _, _ := strings.Cut(rest, string(filepath.Separator))
			if isTempEntry(top) || ownedEntry(top, w.names) {
				continue
			}
		}
		out[k] = v
	}
	return out
}

func (w *faultWorld) noteBuild(name string, b build) {
	if b.refused {
		return
	}
	ver := name + "@" + b.sum[:16]
	for _, c := range b.candidates {
		if completeFiles(c) {
			w.versions[ver] = append(w.versions[ver], c)
		}
	}
}

// newFaultWorld seeds an images directory with the entries a site worker's
// images dir holds that are not images, and files outside it.
func newFaultWorld(t *rapid.T, stats map[string]int) (*faultWorld, func()) {
	base, err := os.MkdirTemp("", "imgfault")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	// macOS temp dirs sit behind a symlink; nothing here depends on it.
	w := &faultWorld{base: base, imagesDir: filepath.Join(base, "site", "images"), names: faultPoolNames,
		m: newDirModel(), versions: map[string][]map[string]string{}, stats: stats}
	for p, c := range map[string]string{
		filepath.Join(base, "site", "efi", "systemd-bootx64.efi"):  "PACKAGE-EFI",
		filepath.Join(base, "site", "victim", "precious"):          "SITE-STATE",
		filepath.Join(base, "victim", "precious"):                  "HOST-STATE",
		filepath.Join(w.imagesDir, "operator-notes", "README"):     "a directory that is not an image",
		filepath.Join(w.imagesDir, "noble-pxe.sha256"):             "a marker file an earlier release wrote\n",
		filepath.Join(w.imagesDir, "noble-pxe-rollback", "README"): "an entry whose name starts with an image's name",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// The package links the bootloader into the images dir.
	if err := os.Symlink("../efi/systemd-bootx64.efi", filepath.Join(w.imagesDir, "systemd-bootx64.efi")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var ts *httptest.Server
	w.srv, ts = newFaultServer()
	w.url = ts.URL
	return w, func() {
		ts.Close()
		_ = os.RemoveAll(base)
	}
}

// seedInitial puts each name in a state it can be in before its first sync
// under the link layout.
func (w *faultWorld) seedInitial(t *rapid.T) {
	for _, name := range w.names {
		switch rapid.SampledFrom([]string{"absent", "absent", "plain-dir", "plain-dir", "operator-symlink"}).Draw(t, "initial_"+name) {
		case "plain-dir":
			files := map[string]string{}
			for _, f := range bootFiles {
				files[f] = f + ":" + name + ":pre-link"
			}
			w.m.plain[name] = files
			for f, c := range files {
				p := filepath.Join(w.imagesDir, name, f)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatalf("seed: %v", err)
				}
				if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			w.stats["initial_plain_dir"]++
		case "operator-symlink":
			w.m.foreign[name] = "operator-notes"
			if err := os.Symlink("operator-notes", filepath.Join(w.imagesDir, name)); err != nil {
				t.Fatalf("seed: %v", err)
			}
			w.stats["initial_operator_symlink"]++
		}
	}
	w.bystanders = w.outsideTree()
}

var rowFaults = []string{"", "", "", "", "500", "404", "truncate", "corrupt", "wrongsha", "othersha", "upper", "notadigest", "nourl", "slow"}

// genRows draws a set of image rows over builds and publishes their
// tarballs under a path unique to this step.
func (w *faultWorld) genRows(t *rapid.T, step string, builds map[string][]build, faults []string) (map[string]*model.Image, map[string]row, map[string]string) {
	images := map[string]*model.Image{}
	rows := map[string]row{}
	paths := map[string]string{}
	n := rapid.IntRange(1, len(w.names)).Draw(t, "n_images")
	for i := 0; i < n; i++ {
		name := rapid.SampledFrom(w.names).Draw(t, "name")
		if _, dup := images[name]; dup {
			continue
		}
		idx := rapid.IntRange(0, len(builds[name])-1).Draw(t, "build")
		b := builds[name][idx]
		fault := rapid.SampledFrom(faults).Draw(t, "fault")
		p := fmt.Sprintf("/%s/%s/%d.tar.gz", step, name, idx)
		r := row{name: name, pinned: b.sum, url: true, servedSum: b.sum, candidates: b.candidates, refused: b.refused}
		blob, mode := b.blob, ""
		switch fault {
		case "500", "404", "truncate":
			r.transport, mode = fault, fault
		case "slow":
			mode = fault
		case "corrupt":
			// An upload that is not a gzip stream, pinned by its own digest.
			blob = append([]byte("not gzip "), blob...)
			r.servedSum = digestOf(blob)
			r.pinned, r.refused, r.candidates = r.servedSum, true, nil
		case "wrongsha":
			r.pinned = strings.Repeat("0", 64)
		case "othersha":
			// The row carries another build's digest over this build's URL.
			r.pinned = builds[name][(idx+1)%len(builds[name])].sum
		case "upper":
			r.pinned = " " + strings.ToUpper(b.sum) + "\n"
		case "notadigest":
			r.pinned = rapid.SampledFrom([]string{"sha256:" + b.sum, b.sum[:63], b.sum + "0", "../" + b.sum[3:], "latest"}).Draw(t, "bad_digest")
		case "nourl":
			r.url = false
		}
		w.srv.put(p, blob, mode)
		img := &model.Image{Name: name, Version: strconv.Itoa(idx), SHA256: r.pinned}
		if r.url {
			img.URL = w.url + p
		}
		images[name], rows[name], paths[name] = img, r, p
		w.stats["fault_"+fault]++
		w.stats["shape_"+b.shape]++
	}
	return images, rows, paths
}

// syncAndCheck runs SyncImages for rows and checks the directory, the
// returned error and the requests the bucket saw against the model.
func (w *faultWorld) syncAndCheck(t *rapid.T, when string, images map[string]*model.Image, rows map[string]row, paths map[string]string) {
	hitsBefore := w.srv.hitCounts()
	syncErr := SyncImages(faultLog, w.imagesDir, images)
	hitsAfter := w.srv.hitCounts()
	owned, temps, _ := splitTree(w.imagesTree(), w.names)
	w.stats["syncs"]++

	// A tarball with several top-level directories installs any one of
	// them: take the one on disk, if the link moved to this version.
	pick := func(ver string, candidates []map[string]string) map[string]string {
		w.stats["two_top_dirs"]++
		name, _, _ := strings.Cut(ver, "@")
		if owned[name] != treeLinkPfx+ver {
			return nil
		}
		got := subTree(owned, ver)
		for _, c := range candidates {
			if diffTrees(filesTree(c), got) == "" {
				return c
			}
		}
		t.Fatalf("%s: %s holds none of its tarball's top-level directories in full: %v", when, ver, got)
		return nil
	}
	var wantFailed, wantFetchFailed []string
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		res := w.m.sync(rows[name], pick)
		if res.failed {
			wantFailed = append(wantFailed, name)
			w.stats["rows_failed"]++
		}
		if res.fetchFailed {
			wantFetchFailed = append(wantFetchFailed, name)
		}
		wantHits := 0
		if res.fetched {
			wantHits = 1
			w.stats["rows_fetched"]++
		}
		if got := hitsAfter[paths[name]] - hitsBefore[paths[name]]; got != wantHits {
			t.Fatalf("%s: image %q was requested %d times, want %d", when, name, got, wantHits)
		}
	}
	if (syncErr != nil) != (len(wantFailed) > 0) {
		t.Fatalf("%s: SyncImages returned %v; rows the model fails: %v", when, syncErr, wantFailed)
	}
	if got := FetchFailed(syncErr); !slices.Equal(got, wantFetchFailed) {
		t.Fatalf("%s: FetchFailed = %v, want %v (err %v)", when, got, wantFetchFailed, syncErr)
	}
	if d := diffTrees(w.m.render(), owned); d != "" {
		t.Fatalf("%s: images on disk differ from the model (err %v): %s", when, syncErr, d)
	}
	if len(temps) > 0 {
		t.Fatalf("%s: SyncImages returned (err %v) leaving temp entries: %v", when, syncErr, slices.Sorted(maps.Keys(temps)))
	}
	if d := diffTrees(w.bystanders, w.outsideTree()); d != "" {
		t.Fatalf("%s: entries no image owns changed: %s", when, d)
	}
	for name, ver := range w.m.link {
		if got, ok := Installed(w.imagesDir, name); !ok || got != ver {
			t.Fatalf("%s: Installed(%q) = %q, %v; the model links it to %s", when, name, got, ok, ver)
		}
	}
}

func (w *faultWorld) genBuilds(t *rapid.T, perName, size int) map[string][]build {
	builds := map[string][]build{}
	for _, name := range w.names {
		for i := 0; i < perName; i++ {
			b := genBuild(t, name, i, size)
			builds[name] = append(builds[name], b)
			w.noteBuild(name, b)
		}
	}
	return builds
}

// TestProp_SyncImages_FaultSchedule replays histories of syncs over tarball
// shapes and row and transport faults against the directory model.
func TestProp_SyncImages_FaultSchedule(t *testing.T) {
	stats := map[string]int{}
	rapid.Check(t, func(t *rapid.T) {
		w, cleanup := newFaultWorld(t, stats)
		defer cleanup()
		w.seedInitial(t)
		builds := w.genBuilds(t, 3, 0)
		steps := rapid.IntRange(1, 8).Draw(t, "steps")
		for step := 0; step < steps; step++ {
			images, rows, paths := w.genRows(t, fmt.Sprintf("s%d", step), builds, rowFaults)
			w.syncAndCheck(t, fmt.Sprintf("step %d", step), images, rows, paths)
		}
	})
	reportSyncStats(t, stats)
}

// rapidChecksGiven reports whether -rapid.checks was on the command line.
// It is read once, before checkN sets the flag itself.
var rapidChecksGiven = sync.OnceValue(func() bool {
	given := false
	flag.Visit(func(f *flag.Flag) { given = given || f.Name == "rapid.checks" })
	return given
})

// checkN is rapid.Check with n checks instead of rapid's default, for
// properties whose every check spawns processes or waits out a timeout. An
// explicit -rapid.checks still decides, so a deep run scales all of them.
func checkN(t *testing.T, n int, prop func(*rapid.T)) {
	t.Helper()
	if f := flag.Lookup("rapid.checks"); f != nil && !rapidChecksGiven() {
		old := f.Value.String()
		if err := flag.Set("rapid.checks", strconv.Itoa(n)); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = flag.Set("rapid.checks", old) }()
	}
	rapid.Check(t, prop)
}

// TestProp_SyncImages_StalledDownloadThenRetry: a download whose server
// stops sending, before the response headers or anywhere in the body, is a
// failure to fetch like any other. It changes nothing on disk, leaves no
// temp entry, and is reported by FetchFailed, which is what makes the site
// worker try again. The worker's retry syncs only the rows FetchFailed
// named; replayed here, it must bring exactly those rows to what the model
// gives, whether or not the bucket has recovered, and leave the others
// alone.
func TestProp_SyncImages_StalledDownloadThenRetry(t *testing.T) {
	shortStallTimeout(t, 300*time.Millisecond)
	stats := map[string]int{}
	checkN(t, 20, func(t *rapid.T) {
		w, cleanup := newFaultWorld(t, stats)
		defer cleanup()
		w.seedInitial(t)
		builds := w.genBuilds(t, 2, 0)
		steps := rapid.IntRange(1, 3).Draw(t, "steps")
		stalled := false
		for step := 0; step < steps; step++ {
			images, rows, paths := w.genRows(t, fmt.Sprintf("s%d", step), builds, []string{"", "", "", "500", "truncate", "wrongsha"})
			// One stall per history keeps the property's run time down.
			if !stalled {
				victim := rapid.SampledFrom(slices.Sorted(maps.Keys(rows))).Draw(t, "stall_victim")
				if r := rows[victim]; r.transport == "" {
					w.srv.mu.Lock()
					at := rapid.IntRange(-1, len(w.srv.blobs[paths[victim]])-1).Draw(t, "stall_at")
					w.srv.mode[paths[victim]] = "stall:" + strconv.Itoa(at)
					w.srv.mu.Unlock()
					r.transport = "stall"
					rows[victim] = r
					stalled = true
					stats["rows_stalled"]++
				}
			}
			began := time.Now()
			w.syncAndCheck(t, fmt.Sprintf("step %d", step), images, rows, paths)
			if d := time.Since(began); d > 10*time.Second {
				t.Fatalf("step %d: a sync with a stall timeout of %s took %s", step, stallTimeout, d)
			}

			retryImages, retryRows := map[string]*model.Image{}, map[string]row{}
			for name, r := range rows {
				if r.transport == "" {
					continue
				}
				// The bucket recovers, or keeps failing (without another
				// stall to wait out).
				mode := rapid.SampledFrom([]string{"", "", "500"}).Draw(t, "retry_"+name)
				w.srv.mu.Lock()
				w.srv.mode[paths[name]] = mode
				w.srv.mu.Unlock()
				r.transport = mode
				retryImages[name], retryRows[name] = images[name], r
			}
			if len(retryRows) > 0 {
				stats["retries"]++
				w.syncAndCheck(t, fmt.Sprintf("retry after step %d", step), retryImages, retryRows, paths)
			}
		}
	})
	reportSyncStats(t, stats)
}

// --- crashes ---

const (
	syncChildEnv    = "SLAGWORKS_IMAGEPULL_SYNC_CHILD"
	syncChildImages = "SLAGWORKS_IMAGEPULL_SYNC_CHILD_IMAGES"
)

// TestHelperSyncChild is the child process of the crash and concurrency
// properties: it runs SyncImages once, for the images in its environment,
// on its working directory. In a normal test run it does nothing.
func TestHelperSyncChild(t *testing.T) {
	if os.Getenv(syncChildEnv) != "1" {
		t.Skip("child process of the SyncImages crash and concurrency properties")
	}
	var images map[string]*model.Image
	if err := json.Unmarshal([]byte(os.Getenv(syncChildImages)), &images); err != nil {
		t.Fatal(err)
	}
	_ = SyncImages(faultLog, ".", images)
}

// startSyncChild starts a child sync of imagesDir. exited delivers the
// child's exit error once: nil when it ran to the end, whatever the sync
// itself returned; an error when it was killed, panicked, or the race
// detector reported a race in it.
func startSyncChild(t *rapid.T, imagesDir string, images map[string]*model.Image) (cmd *exec.Cmd, exited chan error) {
	enc, err := json.Marshal(images)
	if err != nil {
		t.Fatalf("encode images: %v", err)
	}
	cmd = exec.Command(os.Args[0], "-test.run=^TestHelperSyncChild$", "-test.count=1")
	// Under -race a process sleeps a second before it exits unless told
	// otherwise, which would be most of the run time here.
	cmd.Dir = imagesDir
	cmd.Env = append(os.Environ(), syncChildEnv+"=1", syncChildImages+"="+string(enc),
		"GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	exited = make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return cmd, exited
}

func drain(ch chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// killSync runs a sync of images in a child process and SIGKILLs it: when
// stallAt >= 0, once the bucket has sent that many bytes of victimPath and
// stopped; otherwise delay after the bucket finished sending victimPath. A
// child that exits first is left alone.
func (w *faultWorld) killSync(t *rapid.T, images map[string]*model.Image, victimPath string, stallAt int, delay time.Duration) {
	if stallAt >= 0 {
		w.srv.mu.Lock()
		prev := w.srv.mode[victimPath]
		w.srv.mode[victimPath] = "stall:" + strconv.Itoa(stallAt)
		w.srv.mu.Unlock()
		defer func() {
			w.srv.mu.Lock()
			w.srv.mode[victimPath] = prev
			w.srv.mu.Unlock()
		}()
	}
	drain(w.srv.stallCh)
	drain(w.srv.doneCh)
	cmd, exited := startSyncChild(t, w.imagesDir, images)
	timeout := time.After(30 * time.Second)
wait:
	for {
		select {
		case p := <-w.srv.stallCh:
			if p == victimPath {
				w.stats["kill_mid_download"]++
				break wait
			}
		case p := <-w.srv.doneCh:
			if stallAt < 0 && p == victimPath {
				time.Sleep(delay)
				w.stats["kill_after_download"]++
				break wait
			}
		case err := <-exited:
			if err != nil {
				t.Fatalf("child sync failed by itself: %v", err)
			}
			w.stats["kill_child_already_exited"]++
			return
		case <-timeout:
			_ = cmd.Process.Kill()
			<-exited
			t.Fatalf("child sync neither fetched %s nor exited in 30s", victimPath)
		}
	}
	_ = cmd.Process.Signal(syscall.SIGKILL)
	<-exited
}

// checkAfterKill checks what a killed sync may leave and brings the model
// up to date with the directory. The model holds the state before the killed
// sync. At any instant:
//
//   - a version directory is a complete image or absent;
//   - a name that was a link is still a link, to a version directory that
//     exists;
//   - a name that was a plain directory or an operator's symlink is that,
//     unchanged, or a link to a version directory that exists;
//   - nothing else changed, apart from temp entries, which the next sync
//     removes.
//
// It returns the names that were plain directories and are now missing: the
// first switch of a plain directory moves it aside and then renames the link
// in, and a kill between the two renames leaves no <name> until the next
// sync.
func (w *faultWorld) checkAfterKill(t *rapid.T, when string) (missing []string) {
	tree := w.imagesTree()
	owned, _, _ := splitTree(tree, w.names)
	next := newDirModel()
	for k, v := range owned {
		if strings.Contains(k, string(filepath.Separator)) {
			continue
		}
		name, _, isVersion := strings.Cut(k, "@")
		if !isVersion {
			continue
		}
		if v != treeDir {
			t.Fatalf("%s: %s is not a directory: %q", when, k, v)
		}
		got := subTree(owned, k)
		var files map[string]string
		for _, c := range w.versions[k] {
			if diffTrees(filesTree(c), got) == "" {
				files = c
			}
		}
		if files == nil {
			t.Fatalf("%s: version directory %s of image %q is not a complete image of that version: %v", when, k, name, got)
		}
		next.dirs[k] = files
	}
	for _, name := range w.names {
		entry, present := owned[name]
		target, isLink := strings.CutPrefix(entry, treeLinkPfx)
		_, wasLink := w.m.link[name]
		wasPlain := w.m.plain[name] != nil
		wasForeign := w.m.foreign[name] != ""
		switch {
		case !present:
			if wasLink || wasForeign {
				t.Fatalf("%s: %s was a symlink before the killed sync and is now missing", when, name)
			}
			if wasPlain {
				missing = append(missing, name)
			}
		case isLink && strings.HasPrefix(target, name+"@"):
			if next.dirs[target] == nil {
				t.Fatalf("%s: %s links to %s, which is not on disk", when, name, target)
			}
			next.link[name] = target
		case isLink:
			if !wasForeign || target != w.m.foreign[name] {
				t.Fatalf("%s: %s is a symlink to %q", when, name, target)
			}
			next.foreign[name] = target
		default:
			if !wasPlain {
				t.Fatalf("%s: %s became a plain directory", when, name)
			}
			if d := diffTrees(filesTree(w.m.plain[name]), subTree(owned, name)); d != "" {
				t.Fatalf("%s: plain directory %s changed while it was still served: %s", when, name, d)
			}
			next.plain[name] = w.m.plain[name]
		}
	}
	if d := diffTrees(w.bystanders, w.outsideTree()); d != "" {
		t.Fatalf("%s: entries no image owns changed: %s", when, d)
	}
	w.m = next
	return missing
}

// TestProp_SyncImages_CrashRestart kills syncs at random points of a fault
// history, checks what each kill left, and requires the restarted sync to
// reach exactly the state the model gives for it.
func TestProp_SyncImages_CrashRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns child processes")
	}
	stats := map[string]int{}
	rapid.Check(t, func(t *rapid.T) {
		w, cleanup := newFaultWorld(t, stats)
		defer cleanup()
		w.seedInitial(t)
		builds := w.genBuilds(t, 3, rapid.SampledFrom([]int{0, 4 << 10}).Draw(t, "file_size"))
		steps := rapid.IntRange(1, 4).Draw(t, "steps")
		for step := 0; step < steps; step++ {
			images, rows, paths := w.genRows(t, fmt.Sprintf("s%d", step), builds, rowFaults)
			if rapid.Bool().Draw(t, "crash") {
				kills := rapid.IntRange(1, 2).Draw(t, "kills")
				for k := 0; k < kills; k++ {
					victim := rapid.SampledFrom(slices.Sorted(maps.Keys(paths))).Draw(t, "victim")
					p := paths[victim]
					stallAt := -1
					w.srv.mu.Lock()
					size, mode := len(w.srv.blobs[p]), w.srv.mode[p]
					w.srv.mu.Unlock()
					if mode == "" && rapid.Bool().Draw(t, "mid_download") {
						stallAt = rapid.IntRange(0, size-1).Draw(t, "stall_at")
					}
					delay := time.Duration(rapid.IntRange(0, 3000).Draw(t, "delay_us")) * time.Microsecond
					w.killSync(t, images, p, stallAt, delay)
					if missing := w.checkAfterKill(t, fmt.Sprintf("step %d after kill %d", step, k)); len(missing) > 0 {
						stats["kill_left_plain_dir_name_missing"]++
					}
				}
			}
			w.syncAndCheck(t, fmt.Sprintf("step %d", step), images, rows, paths)
		}
	})
	reportSyncStats(t, stats)
}

// TestProp_SyncImages_CrashInSwitchWindow aims kills at the short stretch in
// which a sync, its download done, installs the version directory, switches
// the link and removes the version before the previous one. It bisects the
// kill delay between "nothing changed yet" and "the sync finished"; a kill
// that leaves anything else landed inside that stretch. There the crash
// invariants must hold (checkAfterKill), a fetch by name must get a complete
// image, and a restarted sync must finish the job.
func TestProp_SyncImages_CrashInSwitchWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns child processes")
	}
	stats := map[string]int{}
	checkN(t, 40, func(t *rapid.T) {
		w, cleanup := newFaultWorld(t, stats)
		defer cleanup()
		w.names = []string{"noble-pxe"}
		w.bystanders = w.outsideTree()
		// Larger files make extraction and removal, and so the stretch,
		// longer.
		size := rapid.SampledFrom([]int{16, 4 << 10, 256 << 10}).Draw(t, "file_size")
		start := rapid.SampledFrom([]string{"two-versions", "one-version", "plain-dir"}).Draw(t, "start")
		var builds []build
		for i := 0; i < 3; i++ {
			b := canonicalBuild("noble-pxe", i, size)
			builds = append(builds, b)
			w.noteBuild("noble-pxe", b)
		}
		rowFor := func(i int) (map[string]*model.Image, map[string]row, map[string]string) {
			p := fmt.Sprintf("/noble-pxe/%d.tar.gz", i)
			w.srv.put(p, builds[i].blob, "")
			return map[string]*model.Image{"noble-pxe": {Name: "noble-pxe", SHA256: builds[i].sum, URL: w.url + p}},
				map[string]row{"noble-pxe": {name: "noble-pxe", pinned: builds[i].sum, url: true, servedSum: builds[i].sum, candidates: builds[i].candidates}},
				map[string]string{"noble-pxe": p}
		}
		var saved *dirModel
		var savedTree map[string]string
		reset := func() {
			if err := os.RemoveAll(filepath.Join(w.imagesDir, "noble-pxe")); err != nil {
				t.Fatalf("reset: %v", err)
			}
			ents, _ := os.ReadDir(w.imagesDir)
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), "noble-pxe@") || isTempEntry(e.Name()) {
					if err := os.RemoveAll(filepath.Join(w.imagesDir, e.Name())); err != nil {
						t.Fatalf("reset: %v", err)
					}
				}
			}
			w.m = newDirModel()
			switch start {
			case "plain-dir":
				files := builds[0].candidates[0]
				w.m.plain["noble-pxe"] = files
				for f, c := range files {
					p := filepath.Join(w.imagesDir, "noble-pxe", f)
					_ = os.MkdirAll(filepath.Dir(p), 0o755)
					if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
						t.Fatalf("reset: %v", err)
					}
				}
			case "two-versions":
				for _, i := range []int{0, 1} {
					images, rows, paths := rowFor(i)
					w.syncAndCheck(t, "reset", images, rows, paths)
				}
			case "one-version":
				images, rows, paths := rowFor(1)
				w.syncAndCheck(t, "reset", images, rows, paths)
			}
			saved = w.m.clone()
			savedTree, _, _ = splitTree(w.imagesTree(), w.names)
		}
		reset()
		images, rows, paths := rowFor(2)
		final := saved.clone()
		final.sync(rows["noble-pxe"], nil)
		wantNew := final.render()

		lo, hi := 0, 20000 // microseconds after the download finished
		for i := 0; i < 14 && hi-lo > 1; i++ {
			d := (lo + hi) / 2
			w.killSync(t, images, paths["noble-pxe"], -1, time.Duration(d)*time.Microsecond)
			owned, temps, _ := splitTree(w.imagesTree(), w.names)
			switch {
			case diffTrees(savedTree, owned) == "" && len(temps) <= 2:
				// Killed before installing anything; at most the download
				// and the extraction are left behind.
				stats["kill_too_early"]++
				lo = d
				if len(temps) > 0 {
					// Leftovers do not change the state a sync starts from;
					// the next child sweeps them.
					stats["kill_left_temps_only"]++
				}
				continue
			case diffTrees(wantNew, owned) == "" && len(temps) == 0:
				stats["kill_too_late"]++
				hi = d
				reset()
				continue
			}
			stats["kill_in_window_from_"+start]++
			missing := w.checkAfterKill(t, fmt.Sprintf("kill %dus after the download (start %s)", d, start))
			if len(missing) > 0 {
				// The first switch of a plain directory moves it aside and
				// then renames the link in: two renames, and a kill between
				// them leaves no <name> until the next sync.
				stats["kill_left_plain_dir_name_missing"]++
			}
			w.syncAndCheck(t, "restart after a kill in the window", images, rows, paths)
			return
		}
	})
	reportSyncStats(t, stats)
}

// --- concurrency ---

// TestProp_SyncImages_ConcurrentSyncsSerialize runs several syncs of one
// images directory at once, some in this process and some in child
// processes, each with its own rows: a site worker and `slagctl image sync
// --dir` on the same host, or two workers started by mistake. Meanwhile
// readers fetch images the way the boot handler and its file server do.
//
//   - The directory ends in the state the model gives for the syncs run one
//     after another in some order.
//   - A reader that resolves the link once and reads the four boot files
//     from that version directory gets one version's files or, when that
//     version has since been removed, a missing file; never another
//     version's bytes or a partial file.
//   - A fetch by name of an image that was linked before the syncs started
//     always succeeds.
func TestProp_SyncImages_ConcurrentSyncsSerialize(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns child processes")
	}
	stats := map[string]int{}
	rapid.Check(t, func(t *rapid.T) {
		w, cleanup := newFaultWorld(t, stats)
		defer cleanup()
		w.names = []string{"noble-pxe", "jammy-pxe"}
		w.bystanders = w.outsideTree()
		builds := map[string][]build{}
		for _, name := range w.names {
			for i := 0; i < 4; i++ {
				b := canonicalBuild(name, i, 2048)
				builds[name] = append(builds[name], b)
				w.noteBuild(name, b)
			}
		}
		// noble-pxe starts linked, jammy-pxe as drawn.
		first, firstRows, firstPaths := w.genRowsFixed("init", builds, map[string]int{"noble-pxe": 0})
		w.syncAndCheck(t, "initial sync", first, firstRows, firstPaths)
		if rapid.Bool().Draw(t, "jammy_plain_dir") {
			files := map[string]string{"vmlinuz": "pre-link"}
			w.m.plain["jammy-pxe"] = files
			p := filepath.Join(w.imagesDir, "jammy-pxe", "vmlinuz")
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte("pre-link"), 0o644); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}

		type participant struct {
			images map[string]*model.Image
			rows   map[string]row
			child  bool
		}
		n := rapid.IntRange(2, 3).Draw(t, "syncs")
		parts := make([]participant, n)
		for i := range parts {
			images, rows, _ := w.genRows(t, fmt.Sprintf("p%d", i), builds, []string{"", "", "", "500", "wrongsha", "slow"})
			parts[i] = participant{images: images, rows: rows, child: rapid.Bool().Draw(t, "child")}
		}

		var stop atomic.Bool
		var readerErr atomic.Pointer[string]
		fail := func(format string, args ...any) {
			s := fmt.Sprintf(format, args...)
			readerErr.CompareAndSwap(nil, &s)
		}
		var readers sync.WaitGroup
		for r := 0; r < 2; r++ {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for !stop.Load() {
					time.Sleep(100 * time.Microsecond)
					// The file server's read of a URL that names the image.
					if _, err := os.ReadFile(filepath.Join(w.imagesDir, "noble-pxe", "vmlinuz")); err != nil {
						fail("fetch by name of a linked image failed: %v", err)
					}
					// The boot handler's read of the link, then a boot's
					// fetches from the version directory it named.
					ver, ok := Installed(w.imagesDir, "noble-pxe")
					if !ok {
						fail("noble-pxe stopped being a link to a version directory")
						continue
					}
					for _, f := range bootFiles {
						b, err := os.ReadFile(filepath.Join(w.imagesDir, ver, f))
						if errors.Is(err, fs.ErrNotExist) {
							continue
						}
						if err != nil {
							fail("read %s/%s: %v", ver, f, err)
							continue
						}
						if want := w.versions[ver][0][f]; string(b) != want {
							fail("%s/%s read %d bytes that are not that version's file", ver, f, len(b))
						}
					}
				}
			}()
		}

		var syncs sync.WaitGroup
		var children []chan error
		for _, p := range parts {
			if p.child {
				_, exited := startSyncChild(t, w.imagesDir, p.images)
				children = append(children, exited)
				continue
			}
			syncs.Add(1)
			go func() {
				defer syncs.Done()
				_ = SyncImages(faultLog, w.imagesDir, p.images)
			}()
		}
		syncs.Wait()
		var childErr error
		for _, exited := range children {
			if err := <-exited; err != nil {
				childErr = err
			}
		}
		stop.Store(true)
		readers.Wait()
		if childErr != nil {
			t.Fatalf("a child sync failed (a panic, or a data race under -race): %v", childErr)
		}
		if s := readerErr.Load(); s != nil {
			t.Fatalf("during concurrent syncs: %s", *s)
		}

		owned, temps, _ := splitTree(w.imagesTree(), w.names)
		if len(temps) > 0 {
			t.Fatalf("concurrent syncs left temp entries: %v", slices.Sorted(maps.Keys(temps)))
		}
		if d := diffTrees(w.bystanders, w.outsideTree()); d != "" {
			t.Fatalf("entries no image owns changed: %s", d)
		}
		var tried []string
		for _, order := range permutations(n) {
			m := w.m.clone()
			for _, i := range order {
				for _, name := range slices.Sorted(maps.Keys(parts[i].rows)) {
					m.sync(parts[i].rows[name], nil)
				}
			}
			d := diffTrees(m.render(), owned)
			if d == "" {
				stats["serializable"]++
				return
			}
			tried = append(tried, fmt.Sprintf("order %v: %s", order, d))
		}
		t.Fatalf("the images dir after %d concurrent syncs matches no order of running them one at a time:\n%s", n, strings.Join(tried, "\n"))
	})
	reportSyncStats(t, stats)
}

// genRowsFixed is genRows for chosen builds with no faults.
func (w *faultWorld) genRowsFixed(step string, builds map[string][]build, pick map[string]int) (map[string]*model.Image, map[string]row, map[string]string) {
	images, rows, paths := map[string]*model.Image{}, map[string]row{}, map[string]string{}
	for name, idx := range pick {
		b := builds[name][idx]
		p := fmt.Sprintf("/%s/%s/%d.tar.gz", step, name, idx)
		w.srv.put(p, b.blob, "")
		images[name] = &model.Image{Name: name, SHA256: b.sum, URL: w.url + p}
		rows[name] = row{name: name, pinned: b.sum, url: true, servedSum: b.sum, candidates: b.candidates}
		paths[name] = p
	}
	return images, rows, paths
}

func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{nil}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for at := 0; at <= len(p); at++ {
			out = append(out, slices.Insert(slices.Clone(p), at, n-1))
		}
	}
	return out
}
