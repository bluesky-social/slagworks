package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
)

// TestCommissionDeploy_RejectBadVia pins the transport-discriminator
// validation on the commission and deploy op-creation handlers: an invalid
// `via` must be rejected with 400 BEFORE any store/NetBox access, mirroring
// the power op. "" and "bmc" and "pdu" are the only accepted values. This is
// the guard that lets `slagctl machine {commission,deploy} --via pdu` route
// a BMC-less host (e.g. a leaf switch) to its PDU outlets.
func TestCommissionDeploy_RejectBadVia(t *testing.T) {
	// No store/netbox: the via-validation returns before either is touched,
	// so a bare Server with only an echo is enough to exercise the guard.
	s := &Server{echo: echo.New()}
	s.echo.POST("/api/v1/machines/:id/commission", s.commissionMachine)
	s.echo.POST("/api/v1/machines/:id/deploy", s.deployMachine)
	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	for _, path := range []string{"commission", "deploy"} {
		t.Run(path+" bad via -> 400", func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost,
				srv.URL+"/api/v1/machines/some-id/"+path,
				strings.NewReader(`{"via":"telepathy"}`))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s with via=telepathy: got status %d, want 400", path, resp.StatusCode)
			}
		})
	}
}

// TestValidVia pins the accepted transport set shared by deploy, commission,
// and power: "" (auto by credential kind), bmc, pdu, amt.
func TestValidVia(t *testing.T) {
	for _, v := range []string{"", "bmc", "pdu", "amt"} {
		if !validVia(v) {
			t.Errorf("validVia(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"telepathy", "AMT", "ipmi", "redfish", " amt"} {
		if validVia(v) {
			t.Errorf("validVia(%q) = true, want false", v)
		}
	}
}
