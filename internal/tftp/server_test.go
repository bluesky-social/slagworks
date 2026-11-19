package tftp

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	gotftp "github.com/pin/tftp/v3"
)

// mockTransfer implements io.ReaderFrom and gotftp.OutgoingTransfer for tests.
type mockTransfer struct {
	remoteAddr net.UDPAddr
	written    int64
}

func (m *mockTransfer) ReadFrom(r io.Reader) (int64, error) {
	n, err := io.Copy(io.Discard, r)
	m.written = n
	return n, err
}

func (m *mockTransfer) RemoteAddr() net.UDPAddr { return m.remoteAddr }
func (m *mockTransfer) SetSize(_ int64)         {}

// Verify mockTransfer satisfies the interface the server checks.
var _ gotftp.OutgoingTransfer = (*mockTransfer)(nil)

func setupServeDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "snponly.efi"), []byte("fake-efi-binary"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "undionly.kpxe"), []byte("fake-bios-binary"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadHandler_ServesFile(t *testing.T) {
	dir := setupServeDir(t)
	h := NewReadHandler(dir)

	rc, err := h.Open("snponly.efi")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "fake-efi-binary" {
		t.Fatalf("expected 'fake-efi-binary', got %q", string(data))
	}
}

// The serve directory comes from SLAGWORKS_TFTP_DIR (or qemutest-tftpd's
// -dir) verbatim. Every spelling of the same directory must serve the same
// files and reject the same escapes; a trailing slash, a "./" prefix or a
// relative path must not turn every request into "rejected path traversal".
func TestReadHandler_ServeDirSpelling(t *testing.T) {
	parent := t.TempDir()
	serve := filepath.Join(parent, "ipxe")
	if err := os.Mkdir(serve, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(serve, "snponly.efi"), []byte("EFI"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "outside"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		cwd  string
		dir  string
	}{
		{"absolute", parent, serve},
		{"absolute trailing slash", parent, serve + "/"},
		{"absolute double slash", parent, serve + "//"},
		{"absolute trailing dot", parent, serve + "/."},
		{"relative", parent, "ipxe"},
		{"relative dot-slash prefix", parent, "./ipxe"},
		{"relative trailing slash", parent, "ipxe/"},
		{"current directory", serve, "."},
		{"current directory trailing slash", serve, "./"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.cwd)
			h := NewReadHandler(tc.dir)

			rc, err := h.Open("snponly.efi")
			if err != nil {
				t.Fatalf("serve dir %q: Open(snponly.efi) = %v, want the file", tc.dir, err)
			}
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != "EFI" {
				t.Errorf("serve dir %q: got %q, want %q", tc.dir, b, "EFI")
			}

			for _, name := range []string{"../outside", ".", "./"} {
				if rc, err := h.Open(name); err == nil {
					rc.Close()
					t.Errorf("serve dir %q: Open(%q) succeeded, want rejection", tc.dir, name)
				}
			}
		})
	}
}

func TestReadHandler_FileNotFound(t *testing.T) {
	dir := setupServeDir(t)
	h := NewReadHandler(dir)

	_, err := h.Open("nonexistent.bin")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadHandler_RejectsPathTraversal(t *testing.T) {
	dir := setupServeDir(t)
	h := NewReadHandler(dir)

	cases := []string{
		"../etc/passwd",
		"../../etc/passwd",
		"subdir/../../../etc/passwd",
		"/etc/passwd",
		"/absolute/path",
	}

	for _, tc := range cases {
		_, err := h.Open(tc)
		if err == nil {
			t.Fatalf("expected error for path traversal %q, got nil", tc)
		}
	}
}

func TestReadHandler_RejectsEmptyFilename(t *testing.T) {
	dir := setupServeDir(t)
	h := NewReadHandler(dir)

	_, err := h.Open("")
	if err == nil {
		t.Fatal("expected error for empty filename")
	}
}

func TestServer_KnownIPs_AllowsRegisteredIP(t *testing.T) {
	dir := setupServeDir(t)
	allowedIP := "10.0.0.5"
	s := NewServer(Config{
		ServeDir: dir,
		KnownIPs: func(ip string) bool { return ip == allowedIP },
	})

	mt := &mockTransfer{remoteAddr: net.UDPAddr{IP: net.ParseIP(allowedIP), Port: 1234}}
	if err := s.readHandler("snponly.efi", mt); err != nil {
		t.Fatalf("expected allowed IP to succeed: %v", err)
	}
	if mt.written == 0 {
		t.Fatal("expected file data to be transferred")
	}
}

func TestServer_KnownIPs_DropsUnregisteredIP(t *testing.T) {
	dir := setupServeDir(t)
	s := NewServer(Config{
		ServeDir: dir,
		KnownIPs: func(ip string) bool { return false },
	})

	mt := &mockTransfer{remoteAddr: net.UDPAddr{IP: net.ParseIP("10.0.0.99"), Port: 1234}}
	err := s.readHandler("snponly.efi", mt)
	if err == nil {
		t.Fatal("expected error for unregistered IP")
	}
	if mt.written != 0 {
		t.Fatal("expected no data transferred for dropped request")
	}
}

func TestServer_KnownIPs_NilAllowsAll(t *testing.T) {
	dir := setupServeDir(t)
	s := NewServer(Config{
		ServeDir: dir,
		KnownIPs: nil, // promiscuous
	})

	mt := &mockTransfer{remoteAddr: net.UDPAddr{IP: net.ParseIP("10.0.0.99"), Port: 1234}}
	if err := s.readHandler("snponly.efi", mt); err != nil {
		t.Fatalf("expected nil KnownIPs to allow all: %v", err)
	}
}
