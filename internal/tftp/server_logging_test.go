package tftp

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gotftp "github.com/pin/tftp/v3"
)

// fakeTransfer implements gotftp.OutgoingTransfer enough to give the
// readHandler a remote address. The pin/tftp v3 OutgoingTransfer
// surface includes more methods than we need; we provide only what
// readHandler reads.
type fakeTransfer struct {
	remote *net.UDPAddr
	io.Writer
}

func (f *fakeTransfer) RemoteAddr() net.UDPAddr { return *f.remote }
func (f *fakeTransfer) SetSize(int64)           {}
func (f *fakeTransfer) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(f.Writer, r)
}

// captureLogs builds a JSON-emitting slog logger so tests can parse
// per-record envelopes after the handler runs.
func captureLogs(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

func parseRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// TestReadHandler_LogsRequestServedScopedToMachine pins that a
// TFTP request from a known IP should produce two envelopes — a
// request entry and a served entry — each tagged with client_mac,
// machine_id, hostname so the per-machine log view picks them up.
func TestReadHandler_LogsRequestServedScopedToMachine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "snponly.efi"), []byte("ipxe-bytes"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	logger, buf := captureLogs(t)
	srv := NewServer(Config{
		BindAddr: ":0",
		ServeDir: dir,
		MachineForIP: func(ip string) (mac, machineID, hostname string) {
			if ip == "10.0.0.42" {
				return "aa:bb:cc:dd:ee:ff", "11111111-1111-1111-1111-111111111111", "kworker-test.pop1"
			}
			return "", "", ""
		},
		Logger: logger,
	})

	out := &bytes.Buffer{}
	rf := &fakeTransfer{
		remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.42"), Port: 69},
		Writer: out,
	}
	if err := srv.readHandler("snponly.efi", rf); err != nil {
		t.Fatalf("readHandler: %v", err)
	}
	if got := out.String(); got != "ipxe-bytes" {
		t.Errorf("served body = %q, want %q", got, "ipxe-bytes")
	}

	recs := parseRecords(t, buf)
	var req, served map[string]any
	for _, r := range recs {
		switch r["msg"] {
		case "TFTP request":
			req = r
		case "TFTP served":
			served = r
		}
	}
	if req == nil {
		t.Fatalf("no 'TFTP request' record: %v", recs)
	}
	if served == nil {
		t.Fatalf("no 'TFTP served' record: %v", recs)
	}
	for _, r := range []map[string]any{req, served} {
		if r["filename"] != "snponly.efi" {
			t.Errorf("filename = %v", r["filename"])
		}
		if r["client_ip"] != "10.0.0.42" {
			t.Errorf("client_ip = %v", r["client_ip"])
		}
		if r["client_mac"] != "aa:bb:cc:dd:ee:ff" {
			t.Errorf("client_mac = %v", r["client_mac"])
		}
		if r["machine_id"] != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("machine_id = %v", r["machine_id"])
		}
		if r["hostname"] != "kworker-test.pop1" {
			t.Errorf("hostname = %v", r["hostname"])
		}
	}
}

// TestReadHandler_LogsNotFound: missing file produces a "TFTP
// not-found" envelope (404-class events have to be visible
// in logs without --level debug).
func TestReadHandler_LogsNotFound(t *testing.T) {
	dir := t.TempDir() // empty
	logger, buf := captureLogs(t)
	srv := NewServer(Config{ServeDir: dir, Logger: logger})

	rf := &fakeTransfer{
		remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.99"), Port: 69},
		Writer: &bytes.Buffer{},
	}
	if err := srv.readHandler("missing.efi", rf); err == nil {
		t.Fatal("expected error for missing file")
	}

	var found bool
	for _, r := range parseRecords(t, buf) {
		if r["msg"] == "TFTP not-found" && r["filename"] == "missing.efi" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no TFTP not-found envelope produced for missing file")
	}
}

// TestReadHandler_UnresolvedIPLeavesEnvelopeUnscoped: when
// MachineForIP returns empty strings the envelope must NOT carry
// machine_id sentinels. Mirrors the DHCP side's contract.
func TestReadHandler_UnresolvedIPLeavesEnvelopeUnscoped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("y"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	logger, buf := captureLogs(t)
	srv := NewServer(Config{
		ServeDir:     dir,
		MachineForIP: func(ip string) (string, string, string) { return "", "", "" },
		Logger:       logger,
	})

	rf := &fakeTransfer{
		remote: &net.UDPAddr{IP: net.ParseIP("10.0.0.7"), Port: 69},
		Writer: &bytes.Buffer{},
	}
	if err := srv.readHandler("x", rf); err != nil {
		t.Fatalf("readHandler: %v", err)
	}
	for _, r := range parseRecords(t, buf) {
		if _, has := r["machine_id"]; has {
			t.Errorf("unresolved IP must not stamp machine_id: %v", r)
		}
	}
}

// Build-time assertion: fakeTransfer satisfies the OutgoingTransfer
// surface readHandler probes. If pin/tftp adds methods we'll catch
// it here rather than via test runtime.
var _ gotftp.OutgoingTransfer = (*fakeTransfer)(nil)
