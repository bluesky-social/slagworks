package httpboot

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
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/imagepull"
	"github.com/bluesky-social/slagworks/internal/model"
)

// swapTestTarball returns a gzipped image tarball whose four files each
// read "<version>:<file>", so a fetched body tells which version served it,
// and the tarball's SHA256.
func swapTestTarball(t *testing.T, version string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "noble-pxe/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"vmlinuz", "initrd", "initrd-local", "root.squashfs"} {
		body := version + ":" + f
		if err := tw.WriteHeader(&tar.Header{Name: "noble-pxe/" + f, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// TestImageSwap_OneBootFetchesOneImageVersion: a boot fetches image files
// from the site worker at three moments: iPXE fetches the kernel and initrd
// the boot script names, the BOOT hook fetches root.squashfs once the kernel
// is up, and on a UEFI deploy the agent's bootloader step fetches vmlinuz and
// initrd-local after partitioning and rsync, minutes later. The hook and the
// agent build their URLs from the script's slagworks.server= and
// slagworks.image=. The image is one artifact: the ESP gets the image's
// kernel and the deployed root gets /lib/modules from its squashfs, so a
// re-pin (slagctl image follow-latest) that lands while a boot is in flight
// must not hand that boot files from the new version. Before the fix the
// script named the image's directory, which SyncImages replaced in place,
// so every fetch after the re-pin returned the new version: a UEFI host got
// the new kernel on its ESP over the old kernel's modules.
func TestImageSwap_OneBootFetchesOneImageVersion(t *testing.T) {
	v1, v1sum := swapTestTarball(t, "v1")
	v2, v2sum := swapTestTarball(t, "v2")
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/noble-pxe/v1.tar.gz":
			_, _ = w.Write(v1)
		case "/noble-pxe/v2.tar.gz":
			_, _ = w.Write(v2)
		default:
			http.NotFound(w, r)
		}
	}))
	defer bucket.Close()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tc := range []struct {
		name       string
		deployMode bool
		commission bool
	}{
		{name: "deploy", deployMode: true},
		{name: "commission", deployMode: true, commission: true},
		{name: "kernel-only", deployMode: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			imagesDir := t.TempDir()
			pin := func(sum, path string) {
				t.Helper()
				if err := imagepull.SyncImages(quiet, imagesDir, map[string]*model.Image{
					"noble-pxe": {Name: "noble-pxe", Version: "24.04", SHA256: sum, URL: bucket.URL + path},
				}); err != nil {
					t.Fatalf("SyncImages: %v", err)
				}
			}

			p := newTestProviderKnownMAC()
			if tc.commission {
				id, _ := p.MachineIDForMAC(testMAC)
				p.SetCommissioning(id)
			}
			srv := NewServer(":0", ServerConfig{ImagesDir: imagesDir, DefaultImage: "noble-pxe", DeployMode: tc.deployMode, Provider: p})
			srv.log = quiet
			mux := srv.buildMux()

			// get fetches rawURL from the worker the way a target does: iPXE
			// expands ${next-server} and percent-encodes '@' and ':' in the
			// request path; the hook's busybox wget and the agent's wget send
			// the path as written, which the literal form covers.
			get := func(rawURL string, ipxe bool) string {
				t.Helper()
				u, err := url.Parse(strings.ReplaceAll(rawURL, "${next-server}", "worker"))
				if err != nil {
					t.Fatalf("parse %q: %v", rawURL, err)
				}
				target := u.RequestURI()
				if ipxe {
					target = strings.NewReplacer("@", "%40", ":", "%3A").Replace(u.EscapedPath())
				}
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("GET %s = %d", target, rec.Code)
				}
				return rec.Body.String()
			}
			type boot struct{ kernel, initrd, base string }
			bootScript := func() boot {
				t.Helper()
				script := get("/boot/ipxe?mac="+testMAC, false)
				kernel := regexp.MustCompile(`(?m)^kernel (\S+)`).FindStringSubmatch(script)
				initrd := regexp.MustCompile(`(?m)^initrd (\S+)`).FindStringSubmatch(script)
				image := regexp.MustCompile(`slagworks\.image=(\S+)`).FindStringSubmatch(script)
				server := regexp.MustCompile(`slagworks\.server=(\S+)`).FindStringSubmatch(script)
				if kernel == nil || initrd == nil || image == nil || server == nil {
					t.Fatalf("boot script lacks kernel, initrd, slagworks.image= or slagworks.server=:\n%s", script)
				}
				return boot{kernel: kernel[1], initrd: initrd[1], base: fmt.Sprintf("http://%s/files/images/%s", server[1], image[1])}
			}

			pin(v1sum, "/noble-pxe/v1.tar.gz")
			b := bootScript()
			got := map[string]string{
				"pxe vmlinuz": get(b.kernel, true),
				"pxe initrd":  get(b.initrd, true),
			}

			pin(v2sum, "/noble-pxe/v2.tar.gz") // the operator re-pins mid-boot

			got["hook root.squashfs"] = get(b.base+"/root.squashfs", false)
			got["bootloader vmlinuz"] = get(b.base+"/vmlinuz", false)
			got["bootloader initrd-local"] = get(b.base+"/initrd-local", false)
			for stage, body := range got {
				if !strings.HasPrefix(body, "v1:") {
					t.Errorf("%s returned %q; every file of a boot whose script was served at image v1 must come from v1 (got %v)", stage, body, got)
				}
			}

			// A boot that starts after the re-pin gets the new version.
			b = bootScript()
			for stage, u := range map[string]string{"pxe vmlinuz": b.kernel, "hook root.squashfs": b.base + "/root.squashfs"} {
				if body := get(u, false); !strings.HasPrefix(body, "v2:") {
					t.Errorf("after the re-pin, a new boot's %s returned %q, want v2", stage, body)
				}
			}
		})
	}
}
