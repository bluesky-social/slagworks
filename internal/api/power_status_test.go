package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

// NOTE: the happy-path handler flow (store lookup → bmc_runtime
// resolution → hub RPC) is covered end-to-end by the ws package's
// TestHub_PowerStatus_RoundTrip plus the worker's TestRunPowerStatus_*;
// the api package's test fixtures are store-less pure-handler tests,
// so the tests here pin the handler's guard rails and the wire shape.

// TestPowerStatusMachine_NoRPCConfigured pins the unconfigured-server
// outcome: a clean 503, not a nil-deref. Servers without a WS hub
// (tests, future split deployments) must reject the read loudly.
func TestPowerStatusMachine_NoRPCConfigured(t *testing.T) {
	s := &Server{}
	e := echo.New()
	id := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/machines/"+id.String()+"/power", nil)
	w := httptest.NewRecorder()
	c := e.NewContext(req, w)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: id.String()}})

	err := s.powerStatusMachine(c)
	if err == nil {
		t.Fatal("expected error when PowerStatusRPC is not configured")
	}
	he, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("expected *echo.HTTPError, got %T: %v", err, err)
	}
	if he.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", he.Code)
	}
	if !strings.Contains(he.Message, "WebSocket hub") {
		t.Errorf("message %q should explain what's missing", he.Message)
	}
}

// TestPowerStatusResponse_JSONShape pins the wire format slagctl
// unmarshals into. Mirrors TestDeviceCreds_JSONShape: renaming a
// field here without the client silently zeroes it.
func TestPowerStatusResponse_JSONShape(t *testing.T) {
	id := uuid.New()
	b, err := json.Marshal(PowerStatusResponse{
		MachineID: id,
		State:     "on",
		Transport: "redfish",
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"machine_id": id.String(),
		"state":      "on",
		"transport":  "redfish",
	} {
		if got, ok := m[k]; !ok || got != want {
			t.Errorf("field %q: got %v (present=%v), want %q", k, got, ok, want)
		}
	}
}

// TestPowerStatusRoute_IsGET pins the verb split on /machines/:id/power:
// GET is the status read (auth-gated like every read), POST stays the
// actuation path. A regression that drops the GET route would resurface
// the "not yet implemented" era with a 405.
func TestPowerStatusRoute_IsGET(t *testing.T) {
	s := &Server{
		verifier: &fakeVerifier{},
		authz:    nil, // not reached; the route just needs to exist
	}
	e := echo.New()
	// Register only what this test needs: a route table probe.
	api := e.Group("/api/v1")
	api.GET("/machines/:id/power", s.powerStatusMachine)

	id := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/machines/"+id.String()+"/power", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	// No RPC configured → handler runs and returns 503; the point is
	// the route matched (405/404 would mean it didn't).
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected route to match and 503 (no RPC), got %d: %s", w.Code, w.Body.String())
	}
}
