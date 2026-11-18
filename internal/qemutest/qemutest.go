//go:build integration

// Package qemutest hosts the full-stack QEMU integration test suite:
// a per-test FakeHub (replaces slagworksd global), an in-process
// SiteWorker (real Cache + DHCP/TFTP/HTTP + wsconn), and a QEMU VM
// subprocess that actually PXE-boots.
//
// All of this is behind `//go:build integration` and expected to run
// on a KVM host (cpu3-pop3 today) via the test-qemu.yaml workflow,
// which stages canonical images under SLAGWORKS_IMAGES_DIR before
// invoking `go test`.
package qemutest

import (
	"os"
	"sync/atomic"
	"testing"
)

// defaultCanonicalImagesDir matches the cpu2 production layout. Tests
// override via SLAGWORKS_IMAGES_DIR (read in New, below).
const defaultCanonicalImagesDir = "/var/lib/slagworks/images"

// Env is the top-level test harness. One per test.
type Env struct {
	Hub        *FakeHub
	SubnetPool *subnetPool

	t                  *testing.T
	canonicalImagesDir string
	bridgeSeq          atomic.Int32
}

// New sets up a per-test environment: FakeHub started under an
// httptest.Server, subnet pool primed, cleanup registered. Fails the
// test loudly if KVM isn't available — these tests must run on a
// KVM host (cpu3-pop3 via the test-qemu.yaml workflow). Silent
// skipping previously hid CI environments where the suite was wired
// up but non-functional.
func New(t *testing.T) *Env {
	t.Helper()
	requireKVM(t)

	// SLAGWORKS_IMAGES_DIR is set by .github/workflows/test-qemu.yaml
	// to the per-run images directory staged on the kvm runner.
	// Production site workers use the same env var, so the name
	// stays aligned.
	canonical := os.Getenv("SLAGWORKS_IMAGES_DIR")
	if canonical == "" {
		canonical = defaultCanonicalImagesDir
	}

	env := &Env{
		SubnetPool:         getPool(),
		t:                  t,
		canonicalImagesDir: canonical,
	}
	env.Hub = newFakeHub(env)
	return env
}

func requireKVM(t *testing.T) {
	t.Helper()
	info, err := os.Stat("/dev/kvm")
	if err != nil {
		t.Fatalf("qemutest: /dev/kvm unavailable (%v); this suite must run on a KVM host (cpu3-pop3)", err)
	}
	if info.Mode()&0660 == 0 {
		t.Fatalf("qemutest: /dev/kvm not accessible (mode %v); this suite must run on a KVM host", info.Mode())
	}
}
