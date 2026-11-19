//go:build proptest_findings

package httpboot

// Fails on the code as it stands, hence the proptest_findings tag:
//
//	go test -tags proptest_findings ./internal/httpboot/ -run TestBootLocal_UEFI_ReachesADiskBeyondTheSecond
//
// Found by the QEMU storage property suite (internal/qemutest,
// TestStoragePropFindings/uefi-esp-third-disk): a machine whose boot disk is
// the third disk the firmware enumerates installs, boots once through the
// one-shot BootNext, and on every later boot loops in iPXE until the
// embedded script gives up:
//
//	slagworks: boot-local for mac 52:54:00:17:fd:74
//	Booting from SAN device 0x80
//	Boot from SAN device 0x80 failed: Error 0x3d222083
//	Booting from SAN device 0x81
//	Boot from SAN device 0x81 failed: Error 0x3d222083
//	Could not boot: Error 0x00000001
//	slagworks: PXE attempt 20
//	slagworks: PXE boot failed after 20 attempts; dropping to shell

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestBootLocal_UEFI_ReachesADiskBeyondTheSecond: the UEFI boot-local script
// is the only way a PXE-first machine reaches its installed system after the
// first boot, and a storage script may put the ESP on any disk. The script
// must therefore be able to boot a disk other than the firmware's first two.
// It passes when the script names drive 0x82 or walks the drives with a
// variable; it fails on a script that stops at 0x81.
func TestBootLocal_UEFI_ReachesADiskBeyondTheSecond(t *testing.T) {
	h := NewHandler(HandlerConfig{}, newTestProvider())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01&platform=efi", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()

	if !strings.Contains(body, "sanboot") {
		t.Fatalf("expected the UEFI sanboot boot-local script, got %q", body)
	}
	drives := regexp.MustCompile(`--drive (\S+)`).FindAllStringSubmatch(body, -1)
	for _, d := range drives {
		if d[1] == "0x82" || strings.HasPrefix(d[1], "${") {
			return
		}
	}
	var tried []string
	for _, d := range drives {
		tried = append(tried, d[1])
	}
	t.Fatalf("the UEFI boot-local script tries only drives %v and then fails the chain; "+
		"a machine whose ESP is on a later disk cannot boot after its first (BootNext) boot. Script:\n%s",
		tried, body)
}
