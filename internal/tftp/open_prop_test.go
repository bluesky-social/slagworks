package tftp

// Property-based coverage for ReadHandler.Open, the file-resolution core of the
// TFTP server (and the model for "what a booting host may fetch").
//
// Oracle: an independent resolver over a known directory tree. A request is
// servable iff it is a relative name with no ".." element that names a
// regular file under the serve directory. Two properties:
//
//   - confinement: bytes returned always equal the bytes of the file the
//     oracle resolved inside the serve directory; a name that resolves
//     outside is never served.
//   - metamorphic: the answer does not depend on how the operator spelled
//     the serve directory (trailing slash, doubled slash, "./" prefix,
//     trailing "/."), because every spelling names the same directory.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

type tftpTree struct {
	root  string // relative to the package dir, no "./" prefix
	serve string
	files map[string]string // serve-relative slash path -> content
}

func buildTFTPTree(t *testing.T) tftpTree {
	t.Helper()
	root, err := os.MkdirTemp(".", "tftpprop-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	tr := tftpTree{root: root, serve: filepath.Join(root, "serve"), files: map[string]string{
		"snponly.efi":   "EFI-BINARY",
		"undionly.kpxe": "KPXE-BINARY",
		"sub/nested":    "NESTED",
		"a..b":          "DOTDOT-IN-NAME",
	}}
	for rel, content := range tr.files {
		p := filepath.Join(tr.serve, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Outside the serve dir: a sibling whose name shares the serve dir as a
	// string prefix, and a file in the parent.
	for p, content := range map[string]string{
		filepath.Join(root, "serve-secrets", "leak"): "SECRET-SIBLING",
		filepath.Join(root, "secret"):                "SECRET-PARENT",
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tr
}

// oracleResolve returns the content a request must yield, or ok=false when
// it must be refused. strict is false for the two spellings whose handling
// the contract leaves open (a leading slash; ".." inside a name), where
// either refusing or serving the confined file is acceptable.
func (tr tftpTree) oracleResolve(name string) (content string, ok, strict bool) {
	strict = true
	if name == "" {
		return "", false, true
	}
	if strings.HasPrefix(name, "/") {
		return "", false, true // absolute: never served
	}
	var parts []string
	for _, el := range strings.Split(name, "/") {
		switch el {
		case "", ".":
			continue
		case "..":
			return "", false, true
		}
		if strings.Contains(el, "..") {
			strict = false
		}
		parts = append(parts, el)
	}
	c, found := tr.files[strings.Join(parts, "/")]
	return c, found, strict
}

// TestProp_TFTPOpen_Confined uses only the canonical absolute spelling of
// the serve directory.
func TestProp_TFTPOpen_Confined(t *testing.T) {
	checkTFTPOpen(t, false)
}

// TestProp_TFTPOpen_ConfinedAndSpellingIndependent adds every other spelling
// of the same serve directory.
func TestProp_TFTPOpen_ConfinedAndSpellingIndependent(t *testing.T) {
	checkTFTPOpen(t, true)
}

func checkTFTPOpen(t *testing.T, allSpellings bool) {
	tr := buildTFTPTree(t)
	abs, err := filepath.Abs(tr.serve)
	if err != nil {
		t.Fatal(err)
	}
	// Every string here names the same directory.
	spellings := []string{abs}
	if allSpellings {
		spellings = append(spellings,
			abs+"/",
			abs+"//",
			abs+"/.",
			strings.Replace(abs, "/serve", "//serve", 1),
			tr.serve,
			"./"+tr.serve,
			tr.serve+"/",
		)
	}
	names := []string{
		"snponly.efi", "undionly.kpxe", "sub/nested", "a..b", "./snponly.efi", "sub//nested", "sub/./nested",
		"/snponly.efi", "missing.efi", "sub", "", ".", "..", "../secret", "../serve-secrets/leak",
		"sub/../snponly.efi", "sub/../../secret", "..snponly.efi", abs + "/snponly.efi",
		filepath.Join(filepath.Dir(abs), "secret"), "snponly.efi/",
		// No case variants: whether "SNPONLY.EFI" opens depends on the host
		// filesystem (case-insensitive on a macOS dev laptop).
	}
	var served, refused, outside int
	rapid.Check(t, func(t *rapid.T) {
		dir := rapid.SampledFrom(spellings).Draw(t, "serve_dir_spelling")
		name := rapid.SampledFrom(names).Draw(t, "filename")
		want, wantOK, strict := tr.oracleResolve(name)

		rc, err := NewReadHandler(dir).Open(name)
		var got string
		gotOK := false
		if err == nil {
			b, rerr := io.ReadAll(rc)
			_ = rc.Close()
			if rerr == nil {
				got, gotOK = string(b), true
			}
		}
		if gotOK && strings.HasPrefix(got, "SECRET") {
			outside++
			t.Fatalf("Open(%q) with serve dir %q returned a file from outside the serve dir: %q", name, dir, got)
		}
		if gotOK && (!wantOK || got != want) {
			t.Fatalf("Open(%q) with serve dir %q served %q; oracle says ok=%v content=%q", name, dir, got, wantOK, want)
		}
		if !gotOK && wantOK && strict {
			t.Fatalf("Open(%q) with serve dir %q refused (%v) a file that exists in the serve dir (want %q)", name, dir, err, want)
		}
		if gotOK {
			served++
		} else {
			refused++
		}
	})
	t.Logf("coverage: served=%d refused=%d outside=%d", served, refused, outside)
}
