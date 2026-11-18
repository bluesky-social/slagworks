package netbox

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// TestFindOrCreateDeviceType_MatchesByModelNotSlug pins the lookup key:
// NetBox enforces uniqueness on (manufacturer, model), so the find must
// query by model. The pre-existing row here carries a curated slug that
// differs from slugify(model) — exactly the shape of operator-created
// device types (e.g. "supermicro-as-1115hs-tnr" for model
// "AS-1115HS-TNR" under a lookup that would slugify to
// "as-1115hs-tnr"). A slug-keyed find misses, the POST 400s on the
// model uniqueness constraint, and the conflict-recovery re-runs the
// same missing slug query — wedging every commission of that hardware
// model (observed live on kworker-l56-2, 2026-06-11).
func TestFindOrCreateDeviceType_MatchesByModelNotSlug(t *testing.T) {
	posts := 0
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/dcim/device-types/" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			if got := r.URL.Query().Get("model"); got != "AS-1115HS-TNR" {
				t.Errorf("expected model=AS-1115HS-TNR in query, got %v", r.URL.Query())
				json.NewEncoder(w).Encode(ListResponse[DeviceType]{Count: 0})
				return
			}
			json.NewEncoder(w).Encode(ListResponse[DeviceType]{
				Count:   1,
				Results: []DeviceType{{ID: 40, Model: "AS-1115HS-TNR", Slug: "supermicro-as-1115hs-tnr"}},
			})
		case http.MethodPost:
			posts++
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"__all__":["Device type with this Manufacturer and Model already exists."]}`))
		}
	}))

	id, err := c.FindOrCreateDeviceType(context.Background(), 12, "AS-1115HS-TNR")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 40 {
		t.Errorf("id = %d, want 40 (the existing curated row)", id)
	}
	if posts != 0 {
		t.Errorf("expected find to hit, got %d POST attempts", posts)
	}
}

// TestFindOrCreateManufacturer_MatchesByName pins the same rule for
// manufacturers: the find queries by name (the colliding field), not
// by computed slug.
func TestFindOrCreateManufacturer_MatchesByName(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s (find should hit, no create)", r.Method)
		}
		if got := r.URL.Query().Get("name"); got != "Supermicro" {
			t.Errorf("expected name=Supermicro in query, got %v", r.URL.Query())
		}
		json.NewEncoder(w).Encode(ListResponse[Manufacturer]{
			Count:   1,
			Results: []Manufacturer{{ID: 12, Name: "Supermicro", Slug: "smc"}},
		})
	}))

	id, err := c.FindOrCreateManufacturer(context.Background(), "Supermicro")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 12 {
		t.Errorf("id = %d, want 12", id)
	}
}

// TestFindOrCreateDeviceType_CreatesWhenMissing verifies the create
// path still works when no row matches the model.
func TestFindOrCreateDeviceType_CreatesWhenMissing(t *testing.T) {
	var created map[string]any
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(ListResponse[DeviceType]{Count: 0})
		case http.MethodPost:
			json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(DeviceType{ID: 77})
		}
	}))

	id, err := c.FindOrCreateDeviceType(context.Background(), 12, "Brand New Model")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != 77 {
		t.Errorf("id = %d, want 77", id)
	}
	if created["model"] != "Brand New Model" || created["slug"] != "brand-new-model" {
		t.Errorf("create body = %v", created)
	}
}
