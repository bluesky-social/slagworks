package netbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// TestSyncCommissionData_KeepsOperatorDeviceTags pins that the commission
// sync adds slagworks-managed to a device's tags without removing the
// operator's. NetBox treats `tags` on a device PATCH as the complete tag set
// (4.6 added add_tags; earlier versions only replace), so a PATCH carrying
// tags=[slagworks-managed] alone strips every other tag, and that used to
// happen on every commission and every retry of a commission report.
func TestSyncCommissionData_KeepsOperatorDeviceTags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing string   // tags JSON the device GET returns
		want     []string // tag slugs the device PATCH must carry; nil means the PATCH must omit tags
	}{
		{
			name:     "operator tag kept, managed tag added",
			existing: `[{"id":3,"name":"ansible-managed","slug":"ansible-managed"}]`,
			want:     []string{"ansible-managed", "slagworks-managed"},
		},
		{
			name:     "untagged device gets the managed tag",
			existing: `[]`,
			want:     []string{"slagworks-managed"},
		},
		{
			// A recommission must not rewrite the tag set at all: an
			// operator tag edit landing between the read and the write
			// would otherwise be lost.
			name:     "already managed, tags left alone",
			existing: `[{"id":3,"name":"ansible-managed","slug":"ansible-managed"},{"id":9,"name":"slagworks-managed","slug":"slagworks-managed"}]`,
			want:     nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu          sync.Mutex
				devicePatch map[string]json.RawMessage
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.URL.Path == "/api/dcim/devices/7/" && r.Method == http.MethodGet:
					_, _ = w.Write([]byte(`{"id":7,"name":"cpu1","tags":` + tc.existing + `}`))
				case r.URL.Path == "/api/dcim/devices/7/" && r.Method == http.MethodPatch:
					body, _ := io.ReadAll(r.Body)
					if err := json.Unmarshal(body, &devicePatch); err != nil {
						t.Errorf("device PATCH body %q: %v", body, err)
					}
					_, _ = w.Write([]byte(`{"id":7}`))
				case r.URL.Path == "/api/dcim/interfaces/" && r.Method == http.MethodGet:
					_, _ = w.Write([]byte(`{"count":0,"next":null,"results":[]}`))
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			defer srv.Close()
			c := NewClient(ClientConfig{BaseURL: srv.URL, Token: "t"})

			if err := c.SyncCommissionData(context.Background(), CommissionSync{DeviceID: 7, SystemSerial: "SN1"}); err != nil {
				t.Fatalf("SyncCommissionData: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if devicePatch == nil {
				t.Fatal("expected a PATCH to /api/dcim/devices/7/")
			}
			rawTags, wrote := devicePatch["tags"]
			if tc.want == nil {
				if wrote {
					t.Fatalf("PATCH tags = %s; the device already carries slagworks-managed, so the tag set must not be rewritten", rawTags)
				}
				return
			}
			if !wrote {
				t.Fatalf("PATCH omits tags; want %v", tc.want)
			}
			var tags []map[string]any
			if err := json.Unmarshal(rawTags, &tags); err != nil {
				t.Fatalf("PATCH tags %s: %v", rawTags, err)
			}
			var got []string
			for _, tg := range tags {
				s, _ := tg["slug"].(string)
				got = append(got, s)
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("PATCH tag slugs = %v, want %v (NetBox replaces the device's tag set with exactly this list)", got, tc.want)
			}
		})
	}
}
