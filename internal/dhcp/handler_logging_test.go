package dhcp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/iana"
)

// captureLogs returns a slog.Logger that emits each record as a JSON
// line into the returned buffer. Handler.SetLogger consumes the
// logger; tests then parse the buffer into individual envelopes and
// assert per-key values.
func captureLogs(t *testing.T) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

// parseRecords splits the captured buffer into per-line JSON objects.
// Each object's keys mirror the slog attrs the handler stamped via
// log.With(...) plus log.Info(...).
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

// TestHandlePacket_StampsFrameFields pins the structured-fields
// requirement: every DHCP log envelope should carry frame_type,
// client_mac, and xid. Without this the operator can't tell from
// logs whether a reply came from us, which transaction it answers,
// or what kind of frame it was. Asserts the OFFER envelope's
// client_ip / boot_file / next_server / server_id / gateway too.
func TestHandlePacket_StampsFrameFields(t *testing.T) {
	logger, buf := captureLogs(t)
	h := NewHandler(testConfig())
	h.SetLogger(logger)

	mac, _ := net.ParseMAC("52:54:00:aa:bb:11")
	reply := h.HandlePacket(mustDiscover(t, mac, withArch(iana.EFI_X86_64), withPXEClient()))
	if reply == nil {
		t.Fatal("expected offer")
	}

	recs := parseRecords(t, buf)
	if len(recs) == 0 {
		t.Fatal("expected at least one log record")
	}
	// The "DHCP Offer" record is the one we care about; later
	// records may exist (none today, but the test shouldn't depend
	// on that).
	var offer map[string]any
	for _, r := range recs {
		if r["msg"] == "DHCP Offer" {
			offer = r
			break
		}
	}
	if offer == nil {
		t.Fatalf("no DHCP Offer record in: %v", recs)
	}

	want := map[string]string{
		"frame_type":  "DISCOVER",
		"client_mac":  "52:54:00:aa:bb:11",
		"client_ip":   "10.99.1.100",
		"boot_file":   "snponly.efi",
		"next_server": "10.99.1.1",
		"server_id":   "10.99.1.1",
		"gateway":     "10.99.1.1",
	}
	for k, v := range want {
		got, ok := offer[k].(string)
		if !ok {
			t.Errorf("offer.%s missing or not a string: %v", k, offer[k])
			continue
		}
		if got != v {
			t.Errorf("offer.%s = %q, want %q", k, got, v)
		}
	}
	if _, ok := offer["xid"]; !ok {
		t.Errorf("offer.xid missing")
	}
}

// TestHandlePacket_ResolvesMachineID pins that when the
// MachineIDForMAC callback resolves a MAC, every DHCP log envelope
// for that request carries `machine_id`. The slogkit handler hoists
// machine_id to envelope.MachineID, which is what makes per-machine
// log views (slagctl logs <host>) include DHCP traffic during PXE.
func TestHandlePacket_ResolvesMachineID(t *testing.T) {
	logger, buf := captureLogs(t)
	cfg := testConfig()
	cfg.MachineIDForMAC = func(m string) string {
		if m == "52:54:00:aa:bb:22" {
			return "11111111-1111-1111-1111-111111111111"
		}
		return ""
	}
	h := NewHandler(cfg)
	h.SetLogger(logger)

	mac, _ := net.ParseMAC("52:54:00:aa:bb:22")
	if reply := h.HandlePacket(mustDiscover(t, mac)); reply == nil {
		t.Fatal("expected offer")
	}

	recs := parseRecords(t, buf)
	for _, r := range recs {
		if id, ok := r["machine_id"].(string); !ok || id != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("record missing or wrong machine_id: %v", r)
		}
	}
}

// TestHandlePacket_UnknownMACLeavesEnvelopeUnscoped: the bug
// guarded against is "resolved-but-still-un-scoped"; an unresolved MAC is
// fine (and expected) for hosts not in the cache. machine_id should
// simply be absent in those envelopes — not a sentinel like "" or
// "unknown".
func TestHandlePacket_UnknownMACLeavesEnvelopeUnscoped(t *testing.T) {
	logger, buf := captureLogs(t)
	cfg := testConfig()
	cfg.MachineIDForMAC = func(m string) string { return "" }
	h := NewHandler(cfg)
	h.SetLogger(logger)

	mac, _ := net.ParseMAC("52:54:00:aa:bb:33")
	if reply := h.HandlePacket(mustDiscover(t, mac)); reply == nil {
		t.Fatal("expected offer")
	}

	for _, r := range parseRecords(t, buf) {
		if _, has := r["machine_id"]; has {
			t.Errorf("unresolved MAC must not stamp machine_id: %v", r)
		}
	}
}

// TestBootFile_NonNetbootRefusalLoggedAtInfo: a boot-eligible MAC refused a
// boot file for lacking a PXEClient/HTTPClient class identifier logs one
// Info line naming the class and hostname, so a nonconforming real PXE ROM
// is visible instead of silently never booting.
func TestBootFile_NonNetbootRefusalLoggedAtInfo(t *testing.T) {
	logger, buf := captureLogs(t)
	h := NewHandler(testConfig()) // nil BootEligible = eligible
	h.SetLogger(logger)

	mac, _ := net.ParseMAC("00:c0:b7:aa:bb:02")
	reply := h.HandlePacket(mustDiscover(t, mac,
		withVendorClass("APC"),
		dhcpv4.WithOption(dhcpv4.OptHostName("pdu-a"))))
	if reply == nil {
		t.Fatal("expected offer")
	}

	var refusals []map[string]any
	for _, r := range parseRecords(t, buf) {
		if msg, _ := r["msg"].(string); strings.Contains(msg, "not a netboot client") {
			refusals = append(refusals, r)
		}
	}
	if len(refusals) != 1 {
		t.Fatalf("want exactly one refusal record, got %d", len(refusals))
	}
	want := map[string]string{
		"level":      "INFO",
		"client_mac": "00:c0:b7:aa:bb:02",
		"class_id":   "APC",
		"hostname":   "pdu-a",
	}
	for k, v := range want {
		if got, _ := refusals[0][k].(string); got != v {
			t.Errorf("refusal.%s = %q, want %q", k, got, v)
		}
	}
}
