package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

// TestResolveMachineParam_RejectsHostnameWhenNetBoxOff verifies the
// 503 path: a hostname-shaped :id is unresolvable without NetBox
// integration, and we should fail loud rather than silently treating
// the hostname as a UUID and 400ing.
func TestResolveMachineParam_RejectsHostnameWhenNetBoxOff(t *testing.T) {
	s := &Server{}
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/machines/kworker-l34-1.pop1", nil)
	w := httptest.NewRecorder()
	c := e.NewContext(req, w)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: "kworker-l34-1.pop1"}})

	_, err := s.resolveMachineParam(c)
	if err == nil {
		t.Fatal("expected error when netbox is nil and a hostname was passed")
	}
	he, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("expected *echo.HTTPError, got %T: %v", err, err)
	}
	if he.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", he.Code)
	}
}

// TestResolveMachineParam_RejectsMalformedID verifies the 400 path:
// a non-UUID string with no dot is neither a UUID nor a plausible
// hostname; reject before even attempting NetBox.
func TestResolveMachineParam_RejectsMalformedID(t *testing.T) {
	s := &Server{}
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/machines/garbage", nil)
	w := httptest.NewRecorder()
	c := e.NewContext(req, w)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: "garbage"}})

	_, err := s.resolveMachineParam(c)
	if err == nil {
		t.Fatal("expected error for malformed id")
	}
	he, ok := err.(*echo.HTTPError)
	if !ok {
		t.Fatalf("expected *echo.HTTPError, got %T: %v", err, err)
	}
	if he.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", he.Code)
	}
}
