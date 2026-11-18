package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/power"
	"github.com/bluesky-social/slagworks/internal/power/smcipmitool"
)

// TestPowerControllerFactory_AMTRuntimeRoutesToAMT: an amt_runtime builds an
// AMTController against the envelope's host, whatever the snapshot says
// (here: a Supermicro manufacturer slug that would otherwise pick SMC).
func TestPowerControllerFactory_AMTRuntimeRoutesToAMT(t *testing.T) {
	// The controller's Open only fetches the digest challenge.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Digest realm="Digest:X", nonce="n", stale="false", qop="auth"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	m := &model.MachineSnapshot{ID: uuid.New(), BMCIP: "10.252.240.23", AMT: true, Manufacturer: "supermicro"}
	amtRT := &model.AMTRuntime{Host: strings.TrimPrefix(srv.URL, "https://"), Username: "admin", Password: "pw"}
	ctrl, err := newPowerControllerFactory(smcipmitool.Tool{}, nil)(context.Background(), m, model.BMCRuntime{}, nil, amtRT)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	defer ctrl.Close(context.Background())
	if _, ok := ctrl.(*power.AMTController); !ok {
		t.Fatalf("got %T, want *power.AMTController", ctrl)
	}
}

// TestPowerControllerFactory_AMTMachineRefusesBMCPath: an AMT machine with no
// amt_runtime must not get a bmclib/SMC controller pointed at its AMT
// endpoint (plan D2 backstop).
func TestPowerControllerFactory_AMTMachineRefusesBMCPath(t *testing.T) {
	m := &model.MachineSnapshot{ID: uuid.New(), BMCIP: "10.252.240.23", AMT: true, BMCUser: "admin", BMCPass: "pw"}
	ctrl, err := newPowerControllerFactory(smcipmitool.Tool{}, nil)(context.Background(), m, model.BMCRuntime{}, nil, nil)
	if err == nil || ctrl != nil {
		t.Fatalf("factory = (%T, %v), want refusal", ctrl, err)
	}
	if !strings.Contains(err.Error(), "Intel AMT") {
		t.Errorf("error %q should name the AMT refusal", err)
	}
}
