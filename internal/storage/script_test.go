package storage

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustParseScript(t *testing.T, content string) *Script {
	t.Helper()
	s, err := ParseScript("test-script", []byte(content))
	if err != nil {
		t.Fatalf("ParseScript: %v", err)
	}
	return s
}

func commissionJSON(manufacturer, product string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"system": map[string]any{
			"manufacturer": manufacturer,
			"product_name": product,
		},
	})
	return b
}

// TestParseScript_KernelParamsRejected pins that the
// slagworks:kernel_params header is no longer accepted on storage
// scripts. Per-hardware kernel params come from the NetBox
// device_type custom field. This rejection is storage-specific (it
// wraps the shared configscript grammar).
func TestParseScript_KernelParamsRejected(t *testing.T) {
	_, err := ParseScript("bad", []byte(`#!/usr/bin/env bash
# slagworks:match system_product=RS501A
# slagworks:kernel_params console=ttyS1,115200n8
# slagworks:priority 100
echo hello
`))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "kernel_params") || !strings.Contains(err.Error(), "no longer accepted") {
		t.Errorf("error should say kernel_params is no longer accepted, got: %v", err)
	}
}

// TestParseScript_DMIFields confirms storage's field vocabulary is wired
// through to the shared engine: the DMI fields parse, a foreign field is
// rejected.
func TestParseScript_DMIFields(t *testing.T) {
	s := mustParseScript(t, "#!/usr/bin/env bash\n# slagworks:match system_manufacturer=/ASUSTeK.*/\n# slagworks:match system_product=RS501A\n")
	if len(s.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(s.Rules))
	}
	_, err := ParseScript("bad", []byte("#!/usr/bin/env bash\n# slagworks:match site=pop1\n"))
	if err == nil || !strings.Contains(err.Error(), "site") {
		t.Errorf("expected rejection naming the network-only field 'site', got: %v", err)
	}
}

// --- Match (commission-data resolver) ---

func TestMatch_NoScripts(t *testing.T) {
	got, err := Match(nil, commissionJSON("ASUS", "RS501A"), nil)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got != nil {
		t.Error("expected nil with no scripts")
	}
}

func TestMatch_ExactHit(t *testing.T) {
	s := mustParseScript(t, "#!/usr/bin/env bash\n# slagworks:match system_product=RS501A-E12-RS4U\n# slagworks:priority 100\n")
	got, err := Match([]Script{*s}, commissionJSON("ASUSTeK", "RS501A-E12-RS4U"), nil)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got == nil {
		t.Fatal("expected match, got nil")
	}
}

func TestMatch_ExactMiss(t *testing.T) {
	s := mustParseScript(t, "#!/usr/bin/env bash\n# slagworks:match system_product=RS501A-E12-RS4U\n# slagworks:priority 100\n")
	got, err := Match([]Script{*s}, commissionJSON("ASUSTeK", "DIFFERENT-MODEL"), nil)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got != nil {
		t.Error("expected no match for different product")
	}
}

func TestMatch_RegexHit(t *testing.T) {
	s := mustParseScript(t, "#!/usr/bin/env bash\n# slagworks:match system_manufacturer=/ASUSTeK.*/\n# slagworks:priority 10\n")
	got, err := Match([]Script{*s}, commissionJSON("ASUSTeK Computer INC.", "RS501A"), nil)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if got == nil {
		t.Fatal("expected regex match")
	}
}

func TestMatch_InvalidCommissionData(t *testing.T) {
	s := mustParseScript(t, "#!/usr/bin/env bash\n# slagworks:match system_product=X\n")
	_, err := Match([]Script{*s}, json.RawMessage("not-json"), nil)
	if err == nil || !strings.Contains(err.Error(), "parse commission data") {
		t.Fatalf("expected commission-data parse error, got: %v", err)
	}
}
