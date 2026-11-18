package netbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestSyncCommissionData_NeverWritesDeviceType pins the 2026-10-23 pop1
// leaf-3 regression: commissioning an SN2700 (DMI product "MSN2700") moved
// its NetBox device from the curated mellanox-sn2700 type onto the stale
// msn2700 type that FindOrCreateDeviceType resolved from the product name,
// so the network script stopped matching and deploy preflight failed.
// device_type is operator-owned topology (mirror.OnDeviceExternalDrift);
// the commission sync must leave it alone and never touch device types.
func TestSyncCommissionData_NeverWritesDeviceType(t *testing.T) {
	var (
		mu           sync.Mutex
		devicePatch  map[string]any
		typeRequests int
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/dcim/device-types/"),
			strings.HasPrefix(r.URL.Path, "/api/dcim/manufacturers/"):
			typeRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"count": 1, "results": []map[string]any{{"id": 69, "model": "MSN2700", "slug": "msn2700"}}})
		case r.URL.Path == "/api/dcim/devices/42/" && r.Method == http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &devicePatch)
			_, _ = w.Write([]byte(`{"id": 42}`))
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"count": 0, "results": []any{}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9000})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

	err := c.SyncCommissionData(context.Background(), CommissionSync{
		DeviceID:     42,
		ProductName:  "MSN2700",
		BoardVendor:  "Mellanox Technologies Ltd.",
		SystemSerial: "MT2005J02781",
		Hostname:     "leaf-3.pop1",
	})
	if err != nil {
		t.Fatalf("SyncCommissionData: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if devicePatch == nil {
		t.Fatal("expected a PATCH to /api/dcim/devices/42/")
	}
	if dt, ok := devicePatch["device_type"]; ok {
		t.Errorf("commission sync wrote device_type=%v; device_type is operator-owned and must not be written", dt)
	}
	if typeRequests != 0 {
		t.Errorf("commission sync made %d device-type/manufacturer requests; want 0", typeRequests)
	}
}
