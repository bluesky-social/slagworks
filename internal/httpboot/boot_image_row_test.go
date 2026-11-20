package httpboot_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/httpboot"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/worker"
)

// These tests cover which image directory the boot scripts point at. The
// site snapshot's image rows are the authority for what the fleet boots
// (docs/user/images.md), and the worker syncs each row into
// <images_dir>/<row name>. SLAGWORKS_DEFAULT_IMAGE (HandlerConfig.DefaultImage)
// only picks among rows; it must not send a boot to a directory no row pins.

const imageRowMAC = "52:54:00:aa:bb:01"

var imageRowMachine = uuid.MustParse("11111111-1111-4111-8111-111111111111")

var imageFiles = []string{"vmlinuz", "initrd", "initrd-local", "root.squashfs"}

// imageTarball builds an image tarball whose files hold "<tag>:<file>".
func imageTarball(t *testing.T, tag string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "noble-pxe/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, f := range imageFiles {
		body := tag + ":" + f
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
	return buf.Bytes()
}

// imageRowCache returns a site cache holding one commissioned machine with an
// active op of opType and the given image rows. A non-empty imagesDir makes
// ApplySnapshot sync the rows to disk in the background, as a running site
// worker does.
func imageRowCache(t *testing.T, imagesDir string, opType model.OperationType, images []model.Image) *worker.Cache {
	t.Helper()
	// The cache and the image sync log through slog.Default.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	c := worker.NewCache(nil)
	c.ImagesDir = imagesDir
	c.ApplySnapshot(&model.SiteSnapshot{
		SeqNum: 1,
		Machines: []model.MachineSnapshot{{
			ID: imageRowMachine, State: model.MachineStateCommissioned, PXEEligible: true,
			SystemUUID: "4c4c4544-0042-3510-8051-b4c04f4a4433", KernelParams: "console=ttyS1,115200n8",
			Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: imageRowMAC}},
		}},
		Images: images,
	})
	c.ApplyOpResync(nil, nil)
	toState := model.MachineStateDeploying
	if opType == model.OpCommission {
		toState = model.MachineStateCommissioning
	}
	c.ApplyOpDelta(model.Operation{ID: uuid.New(), MachineID: imageRowMachine, Type: opType, Status: model.OpClaimed, Version: 1}, toState)
	return c
}

func imageRowServer(c *worker.Cache, imagesDir, defaultImage string, deployMode bool) func(target string) (int, string) {
	srv := httpboot.NewServer(":0", httpboot.ServerConfig{
		ImagesDir: imagesDir, DefaultImage: defaultImage, HTTPPort: 8080, DeployMode: deployMode, Provider: c,
	})
	srv.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := srv.Build()
	return func(target string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec.Code, rec.Body.String()
	}
}

var kernelLine = regexp.MustCompile(`(?m)^kernel (\S+)`)

// An operator rolls back by pinning a known-good tarball under a new row name
// (creating it as noble-pxe returns 409 while that row exists), then deletes
// the old noble-pxe row. The worker syncs the new row into its own directory
// and nothing prunes <images_dir>/noble-pxe. The deploy must boot the pinned
// rollback image, not the leftover directory that matches
// SLAGWORKS_DEFAULT_IMAGE.
func TestBootServesThePinnedRowNotTheLeftoverDefaultDir(t *testing.T) {
	imagesDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(imagesDir, "noble-pxe"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range imageFiles {
		if err := os.WriteFile(filepath.Join(imagesDir, "noble-pxe", f), []byte("v1:"+f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tarball := imageTarball(t, "v0")
	sum := sha256.Sum256(tarball)
	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tarball) }))
	defer bucket.Close()

	c := imageRowCache(t, imagesDir, model.OpDeploy, []model.Image{{
		Name: "noble-pxe-rollback", Version: "24.04", SHA256: hex.EncodeToString(sum[:]), URL: bucket.URL + "/noble-pxe/old.tar.gz",
	}})
	// The cache syncs images in the background.
	rollbackKernel := filepath.Join(imagesDir, "noble-pxe-rollback", "vmlinuz")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		b, err := os.ReadFile(rollbackKernel)
		if err == nil && string(b) == "v0:vmlinuz" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup: the worker did not sync the pinned row within 10s: %q %v", b, err)
		}
	}
	version := "noble-pxe-rollback@" + hex.EncodeToString(sum[:])[:16]
	get := imageRowServer(c, imagesDir, "noble-pxe", true)

	code, script := get("/boot/ipxe?mac=" + imageRowMAC)
	if code != http.StatusOK {
		t.Fatalf("GET /boot/ipxe = %d", code)
	}
	m := kernelLine.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("no kernel line; the only image row is servable, so the deploy should boot it:\n%s", script)
	}
	if !strings.Contains(script, "slagworks.image="+version+" ") {
		t.Errorf("cmdline does not name the pinned image, so the initrd and agent fetch from another directory:\n%s", script)
	}
	u, err := url.Parse(strings.ReplaceAll(m[1], "${next-server}", "worker"))
	if err != nil {
		t.Fatal(err)
	}
	code, kernel := get(u.RequestURI())
	if code != http.StatusOK || kernel != "v0:vmlinuz" {
		t.Fatalf("the deploy boots %s = %d %q; the only image row pins noble-pxe-rollback (v0:vmlinuz), and no row pins %s",
			u.Path, code, kernel, filepath.Dir(u.Path))
	}
}

var (
	kernelImageDir = regexp.MustCompile(`(?m)^kernel \S+/files/images/([^/\s]+)/vmlinuz `)
	initrdImageDir = regexp.MustCompile(`(?m)^initrd \S+/files/images/([^/\s]+)/initrd$`)
	cmdlineImage   = regexp.MustCompile(`slagworks\.image=(\S+)`)
)

// Every boot script that serves an image (deploy, commission, and the
// kernel-only script served outside deploy mode) resolves it from the image
// rows the same way.
func TestBootImageFollowsTheImageRows(t *testing.T) {
	row := func(name string) model.Image { return model.Image{Name: name, Version: "24.04"} }
	cases := []struct {
		name         string
		defaultImage string // SLAGWORKS_DEFAULT_IMAGE; "" means noble-pxe
		rows         []model.Image
		// want is the image directory the script must boot; "" means the
		// script must refuse and contain each of wantInError.
		want        string
		wantInError []string
		notInScript string
	}{
		{name: "a row named the default wins over another row", rows: []model.Image{row("noble-pxe"), row("noble-pxe-rollback")}, want: "noble-pxe"},
		{name: "the only row boots whatever its name", rows: []model.Image{row("noble-pxe-rollback")}, want: "noble-pxe-rollback"},
		{name: "no rows boots the default directory", want: "noble-pxe"},
		{name: "several rows none named the default refuses", rows: []model.Image{row("noble-a"), row("noble-b")},
			wantInError: []string{"named noble-pxe", "rows are noble-a, noble-b"}},
		{name: "an only row with an invalid name refuses", rows: []model.Image{row("Noble PXE")},
			wantInError: []string{"named noble-pxe", "rows are (invalid name, not synced)"}, notInScript: "Noble PXE"},
		{name: "a row with an invalid name refuses even when the default names it", defaultImage: "Noble PXE", rows: []model.Image{row("Noble PXE")},
			wantInError: []string{"rows are (invalid name, not synced)"}},
	}
	modes := []struct {
		name       string
		opType     model.OperationType
		deployMode bool
		banner     string // the script's echo line when it boots
	}{
		{"deploy", model.OpDeploy, true, "slagworks: deploy boot for mac"},
		{"commission", model.OpCommission, true, "slagworks: commission boot for mac"},
		{"kernel-only", model.OpDeploy, false, "slagworks: booting kernel for mac"},
	}
	for _, tc := range cases {
		for _, mode := range modes {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				defaultImage := tc.defaultImage
				if defaultImage == "" {
					defaultImage = "noble-pxe"
				}
				c := imageRowCache(t, "", mode.opType, tc.rows)
				code, script := imageRowServer(c, "", defaultImage, mode.deployMode)("/boot/ipxe?mac=" + imageRowMAC)
				if code != http.StatusOK {
					t.Fatalf("GET /boot/ipxe = %d", code)
				}
				if tc.notInScript != "" && strings.Contains(script, tc.notInScript) {
					t.Errorf("script echoes %q:\n%s", tc.notInScript, script)
				}
				if tc.want == "" {
					if kernelLine.MatchString(script) {
						t.Fatalf("script boots a kernel; want a refusal:\n%s", script)
					}
					for _, s := range append([]string{"ERROR: no image to boot"}, tc.wantInError...) {
						if !strings.Contains(script, s) {
							t.Errorf("refusal does not contain %q:\n%s", s, script)
						}
					}
					return
				}
				if !strings.Contains(script, mode.banner) {
					t.Fatalf("script is not the %s boot script:\n%s", mode.name, script)
				}
				for what, re := range map[string]*regexp.Regexp{"kernel URL": kernelImageDir, "initrd URL": initrdImageDir, "slagworks.image=": cmdlineImage} {
					m := re.FindStringSubmatch(script)
					if m == nil || m[1] != tc.want {
						t.Errorf("%s names image %v, want %s:\n%s", what, m, tc.want, script)
					}
				}
			})
		}
	}
}
