package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

// The client-config endpoint must answer without a token: slagctl reads it
// to learn where to sign in.
func TestClientConfig_UnauthenticatedReturnsIssuerAndClientID(t *testing.T) {
	s := &Server{
		echo:         echo.New(),
		oidcIssuer:   "https://idp.example.com/application/o/slagworks/",
		oidcAudience: "client-abc",
	}
	s.registerRoutes()

	rec := httptest.NewRecorder()
	s.echo.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/client-config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var got ClientConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := ClientConfig{OIDCIssuer: "https://idp.example.com/application/o/slagworks/", OIDCClientID: "client-abc"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
