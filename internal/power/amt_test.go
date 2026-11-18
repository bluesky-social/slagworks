package power

import (
	"context"
	"crypto/md5" //nolint:gosec // AMT digest auth is MD5 by spec; the fake must verify it the same way
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake below implements the WS-Man surface the vendored iamt client
// drives against a real AMT endpoint: HTTP digest auth (qop=auth, MD5),
// Enumerate/Pull of CIM_AssociatedPowerManagementService and the EPR
// enumerations iamt resolves references through (CIM_ComputerSystem,
// CIM_BootConfigSetting, CIM_BootSourceSetting), Get/Put of
// AMT_BootSettingData, and the three Invoke methods (RequestPowerStateChange,
// SetBootConfigRole, ChangeBootOrder). It records every mutating call so the
// tests assert the exact AMT semantics (CIM power-state numbers, the
// one-time boot role, the Force PXE Boot source), not just "no error".

const (
	nsEnv   = "http://www.w3.org/2003/05/soap-envelope"
	nsWSA   = "http://schemas.xmlsoap.org/ws/2004/08/addressing"
	nsWSMan = "http://schemas.dmtf.org/wbem/wsman/1/wsman.xsd"
	nsEnum  = "http://schemas.xmlsoap.org/ws/2004/09/enumeration"
	nsXfer  = "http://schemas.xmlsoap.org/ws/2004/09/transfer"
	cimBase = "http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/"

	resAssocPMS   = cimBase + "CIM_AssociatedPowerManagementService"
	resPMS        = cimBase + "CIM_PowerManagementService"
	resCompSys    = cimBase + "CIM_ComputerSystem"
	resBootCfg    = cimBase + "CIM_BootConfigSetting"
	resBootSrc    = cimBase + "CIM_BootSourceSetting"
	resBootSvc    = cimBase + "CIM_BootService"
	resBootSDData = "http://intel.com/wbem/wscim/1/amt-schema/1/AMT_BootSettingData"

	fakeRealm = "Digest:F3EB554784E729164447A89F60B641C5"
	fakeNonce = "Y7mz6c5k2WmhC0hU7dcIg9GQq9y4C2nV"
)

type fakeAMT struct {
	t        *testing.T
	user     string
	pass     string
	mu       sync.Mutex
	power    int   // CIM PowerState
	avail    []int // AvailableRequestedPowerStates
	requests []int // RequestPowerStateChange PowerState params, in order
	roles    []string
	bootSrcs []string // ChangeBootOrder Source InstanceIDs
	puts     []map[string]string
	authFail int // 401s issued for a bad/missing digest after the first challenge
}

func newFakeAMT(t *testing.T, user, pass string, power int) (*fakeAMT, *httptest.Server) {
	t.Helper()
	f := &fakeAMT{t: t, user: user, pass: pass, power: power,
		avail: []int{2, 5, 8, 10}}
	srv := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // digest auth
	return hex.EncodeToString(sum[:])
}

// verifyDigest checks an Authorization header against RFC 2617 qop=auth,
// exactly as AMT does. Returns false (-> 401) on any mismatch.
func (f *fakeAMT) verifyDigest(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Digest ") {
		return false
	}
	params := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(h, "Digest "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		params[k] = strings.Trim(v, `"`)
	}
	if params["username"] != f.user || params["realm"] != fakeRealm || params["nonce"] != fakeNonce {
		return false
	}
	ha1 := md5hex(f.user + ":" + fakeRealm + ":" + f.pass)
	ha2 := md5hex(r.Method + ":" + params["uri"])
	want := md5hex(strings.Join([]string{ha1, params["nonce"], params["nc"], params["cnonce"], params["qop"], ha2}, ":"))
	return params["qop"] == "auth" && params["response"] == want
}

// soapReq is the flattened view of an inbound WS-Man request the fake
// routes on: the Action and ResourceURI headers plus every body leaf as
// (local-name -> text), and the Selector InstanceID values seen anywhere.
type soapReq struct {
	action, resource string
	leaves           map[string][]string
	instanceIDs      []string
	enumCtx          string
}

func parseSOAP(t *testing.T, body []byte) soapReq {
	t.Helper()
	req := soapReq{leaves: map[string][]string{}}
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	var stack []xml.StartElement
	inBody := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Errorf("fake amt: bad request xml: %v", err)
			return req
		}
		switch v := tok.(type) {
		case xml.StartElement:
			stack = append(stack, v)
			if v.Name.Local == "Body" && v.Name.Space == nsEnv {
				inBody = true
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 {
				continue
			}
			text := strings.TrimSpace(string(v))
			if text == "" {
				continue
			}
			el := stack[len(stack)-1]
			switch {
			case el.Name.Local == "Action" && el.Name.Space == nsWSA && !inBody:
				req.action = text
			case el.Name.Local == "ResourceURI" && !inBody && req.resource == "":
				req.resource = text
			case el.Name.Local == "EnumerationContext":
				req.enumCtx = text
			}
			if el.Name.Local == "Selector" {
				for _, a := range el.Attr {
					if a.Name.Local == "Name" && a.Value == "InstanceID" {
						req.instanceIDs = append(req.instanceIDs, text)
					}
				}
			}
			if inBody {
				req.leaves[el.Name.Local] = append(req.leaves[el.Name.Local], text)
			}
		}
	}
	return req
}

func envelope(action, body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` +
		`<a:Envelope xmlns:a="` + nsEnv + `" xmlns:b="` + nsWSA + `" xmlns:c="` + nsWSMan + `" xmlns:e="` + nsEnum + `">` +
		`<a:Header><b:To>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</b:To>` +
		`<b:Action a:mustUnderstand="true">` + action + `</b:Action>` +
		`<b:MessageID>uuid:00000000-8086-8086-8086-000000000001</b:MessageID></a:Header>` +
		`<a:Body>` + body + `</a:Body></a:Envelope>`
}

func epr(resource string, selectors ...[2]string) string {
	var sel strings.Builder
	for _, s := range selectors {
		fmt.Fprintf(&sel, `<c:Selector Name="%s">%s</c:Selector>`, s[0], s[1])
	}
	return `<b:EndpointReference><b:Address>http://schemas.xmlsoap.org/ws/2004/08/addressing/role/anonymous</b:Address>` +
		`<b:ReferenceParameters><c:ResourceURI>` + resource + `</c:ResourceURI><c:SelectorSet>` + sel.String() +
		`</c:SelectorSet></b:ReferenceParameters></b:EndpointReference>`
}

func (f *fakeAMT) items(resource string) string {
	switch resource {
	case resAssocPMS:
		var b strings.Builder
		fmt.Fprintf(&b, `<h:CIM_AssociatedPowerManagementService xmlns:h="%s"><h:PowerState>%d</h:PowerState>`, resAssocPMS, f.power)
		for _, s := range f.avail {
			fmt.Fprintf(&b, `<h:AvailableRequestedPowerStates>%d</h:AvailableRequestedPowerStates>`, s)
		}
		b.WriteString(`<h:RequestedPowerState>2</h:RequestedPowerState></h:CIM_AssociatedPowerManagementService>`)
		return b.String()
	case resCompSys:
		return epr(resCompSys, [2]string{"CreationClassName", "CIM_ComputerSystem"}, [2]string{"Name", "Intel(r) AMT"}) +
			epr(resCompSys, [2]string{"CreationClassName", "CIM_ComputerSystem"}, [2]string{"Name", "ManagedSystem"})
	case resBootCfg:
		return epr(resBootCfg, [2]string{"InstanceID", "Intel(r) AMT: Boot Configuration 0"})
	case resBootSrc:
		return epr(resBootSrc, [2]string{"InstanceID", "Intel(r) AMT: Force Hard-drive Boot"}) +
			epr(resBootSrc, [2]string{"InstanceID", "Intel(r) AMT: Force PXE Boot"})
	default:
		f.t.Errorf("fake amt: enumerate of unexpected resource %q", resource)
		return ""
	}
}

func (f *fakeAMT) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if !f.verifyDigest(r) {
		f.mu.Lock()
		if r.Header.Get("Authorization") != "" {
			f.authFail++
		}
		f.mu.Unlock()
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", nonce="%s", stale="false", qop="auth"`, fakeRealm, fakeNonce))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	req := parseSOAP(f.t, body)
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/soap+xml; charset=utf-8")
	switch {
	case req.action == "http://schemas.xmlsoap.org/ws/2004/09/enumeration/Enumerate":
		_, _ = io.WriteString(w, envelope(req.action+"Response",
			`<e:EnumerateResponse><e:EnumerationContext>`+req.resource+`</e:EnumerationContext></e:EnumerateResponse>`))
	case req.action == "http://schemas.xmlsoap.org/ws/2004/09/enumeration/Pull":
		_, _ = io.WriteString(w, envelope(req.action+"Response",
			`<e:PullResponse><e:Items>`+f.items(req.enumCtx)+`</e:Items><c:EndOfSequence></c:EndOfSequence></e:PullResponse>`))
	case req.action == nsXfer+"/Get" && req.resource == resBootSDData:
		_, _ = io.WriteString(w, envelope(req.action+"Response",
			`<g:AMT_BootSettingData xmlns:g="`+resBootSDData+`"><g:BIOSPause>true</g:BIOSPause><g:BIOSSetup>false</g:BIOSSetup>`+
				`<g:BootMediaIndex>3</g:BootMediaIndex><g:ElementName>Intel(r) AMT Boot Configuration Settings</g:ElementName>`+
				`<g:InstanceID>Intel(r) AMT:BootSettingData 0</g:InstanceID><g:OptionsCleared>true</g:OptionsCleared>`+
				`<g:UseSOL>false</g:UseSOL></g:AMT_BootSettingData>`))
	case req.action == nsXfer+"/Put" && req.resource == resBootSDData:
		put := map[string]string{}
		for k, v := range req.leaves {
			put[k] = v[0]
		}
		f.puts = append(f.puts, put)
		_, _ = io.WriteString(w, envelope(req.action+"Response", `<g:AMT_BootSettingData xmlns:g="`+resBootSDData+`"></g:AMT_BootSettingData>`))
	case req.action == resPMS+"/RequestPowerStateChange":
		ps, err := strconv.Atoi(firstLeaf(req, "PowerState"))
		if err != nil {
			f.t.Errorf("fake amt: RequestPowerStateChange without numeric PowerState: %v", req.leaves)
		}
		if !containsStr(req.leaves["Selector"], "ManagedSystem") {
			f.t.Errorf("fake amt: RequestPowerStateChange without the ManagedSystem ManagedElement reference: %v", req.leaves)
		}
		f.requests = append(f.requests, ps)
		switch ps {
		case 2:
			f.power = 2
		case 8, 6, 12, 13:
			f.power = 8
		}
		_, _ = io.WriteString(w, envelope(req.action+"Response",
			`<g:RequestPowerStateChange_OUTPUT xmlns:g="`+resPMS+`"><g:ReturnValue>0</g:ReturnValue></g:RequestPowerStateChange_OUTPUT>`))
	case req.action == resBootSvc+"/SetBootConfigRole":
		f.roles = append(f.roles, firstLeaf(req, "Role"))
		_, _ = io.WriteString(w, envelope(req.action+"Response",
			`<g:SetBootConfigRole_OUTPUT xmlns:g="`+resBootSvc+`"><g:ReturnValue>0</g:ReturnValue></g:SetBootConfigRole_OUTPUT>`))
	case req.action == resBootCfg+"/ChangeBootOrder":
		f.bootSrcs = append(f.bootSrcs, req.instanceIDs...)
		_, _ = io.WriteString(w, envelope(req.action+"Response",
			`<g:ChangeBootOrder_OUTPUT xmlns:g="`+resBootCfg+`"><g:ReturnValue>0</g:ReturnValue></g:ChangeBootOrder_OUTPUT>`))
	default:
		f.t.Errorf("fake amt: unhandled action %q resource %q", req.action, req.resource)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func firstLeaf(r soapReq, name string) string {
	if v := r.leaves[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func newTestAMT(t *testing.T, power int) (*fakeAMT, *AMTController) {
	t.Helper()
	f, srv := newFakeAMT(t, "admin", "s3cr3t!Pw", power)
	host := strings.TrimPrefix(srv.URL, "https://")
	ctrl, err := NewAMTController(context.Background(), host, "admin", "s3cr3t!Pw", 5*time.Second, nil)
	if err != nil {
		t.Fatalf("NewAMTController: %v", err)
	}
	return f, ctrl
}

func TestAMTController_PowerStatus(t *testing.T) {
	for _, tc := range []struct {
		cim  int
		want State
	}{{2, StateOn}, {8, StateOff}, {4, StateOff}} {
		_, ctrl := newTestAMT(t, tc.cim)
		got, err := ctrl.PowerStatus(context.Background())
		if err != nil {
			t.Fatalf("cim %d: PowerStatus: %v", tc.cim, err)
		}
		if got != tc.want {
			t.Errorf("cim %d: PowerStatus = %q, want %q", tc.cim, got, tc.want)
		}
	}
}

// TestAMTController_PowerOnOffCycle pins the CIM transitions iamt requests
// through RequestPowerStateChange: on = 2, off = 8 (soft off; the fake
// advertises no graceful states), cycle-from-on = 5 (power cycle off-soft).
func TestAMTController_PowerOnOffCycle(t *testing.T) {
	ctx := context.Background()

	f, ctrl := newTestAMT(t, 8)
	if err := ctrl.PowerOn(ctx); err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if err := ctrl.PowerOff(ctx); err != nil {
		t.Fatalf("PowerOff: %v", err)
	}
	f.mu.Lock()
	f.power = 2
	f.mu.Unlock()
	if err := ctrl.PowerCycle(ctx); err != nil {
		t.Fatalf("PowerCycle: %v", err)
	}
	want := []int{2, 8, 5}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fmt.Sprint(f.requests) != fmt.Sprint(want) {
		t.Errorf("RequestPowerStateChange sequence = %v, want %v", f.requests, want)
	}
	if ctrl.Transport() != "amt" {
		t.Errorf("Transport = %q, want amt", ctrl.Transport())
	}
}

// TestAMTController_SetBootDevicePXE pins the one-time Force PXE Boot: the
// boot settings are PUT with BIOSPause/BIOSSetup cleared, the boot config
// role is IsNextSingleUse (1, i.e. one-time, never persistent), and the
// boot order source is the AMT "Force PXE Boot" setting.
func TestAMTController_SetBootDevicePXE(t *testing.T) {
	f, ctrl := newTestAMT(t, 2)
	if err := ctrl.SetBootDevice(context.Background(), BootPXE); err != nil {
		t.Fatalf("SetBootDevice(pxe): %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.puts) != 1 {
		t.Fatalf("AMT_BootSettingData puts = %d, want 1", len(f.puts))
	}
	put := f.puts[0]
	if put["BIOSPause"] != "false" || put["BIOSSetup"] != "false" || put["BootMediaIndex"] != "0" {
		t.Errorf("boot settings put = %v, want BIOSPause=false BIOSSetup=false BootMediaIndex=0", put)
	}
	if _, ok := put["OptionsCleared"]; ok {
		t.Errorf("read-only OptionsCleared must not be written back: %v", put)
	}
	if fmt.Sprint(f.roles) != "[1]" {
		t.Errorf("SetBootConfigRole roles = %v, want [1] (IsNextSingleUse)", f.roles)
	}
	if !containsStr(f.bootSrcs, "Intel(r) AMT: Force PXE Boot") {
		t.Errorf("ChangeBootOrder sources = %v, want Force PXE Boot", f.bootSrcs)
	}
}

func TestAMTController_SetBootDeviceDefaultAndDisk(t *testing.T) {
	f, ctrl := newTestAMT(t, 2)
	if err := ctrl.SetBootDevice(context.Background(), BootDefault); err != nil {
		t.Fatalf("SetBootDevice(default) should be a no-op, got %v", err)
	}
	if err := ctrl.SetBootDevice(context.Background(), BootDisk); err == nil {
		t.Fatal("SetBootDevice(disk) should error over AMT")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.puts)+len(f.roles)+len(f.bootSrcs) != 0 {
		t.Errorf("default/disk must not touch AMT boot config: puts=%v roles=%v srcs=%v", f.puts, f.roles, f.bootSrcs)
	}
}

func TestAMTController_ChassisSerialUnsupported(t *testing.T) {
	_, ctrl := newTestAMT(t, 2)
	serial, err := ctrl.ChassisSerial(context.Background())
	if !errors.Is(err, ErrChassisSerialUnsupported) || serial != "" {
		t.Fatalf("ChassisSerial = (%q, %v), want (\"\", ErrChassisSerialUnsupported)", serial, err)
	}
}

// TestAMTController_WrongPasswordFails: a bad credential must surface as an
// error on the first call, never as a power state.
func TestAMTController_WrongPasswordFails(t *testing.T) {
	_, srv := newFakeAMT(t, "admin", "right", 2)
	host := strings.TrimPrefix(srv.URL, "https://")
	ctrl, err := NewAMTController(context.Background(), host, "admin", "wrong", 5*time.Second, nil)
	if err != nil {
		t.Fatalf("NewAMTController (open only fetches the challenge): %v", err)
	}
	if st, err := ctrl.PowerStatus(context.Background()); err == nil {
		t.Fatalf("PowerStatus with wrong password = %q, want error", st)
	}
}

func TestNewAMTController_RejectsMissingInputs(t *testing.T) {
	if _, err := NewAMTController(context.Background(), "", "admin", "pw", 0, nil); err == nil {
		t.Error("empty host: want error")
	}
	if _, err := NewAMTController(context.Background(), "10.0.0.1", "admin", "", 0, nil); err == nil {
		t.Error("empty password: want error")
	}
}
