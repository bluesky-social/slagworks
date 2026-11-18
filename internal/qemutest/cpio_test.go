//go:build integration

package qemutest

import (
	"bytes"
	"strings"
	"testing"
)

// TestWriteCpioNewc_StructuralSanity asserts the produced archive
// matches a few basic invariants of the newc cpio format. We don't
// have a Go cpio reader handy and don't want to take a dep just for
// this — but we can check the magic, the file content body, and the
// trailer name, which together prove the layout is at least
// structurally what the kernel will accept.
func TestWriteCpioNewc_StructuralSanity(t *testing.T) {
	body := []byte("ssh-ed25519 AAAA test@host\n")
	out := WriteCpioNewc("etc/slagworks/recovery_authorized_keys", 0o644, body)

	// First entry magic.
	if !bytes.HasPrefix(out, []byte("070701")) {
		t.Fatalf("missing newc magic 070701 at offset 0; got %q", out[:6])
	}

	// File body present.
	if !bytes.Contains(out, body) {
		t.Fatalf("file body not found in archive")
	}

	// Path string (without leading /) present.
	if !bytes.Contains(out, []byte("etc/slagworks/recovery_authorized_keys")) {
		t.Fatalf("path not found in archive")
	}

	// Trailer.
	if !bytes.Contains(out, []byte("TRAILER!!!")) {
		t.Fatalf("missing TRAILER!!! sentinel")
	}

	// Length should be 4-byte aligned (kernel readv expects this).
	if len(out)%4 != 0 {
		t.Fatalf("archive length %d not 4-byte aligned", len(out))
	}

	// Path normalization: leading slash stripped.
	out2 := WriteCpioNewc("/etc/foo", 0o644, []byte("x"))
	if bytes.Contains(out2, []byte{0, '/', 'e', 't', 'c'}) {
		t.Fatalf("leading slash not stripped from cpio name")
	}
	if !bytes.Contains(out2, []byte("etc/foo")) {
		t.Fatalf("expected etc/foo in archive: %s", strings.ReplaceAll(string(out2), "\x00", "."))
	}
}
