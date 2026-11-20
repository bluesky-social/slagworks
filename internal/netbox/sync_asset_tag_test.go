package netbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestSyncCommissionData_RejectedAssetTagDegrades pins that NetBox refusing
// the device's DMI asset tag costs the commission that one field, not the
// whole write-back.
//
// NetBox requires a device's asset_tag to be unique across every device in
// every site, and at most 50 characters. The tag is firmware-provided DMI
// data: a vendor placeholder missing from the junk list (two units of one
// model then report the same string), an operator programming the same tag
// on two hosts, or a board moved between chassis all hand the second host a
// tag another device already holds. NetBox answers the device PATCH with a
// 400 naming asset_tag, and before the fix SyncCommissionData returned that
// error before writing anything else; the commission handler retried it
// until the operation expired, so the second host could never reach
// commissioned.
func TestSyncCommissionData_RejectedAssetTagDegrades(t *testing.T) {
	for _, tc := range []struct {
		name     string
		assetTag string
		// rejection is the 400 body NetBox returns for a PATCH that
		// carries a non-null asset_tag.
		rejection string
	}{
		{
			name:      "tag held by another device",
			assetTag:  "Chassis Asset Tag",
			rejection: `{"asset_tag":["device with this asset tag already exists."]}`,
		},
		{
			name:      "tag longer than NetBox allows",
			assetTag:  strings.Repeat("A", 51),
			rejection: `{"asset_tag":["Ensure this field has no more than 50 characters."]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nb := newAssetTagNetBox(t, tc.rejection)
			var logs bytes.Buffer
			nb.client.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))

			report, err := json.Marshal(map[string]any{"system": map[string]any{
				"serial_number": "S-B",
				"asset_tag":     tc.assetTag,
				"board_vendor":  "Vendor",
			}})
			if err != nil {
				t.Fatal(err)
			}
			s, err := CommissionToSync(7, report)
			if err != nil {
				t.Fatal(err)
			}
			s.SlagworksID = "00000000-0000-4000-8000-000000000007"

			if err := nb.client.SyncCommissionData(context.Background(), *s); err != nil {
				t.Fatalf("SyncCommissionData failed on an asset tag NetBox refuses: %v", err)
			}

			nb.mu.Lock()
			defer nb.mu.Unlock()
			if len(nb.patches) != 2 {
				t.Fatalf("device PATCHes = %d, want 2 (the refused one, then one without asset_tag)", len(nb.patches))
			}
			if got := nb.patches[0]["asset_tag"]; got != tc.assetTag {
				t.Errorf("first PATCH asset_tag = %v, want %q (the DMI tag is still offered first)", got, tc.assetTag)
			}
			landed := nb.patches[1]
			if at, ok := landed["asset_tag"]; ok {
				t.Errorf("retried PATCH carries asset_tag %v; it must omit the field so the device's current tag is left unchanged", at)
			}
			if landed["serial"] != "S-B" {
				t.Errorf("retried PATCH serial = %v, want S-B (the other device fields must still be written)", landed["serial"])
			}
			cf, _ := landed["custom_fields"].(map[string]any)
			if cf["board_vendor"] != "Vendor" || cf["slagworks_id"] != s.SlagworksID {
				t.Errorf("retried PATCH custom_fields = %v, want board_vendor and slagworks_id written", cf)
			}
			if _, ok := landed["tags"]; !ok {
				t.Error("retried PATCH omits tags; the untagged device must still gain slagworks-managed")
			}
			if !nb.interfacesListed {
				t.Error("interfaces were never reconciled; the sync must continue past the device PATCH")
			}

			var warned bool
			for line := range strings.Lines(logs.String()) {
				var rec map[string]any
				if json.Unmarshal([]byte(line), &rec) != nil {
					continue
				}
				if rec["level"] == "WARN" && rec["asset_tag"] == tc.assetTag &&
					rec["device_id"] == float64(7) && rec["machine_id"] == s.SlagworksID {
					warned = true
				}
			}
			if !warned {
				t.Errorf("no warning names device 7, its machine and the refused asset tag; logs:\n%s", logs.String())
			}
		})
	}
}

// TestSyncCommissionData_OtherDeviceRejectionStillFails pins the limit of
// the asset-tag degradation: a 400 that does not name asset_tag is returned
// as before, without a retry, so a genuine problem with the rest of the
// write still fails the sync.
func TestSyncCommissionData_OtherDeviceRejectionStillFails(t *testing.T) {
	nb := newAssetTagNetBox(t, "")
	nb.rejectAll = `{"serial":["Ensure this field has no more than 50 characters."]}`

	err := nb.client.SyncCommissionData(context.Background(), CommissionSync{
		DeviceID: 7, SystemSerial: "S-B", AssetTag: "TAG-1",
	})
	if err == nil || !strings.Contains(err.Error(), "serial") {
		t.Fatalf("SyncCommissionData error = %v, want the serial rejection returned", err)
	}
	nb.mu.Lock()
	defer nb.mu.Unlock()
	if len(nb.patches) != 1 {
		t.Errorf("device PATCHes = %d, want 1 (a rejection that does not name asset_tag is not retried)", len(nb.patches))
	}
}

// assetTagNetBox is a fake NetBox holding one untagged device (id 7) that
// refuses any device PATCH carrying a non-null asset_tag with rejection,
// and every device PATCH with rejectAll when that is set.
type assetTagNetBox struct {
	client *Client

	mu               sync.Mutex
	rejectAll        string
	patches          []map[string]any
	interfacesListed bool
}

func newAssetTagNetBox(t *testing.T, rejection string) *assetTagNetBox {
	t.Helper()
	nb := &assetTagNetBox{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nb.mu.Lock()
		defer nb.mu.Unlock()
		reject := func(body string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(body))
		}
		switch {
		case r.URL.Path == "/api/dcim/devices/7/" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"id":7,"name":"host-b","tags":[]}`))
		case r.URL.Path == "/api/dcim/devices/7/" && r.Method == http.MethodPatch:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("device PATCH body %q: %v", raw, err)
			}
			nb.patches = append(nb.patches, body)
			if nb.rejectAll != "" {
				reject(nb.rejectAll)
				return
			}
			if at, ok := body["asset_tag"]; ok && at != nil {
				reject(rejection)
				return
			}
			_, _ = w.Write([]byte(`{"id":7}`))
		case r.URL.Path == "/api/dcim/interfaces/" && r.Method == http.MethodGet:
			nb.interfacesListed = true
			_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	nb.client = NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})
	return nb
}
