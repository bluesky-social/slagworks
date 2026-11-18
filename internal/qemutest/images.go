//go:build integration

package qemutest

import (
	"os"
	"testing"
)

// ImageDir is the directory the site worker serves over TFTP/HTTP.
// In CI it points at the canonical dir staged by the test-qemu.yaml
// workflow on the kvm runner (SLAGWORKS_IMAGES_DIR). For local
// runs, set the env var to a directory of canonical boot images.
type ImageDir struct {
	Path string
}

// UseCanonicalImages returns an ImageDir pointed at the per-run
// canonical directory (SLAGWORKS_IMAGES_DIR). The .github/workflows/
// test-qemu.yaml workflow stages this dir on the kvm runner before
// invoking `go test`; the env var points at it. (The workflow builds
// the image fresh from the worktree via build.sh, so the initrd here
// is a stock-init initrd carrying the worktree's /scripts/slagworks
// BOOT hook — there's no per-test repack.)
//
// For tests that need to mutate image state without affecting other
// parallel tests (rare — SiteWorker ApplySnapshot only writes when
// the snapshot declares images), add a separate helper that copies
// the canonical tree into t.TempDir().
//
// Fails the test loudly when the dir is missing — matching
// requireKVM's behavior in qemutest.go. Silent-skip used to be the
// behavior here, but it meant a runner with a broken image-sync
// timer would silently no-op the entire suite and CI would report
// green; that's strictly worse than failing.
func (e *Env) UseCanonicalImages(t *testing.T) *ImageDir {
	t.Helper()
	info, err := os.Stat(e.canonicalImagesDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("qemutest: canonical images dir %q unavailable: %v "+
			"(point SLAGWORKS_IMAGES_DIR at a dir of canonical boot images, "+
			"or run the suite via the test-qemu workflow on a kvm host)",
			e.canonicalImagesDir, err)
	}
	return &ImageDir{Path: e.canonicalImagesDir}
}
