package httpboot

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsonLogger returns a logger that emits each record as a JSON line
// into the buffer. Tests parse to assert per-key envelope fields.
func jsonLogger(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

func parseLogRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
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

// TestHTTPBoot_LogsFetchScopedToMachine pins that every served
// HTTP boot fetch produces an envelope with method, path, status,
// bytes, plus client_mac / machine_id / hostname when MachineForIP
// resolves the source IP. Without this the per-machine log view
// can't surface PXE-phase HTTP fetches.
func TestHTTPBoot_LogsFetchScopedToMachine(t *testing.T) {
	dir := t.TempDir()
	imagesDir := filepath.Join(dir, "images")
	if err := os.MkdirAll(filepath.Join(imagesDir, "noble"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imagesDir, "noble", "vmlinuz"), []byte("kernel"), 0644); err != nil {
		t.Fatal(err)
	}

	logger, buf := jsonLogger(t)
	srv := NewServer(":0", ServerConfig{
		ImagesDir: imagesDir,
		Provider:  newTestProvider(),
		MachineForIP: func(ip string) (mac, machineID, hostname string) {
			if ip == "127.0.0.1" {
				return "aa:bb:cc:dd:ee:ff", "11111111-1111-1111-1111-111111111111", "kworker-l34-1.pop1"
			}
			return "", "", ""
		},
	})
	srv.SetLogger(logger)

	ts := httptest.NewServer(srv.Build())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/files/images/noble/vmlinuz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var fetch map[string]any
	for _, r := range parseLogRecords(t, buf) {
		if r["msg"] == "httpboot fetch" {
			fetch = r
			break
		}
	}
	if fetch == nil {
		t.Fatalf("no 'httpboot fetch' envelope; got: %s", buf.String())
	}
	if fetch["path"] != "/files/images/noble/vmlinuz" {
		t.Errorf("path = %v", fetch["path"])
	}
	if fetch["method"] != "GET" {
		t.Errorf("method = %v", fetch["method"])
	}
	if status, ok := fetch["status"].(float64); !ok || int(status) != 200 {
		t.Errorf("status = %v", fetch["status"])
	}
	if fetch["client_mac"] != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("client_mac = %v", fetch["client_mac"])
	}
	if fetch["machine_id"] != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("machine_id = %v", fetch["machine_id"])
	}
	if fetch["hostname"] != "kworker-l34-1.pop1" {
		t.Errorf("hostname = %v", fetch["hostname"])
	}
}

// TestHTTPBoot_Logs404 pins 404 visibility: the
// catch-all 404 path produces a "httpboot fetch" envelope at WARN
// level (not buried at debug). Operators chasing a stuck PXE need to
// see what URL the firmware asked for and didn't get.
func TestHTTPBoot_Logs404(t *testing.T) {
	logger, buf := jsonLogger(t)
	srv := NewServer(":0", ServerConfig{
		Provider: newTestProvider(),
	})
	srv.SetLogger(logger)

	ts := httptest.NewServer(srv.Build())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/does/not/exist.efi")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var fetch map[string]any
	for _, r := range parseLogRecords(t, buf) {
		if r["msg"] == "httpboot fetch" && r["status"].(float64) == 404 {
			fetch = r
			break
		}
	}
	if fetch == nil {
		t.Fatalf("no warn-level 404 envelope: %s", buf.String())
	}
	if fetch["level"] != "WARN" {
		t.Errorf("404 envelope level = %v, want WARN", fetch["level"])
	}
	if fetch["path"] != "/does/not/exist.efi" {
		t.Errorf("path = %v", fetch["path"])
	}
}
