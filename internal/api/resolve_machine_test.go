package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/beyond/tokenverify"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
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

// TestResolveByName_AmbiguousNameIsRefused: NetBox enforces device-name
// uniqueness per site, not globally, so pop-a and pop-b can each hold a
// device named node-1.rack. A request that names a device must then be
// refused with 409 listing the candidates, as resolveDevice already does
// for a serial shared by several devices, not applied to whichever device
// NetBox lists first. A name only one device carries still resolves.
func TestResolveByName_AmbiguousNameIsRefused(t *testing.T) {
	st := store.New(testpg.DB(t, store.Migrate))
	popA := &netbox.NestedRef{ID: 1, Name: "pop-a", Slug: "pop-a"}
	popB := &netbox.NestedRef{ID: 2, Name: "pop-b", Slug: "pop-b"}
	devs := []netbox.Device{
		{ID: 201, Name: "node-1.rack", Serial: "SN-A", Site: popA},
		{ID: 202, Name: "node-1.rack", Serial: "SN-B", Site: popB},
		{ID: 203, Name: "node-2.rack", Serial: "SN-C", Site: popA},
	}
	f := &netboxFixture{devices: devs, deviceByID: map[int]netbox.Device{}}
	for _, d := range devs {
		f.deviceByID[d.ID] = d
	}
	nb := netbox.NewClient(netbox.ClientConfig{BaseURL: f.server(t).URL, Token: "t"})
	bmc := func(pw string) awssecrets.AdminHost {
		return awssecrets.AdminHost{Kind: awssecrets.KindBMC, Username: "admin", Password: pw}
	}
	s := NewServer(Config{
		Store:          st,
		NetBox:         nb,
		RequiredGroups: []string{"platform"},
		BMCSecrets: &awssecrets.FakeReader{Secrets: map[string]*awssecrets.AdminSecret{
			"pop-a": {BySerial: map[string]awssecrets.AdminHost{"SN-A": bmc("pw-a"), "SN-C": bmc("pw-c")}},
			"pop-b": {BySerial: map[string]awssecrets.AdminHost{"SN-B": bmc("pw-b")}},
		}},
		Verifier: &fakeVerifier{claims: map[string]*tokenverify.Claims{
			"op-tok": {Subject: "op", Groups: []string{"platform"}},
		}},
	})
	s.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	machineOf := map[int]uuid.UUID{}
	for _, d := range devs {
		id, devID := uuid.New(), d.ID
		machineOf[d.ID] = id
		if err := st.CreateMachine(&model.Machine{ID: id, NetBoxSerial: d.Serial, NetBoxDeviceID: &devID, State: model.MachineStateRunning}); err != nil {
			t.Fatal(err)
		}
	}
	do := func(method, path string) (int, string) {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer op-tok")
		w := httptest.NewRecorder()
		s.Echo().ServeHTTP(w, req)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	locked := func(devID int) bool {
		t.Helper()
		m, err := st.GetMachine(machineOf[devID])
		if err != nil {
			t.Fatal(err)
		}
		return m.Locked
	}
	// The 409 must name every candidate so the operator can pick one.
	assertCandidates := func(t *testing.T, body string) {
		t.Helper()
		for _, want := range []string{"node-1.rack", "201", "pop-a", "202", "pop-b"} {
			if !strings.Contains(body, want) {
				t.Errorf("409 body does not mention %q: %s", want, body)
			}
		}
	}

	t.Run("machine route", func(t *testing.T) {
		code, body := do(http.MethodPost, "/api/v1/machines/node-1.rack/lock")
		if code != http.StatusConflict {
			t.Fatalf("POST /machines/node-1.rack/lock = %d %s; two devices have that name, want 409", code, body)
		}
		assertCandidates(t, body)
		for _, devID := range []int{201, 202} {
			if locked(devID) {
				t.Errorf("the refused request locked the machine of NetBox device %d", devID)
			}
		}
	})

	t.Run("device creds", func(t *testing.T) {
		code, body := do(http.MethodGet, "/api/v1/devices/node-1.rack/creds")
		if code != http.StatusConflict {
			t.Fatalf("GET /devices/node-1.rack/creds = %d %s; two devices have that name, want 409", code, body)
		}
		assertCandidates(t, body)
		for _, pw := range []string{"pw-a", "pw-b"} {
			if strings.Contains(body, pw) {
				t.Errorf("the refused request returned credential %q: %s", pw, body)
			}
		}
	})

	t.Run("unique name still resolves", func(t *testing.T) {
		if code, body := do(http.MethodPost, "/api/v1/machines/node-2.rack/lock"); code != http.StatusOK {
			t.Fatalf("POST /machines/node-2.rack/lock = %d %s, want 200", code, body)
		}
		if !locked(203) {
			t.Error("lock by a unique name did not lock the machine of NetBox device 203")
		}
		if code, body := do(http.MethodGet, "/api/v1/devices/node-2.rack/creds"); code != http.StatusOK || !strings.Contains(body, "pw-c") {
			t.Fatalf("GET /devices/node-2.rack/creds = %d %s, want 200 with pw-c", code, body)
		}
	})
}
