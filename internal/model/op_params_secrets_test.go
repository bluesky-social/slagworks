package model

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// credentialName is what a credential-looking field name matches. A field
// reachable from OpParams whose Go or JSON name matches must carry
// secret:"true".
var credentialName = regexp.MustCompile(`(?i)password|secret|key|token`)

// unmarkedCredentialFields walks every struct type reachable from t (through
// pointers, slices, arrays and maps) and returns "Type.Field" for each
// exported, serialized field whose name looks like a credential but is not
// marked secret:"true".
func unmarkedCredentialFields(t reflect.Type) []string {
	var out []string
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for {
			switch t.Kind() {
			case reflect.Pointer, reflect.Slice, reflect.Array:
				t = t.Elem()
				continue
			case reflect.Map:
				walk(t.Key())
				t = t.Elem()
				continue
			default:
			}
			break
		}
		if t.Kind() != reflect.Struct || seen[t] {
			return
		}
		seen[t] = true
		for i := range t.NumField() {
			f := t.Field(i)
			jsonName, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || jsonName == "-" {
				continue
			}
			if (credentialName.MatchString(f.Name) || credentialName.MatchString(jsonName)) && f.Tag.Get("secret") != "true" {
				out = append(out, t.Name()+"."+f.Name)
			}
			walk(f.Type)
		}
	}
	walk(t)
	return out
}

// TestOpParamsCredentialFieldsAreMarked is the guard that keeps the
// redaction and the stored-credential scrub from silently rotting: a new
// credential-looking field anywhere under OpParams fails here until it is
// marked secret:"true" (or renamed, if it is not a credential).
func TestOpParamsCredentialFieldsAreMarked(t *testing.T) {
	if got := unmarkedCredentialFields(reflect.TypeFor[OpParams]()); len(got) > 0 {
		t.Fatalf("credential-looking fields under OpParams lack secret:\"true\": %v; mark them so API responses redact them and terminal ops drop them", got)
	}
}

// TestUnmarkedCredentialFields_CatchesNewField proves the guard is not
// vacuous: it reports an unmarked credential under a pointer, a slice, and a
// map value, and accepts a marked one.
func TestUnmarkedCredentialFields_CatchesNewField(t *testing.T) {
	type leaf struct {
		APIToken string `json:"api_token"`
		Marked   string `json:"client_secret" secret:"true"`
		Host     string `json:"host"`
	}
	type root struct {
		Ptr   *leaf           `json:"ptr"`
		Slice []leaf          `json:"slice"`
		Map   map[string]leaf `json:"map"`
		Skip  string          `json:"-"`
	}
	got := unmarkedCredentialFields(reflect.TypeFor[root]())
	if !slices.Equal(got, []string{"leaf.APIToken"}) {
		t.Fatalf("unmarked = %v, want [leaf.APIToken]", got)
	}
}

// TestSecretParamPaths_Pinned pins the full set of credential locations. The
// stored-credential scrub's SQL is generated from this list, so a change
// here is a change to what the operations table keeps.
func TestSecretParamPaths_Pinned(t *testing.T) {
	var got []string
	for _, p := range SecretParamPaths() {
		got = append(got, strings.Join(p, "."))
	}
	want := []string{
		"bmc_runtime.password",
		"pdu_runtime.outlets.[].password",
		"amt_runtime.password",
		"tailscale_runtime.auth_key",
		"monitor_runtime.password",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("secret param paths = %v, want %v", got, want)
	}
}

// TestSecretParamPaths_ReturnsCopy: a caller mutating the returned paths
// cannot change what RedactParams redacts.
func TestSecretParamPaths_ReturnsCopy(t *testing.T) {
	p := SecretParamPaths()
	p[0][0] = "mutated"
	if SecretParamPaths()[0][0] != "bmc_runtime" {
		t.Fatal("SecretParamPaths exposes its backing slices")
	}
}

func TestCollectSecretParamPaths_RejectsUnaddressableShapes(t *testing.T) {
	type nonString struct {
		PIN int `json:"pin" secret:"true"`
	}
	type underMap struct {
		ByHost map[string]MonitorRuntime `json:"by_host"`
	}
	type embedded struct {
		MonitorRuntime
	}
	for name, typ := range map[string]reflect.Type{
		"non-string secret":  reflect.TypeFor[nonString](),
		"secret under a map": reflect.TypeFor[underMap](),
		"embedded struct":    reflect.TypeFor[embedded](),
	} {
		if _, err := collectSecretParamPaths(typ, nil, map[reflect.Type]bool{}); err == nil {
			t.Errorf("%s: want an error, got none", name)
		}
	}
}

// fullParams is an OpParams with every field set, each credential to a
// distinct value so a leak of any one of them is detectable.
func fullParams() OpParams {
	return OpParams{
		Destructive:   true,
		WipeAllDisks:  true,
		Recovery:      true,
		FatalAt:       "installing_bootloader",
		Efivarfs:      "skip",
		DataMarker:    "marker-token",
		BootDevice:    "pxe",
		Device:        "disk",
		Persistent:    true,
		RotateMonitor: true,
		BMCRuntime: &BMCRuntime{ExpectedChassisSerial: "SN1", Username: "ADMIN", Password: "fixture-bmc-pw",
			ExtraConfig: map[string]any{"ipmi_cipher": float64(17)}},
		PDURuntime: &PDURuntime{Outlets: []PDUOutlet{
			{PDUHost: "10.0.0.1", Outlet: 5, Serial: "PS1", Username: "apc", Password: "fixture-pdu-pw-1"},
			{PDUHost: "10.0.0.2", Outlet: 7, Serial: "PS2", Username: "apc", Password: "fixture-pdu-pw-2"},
		}, DownstreamSerial: "SN-DOWN", Outlet: true, AllowSelf: true},
		AMTRuntime:       &AMTRuntime{Host: "10.252.240.23", Username: "admin", Password: "fixture-amt-pw"},
		TailscaleRuntime: &TailscaleRuntime{AuthKey: "fixture-tskey", Hostname: "h-pop1", Tags: []string{"tag:pop1"}},
		MonitorRuntime:   &MonitorRuntime{Password: "fixture-monitor-pw"},
	}
}

var fixtureSecrets = []string{
	"fixture-bmc-pw", "fixture-pdu-pw-1", "fixture-pdu-pw-2", "fixture-amt-pw", "fixture-tskey", "fixture-monitor-pw",
}

// leakedFixtureSecrets reports how many fixture credentials appear in raw,
// so a failure message never prints one.
func leakedFixtureSecrets(raw []byte) int {
	n := 0
	for _, s := range fixtureSecrets {
		if bytes.Contains(raw, []byte(s)) {
			n++
		}
	}
	return n
}

// TestRedactParams_FullyPopulated: every credential becomes the marker and
// every other field decodes to exactly what it was.
func TestRedactParams_FullyPopulated(t *testing.T) {
	in := fullParams()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	red := RedactParams(raw)
	if n := leakedFixtureSecrets(red); n > 0 {
		t.Fatalf("redacted params still carry %d credential(s)", n)
	}
	got, err := DecodeParams(red)
	if err != nil {
		t.Fatalf("redacted params no longer decode: %v", err)
	}
	want := in
	bmc, pdu, amt, ts, mon := *in.BMCRuntime, *in.PDURuntime, *in.AMTRuntime, *in.TailscaleRuntime, *in.MonitorRuntime
	pdu.Outlets = slices.Clone(pdu.Outlets)
	bmc.Password, amt.Password, ts.AuthKey, mon.Password = RedactedMarker, RedactedMarker, RedactedMarker, RedactedMarker
	for i := range pdu.Outlets {
		pdu.Outlets[i].Password = RedactedMarker
	}
	want.BMCRuntime, want.PDURuntime, want.AMTRuntime, want.TailscaleRuntime, want.MonitorRuntime = &bmc, &pdu, &amt, &ts, &mon
	if !reflect.DeepEqual(got, want) {
		// Both sides hold only markers and non-secret fixture values here
		// (the leak check above passed), so printing them is safe.
		t.Errorf("redacted params:\n got %+v\nwant %+v", got, want)
	}
}

// TestRedactParams_EmptyCredentialStaysEmpty: an empty credential (dev mode,
// nothing resolved) is shown as empty, not as the marker, so operators can
// still tell that no credential rode the op.
func TestRedactParams_EmptyCredentialStaysEmpty(t *testing.T) {
	raw := json.RawMessage(`{"amt_runtime":{"host":"10.0.0.9","username":"admin","password":""},"monitor_runtime":{"password":null}}`)
	if got := RedactParams(raw); !bytes.Equal(got, raw) {
		t.Errorf("RedactParams changed params with only empty credentials: %s", got)
	}
}

func TestRedactParams_NothingToRedactReturnsInput(t *testing.T) {
	for _, raw := range []json.RawMessage{
		nil,
		json.RawMessage(``),
		json.RawMessage(`null`),
		json.RawMessage(`{}`),
		json.RawMessage(`{"destructive":true,"bmc_runtime":{"expected_chassis_serial":"SN","extra_config":{}}}`),
		json.RawMessage(`{"pdu_runtime":{"outlets":[]}}`),
	} {
		if got := RedactParams(raw); !bytes.Equal(got, raw) {
			t.Errorf("RedactParams(%s) = %s, want it unchanged", raw, got)
		}
	}
}

// TestRedactParams_PreservesUntouchedValues: values off the credential paths
// are never decoded, so they survive exactly (a large integer in
// extra_config keeps every digit instead of rounding through float64).
func TestRedactParams_PreservesUntouchedValues(t *testing.T) {
	raw := json.RawMessage(`{"bmc_runtime":{"password":"fixture-bmc-pw","extra_config":{"ipmi_cipher":17,"big":12345678901234567890}},"device":"pxe"}`)
	red := RedactParams(raw)
	if leakedFixtureSecrets(red) > 0 {
		t.Fatal("credential survived redaction")
	}
	for _, want := range []string{`"big":12345678901234567890`, `"ipmi_cipher":17`, `"device":"pxe"`, `"password":"[redacted]"`} {
		if !bytes.Contains(red, []byte(want)) {
			t.Errorf("redacted params lost %s: %s", want, red)
		}
	}
}

// TestRedactParams_FailsClosed: a credential path whose shape cannot be
// walked (not the object or array the typed union expects) is withheld
// whole rather than passed through.
func TestRedactParams_FailsClosed(t *testing.T) {
	cases := map[string]struct {
		in, want string
	}{
		"runtime is a string":        {`{"bmc_runtime":"fixture-bmc-pw","device":"pxe"}`, `{"bmc_runtime":"[redacted]","device":"pxe"}`},
		"outlets is an object":       {`{"pdu_runtime":{"outlets":{"password":"fixture-pdu-pw-1"}}}`, `{"pdu_runtime":{"outlets":"[redacted]"}}`},
		"outlet element is a string": {`{"pdu_runtime":{"outlets":["fixture-pdu-pw-1",{"password":"fixture-pdu-pw-2"}]}}`, `{"pdu_runtime":{"outlets":["[redacted]",{"password":"[redacted]"}]}}`},
		"credential is a number":     {`{"monitor_runtime":{"password":12345}}`, `{"monitor_runtime":{"password":"[redacted]"}}`},
		"params is an array":         {`[{"amt_runtime":{"password":"fixture-amt-pw"}}]`, `"[redacted]"`},
		"params is a string":         {`"fixture-tskey"`, `"[redacted]"`},
	}
	for name, tc := range cases {
		got := RedactParams(json.RawMessage(tc.in))
		if leakedFixtureSecrets(got) > 0 {
			t.Errorf("%s: credential survived redaction", name)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s: got %s, want %s", name, got, tc.want)
		}
	}
}

// TestRedactParams_DoesNotMutateInput: the worker-facing publish shares the
// op's Params bytes with the operator response, so redaction must never
// write into its input.
func TestRedactParams_DoesNotMutateInput(t *testing.T) {
	raw, err := json.Marshal(fullParams())
	if err != nil {
		t.Fatal(err)
	}
	orig := bytes.Clone(raw)
	_ = RedactParams(raw)
	if !bytes.Equal(raw, orig) {
		t.Fatal("RedactParams modified its input")
	}
}

func TestOperationRedacted(t *testing.T) {
	raw, err := json.Marshal(fullParams())
	if err != nil {
		t.Fatal(err)
	}
	op := Operation{Type: OpDeploy, Params: raw, Result: json.RawMessage(`{"error":"boom"}`)}
	red := op.Redacted()
	if leakedFixtureSecrets(red.Params) > 0 {
		t.Fatal("Redacted left a credential in Params")
	}
	if leakedFixtureSecrets(op.Params) != len(fixtureSecrets) {
		t.Fatal("Redacted modified the receiver's Params")
	}
	if red.Type != op.Type || string(red.Result) != string(op.Result) {
		t.Errorf("Redacted changed fields other than Params: %+v", red)
	}
}
