package awssecrets

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// TestParseAdminSecret_CarriesFields verifies that the parser extracts
// kind + username + password + extra_config, keyed by device serial.
// Password flows through to slagworks: a kind=bmc entry is the only BMC
// credential slagworks uses, and kind=pdu entries drive PDU power ops.
// Other fields (chassis_serial, verified*) are still not-unmarshaled —
// operator tooling reads those, not slagworks.
func TestParseAdminSecret_CarriesFields(t *testing.T) {
	const blob = `{
		"WM235S600435": {
			"kind": "bmc",
			"chassis_serial": "WM235S600435",
			"username": "ADMIN",
			"password": "AAOZFVIEGC",
			"extra_config": {"ipmi_cipher": 3},
			"verified": true,
			"verified_at": "2026-05-20T00:23:11Z"
		}
	}`

	got, err := parseAdminSecret([]byte(blob))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	host, ok := got.BySerial["WM235S600435"]
	if !ok {
		t.Fatal("expected WM235S600435 in parsed secret")
	}
	if host.Kind != KindBMC {
		t.Errorf("kind: got %q, want %q", host.Kind, KindBMC)
	}
	if host.Username != "ADMIN" {
		t.Errorf("username: got %q, want ADMIN", host.Username)
	}
	if host.Password != "AAOZFVIEGC" {
		t.Errorf("password: got %q, want AAOZFVIEGC", host.Password)
	}
	if v, ok := host.ExtraConfig["ipmi_cipher"]; !ok || v.(float64) != 3 {
		t.Errorf("expected ipmi_cipher=3, got %v", host.ExtraConfig)
	}
}

// TestParseAdminSecret_CarriesPDUKind verifies a kind="pdu" entry
// parses with its apc cred — the PDU power path reads these.
func TestParseAdminSecret_CarriesPDUKind(t *testing.T) {
	const blob = `{
		"ZA2451003321": {
			"kind": "pdu",
			"username": "apc",
			"password": "test-password",
			"extra_config": {}
		}
	}`
	got, err := parseAdminSecret([]byte(blob))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	host := got.Lookup("ZA2451003321")
	if host == nil {
		t.Fatal("expected ZA2451003321 in parsed secret")
	}
	if host.Kind != KindPDU {
		t.Errorf("kind: got %q, want %q", host.Kind, KindPDU)
	}
	if host.Username != "apc" || host.Password != "test-password" {
		t.Errorf("unexpected pdu cred: %q/%q", host.Username, host.Password)
	}
}

// TestParseAdminSecret_CarriesSerialKind verifies a kind="serial" entry
// preserves every canonical connection-recipe key. The values deliberately
// include nested JSON without asserting the console consumer's schema.
func TestParseAdminSecret_CarriesSerialKind(t *testing.T) {
	const blob = `{
		"MT2315J16952": {
			"kind": "serial",
			"username": "root",
			"password": "opengear-port-password",
			"extra_config": {
				"console_server": "console1.pop3",
				"console_server_ip": "10.252.1.30",
				"ssh_port": 3001,
				"raw_tcp_port": 4001,
				"opengear_port": 1,
				"baud": 115200,
				"os_serial_login": {
					"fixture_nested": ["opaque", {"enabled": true}]
				}
			}
		}
	}`
	got, err := parseAdminSecret([]byte(blob))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	host := got.Lookup("MT2315J16952")
	if host == nil {
		t.Fatal("expected MT2315J16952 in parsed secret")
	}
	if host.Kind != KindSerial {
		t.Errorf("kind: got %q, want %q", host.Kind, KindSerial)
	}
	if host.Username != "root" || host.Password != "opengear-port-password" {
		t.Errorf("unexpected serial cred: %q/%q", host.Username, host.Password)
	}
	wantExtra := map[string]any{
		"console_server":    "console1.pop3",
		"console_server_ip": "10.252.1.30",
		"ssh_port":          float64(3001),
		"raw_tcp_port":      float64(4001),
		"opengear_port":     float64(1),
		"baud":              float64(115200),
		"os_serial_login": map[string]any{
			"fixture_nested": []any{
				"opaque",
				map[string]any{"enabled": true},
			},
		},
	}
	if !reflect.DeepEqual(host.ExtraConfig, wantExtra) {
		t.Errorf("extra_config: got %#v, want %#v", host.ExtraConfig, wantExtra)
	}
}

// TestParseAdminSecret_CarriesSerialServerKind verifies a
// kind="serial-server" entry parses with its console-server login and the
// three canonical configuration keys. Their deliberately heterogeneous values
// prove Slagworks preserves arbitrary JSON without defining the consumer schema.
func TestParseAdminSecret_CarriesSerialServerKind(t *testing.T) {
	const blob = `{
		"70082601372405": {
			"kind": "serial-server",
			"username": "root",
			"password": "opengear-unit-password",
			"extra_config": {
				"port_map": {
					"fixture_nested": {"enabled": true}
				},
				"ssh_ports": [3001, "opaque"],
				"raw_tcp_ports": "opaque"
			}
		}
	}`
	got, err := parseAdminSecret([]byte(blob))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	host := got.Lookup("70082601372405")
	if host == nil {
		t.Fatal("expected 70082601372405 in parsed secret")
	}
	if host.Kind != KindSerialServer {
		t.Errorf("kind: got %q, want %q", host.Kind, KindSerialServer)
	}
	if host.Username != "root" || host.Password != "opengear-unit-password" {
		t.Errorf("unexpected serial-server cred: %q/%q", host.Username, host.Password)
	}
	wantExtra := map[string]any{
		"port_map": map[string]any{
			"fixture_nested": map[string]any{"enabled": true},
		},
		"ssh_ports": []any{
			float64(3001),
			"opaque",
		},
		"raw_tcp_ports": "opaque",
	}
	if !reflect.DeepEqual(host.ExtraConfig, wantExtra) {
		t.Errorf("extra_config: got %#v, want %#v", host.ExtraConfig, wantExtra)
	}
}

// TestParseAdminSecret_IgnoresMetaKey ensures the spec's optional
// _meta entry doesn't pollute the serial map.
func TestParseAdminSecret_IgnoresMetaKey(t *testing.T) {
	const blob = `{
		"_meta": {"populated_at": "2026-05-20T00:00:00Z", "schema_version": 2},
		"WM235S600001": {"kind": "bmc", "username": "ADMIN", "extra_config": {}}
	}`
	got, err := parseAdminSecret([]byte(blob))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.BySerial["_meta"]; ok {
		t.Error("_meta must not be a device entry")
	}
	if _, ok := got.BySerial["WM235S600001"]; !ok {
		t.Error("WM235S600001 should be present")
	}
}

// TestLookup_AbsentSerialReturnsNil verifies the "use defaults" path:
// serials not in the secret return nil rather than an error, so
// partial secret population during bringup doesn't fail every op. An
// empty serial also returns nil (NetBox didn't know the serial).
func TestLookup_AbsentSerialReturnsNil(t *testing.T) {
	s, err := parseAdminSecret([]byte(`{"WM235S600001": {"kind": "bmc", "username": "ADMIN", "extra_config": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Lookup("MISSING123"); got != nil {
		t.Errorf("expected nil for absent serial, got %+v", got)
	}
	if got := s.Lookup(""); got != nil {
		t.Errorf("expected nil for empty serial, got %+v", got)
	}
	if got := s.Lookup("WM235S600001"); got == nil {
		t.Error("expected non-nil for present serial")
	}
}

// TestFakeReader_RoundTrip is the primary path tests-of-tests use to
// mock the Reader interface. Round-trip a fixture through it.
func TestFakeReader_RoundTrip(t *testing.T) {
	fake := &FakeReader{
		Secrets: map[string]*AdminSecret{
			"pop1": {
				BySerial: map[string]AdminHost{
					"WM235S600001": {Kind: KindBMC, Username: "ADMIN", ExtraConfig: map[string]any{"ipmi_cipher": 3}},
				},
			},
		},
	}
	got, err := fake.GetAdmin(context.Background(), "pop1")
	if err != nil {
		t.Fatal(err)
	}
	if h := got.Lookup("WM235S600001"); h == nil || h.Username != "ADMIN" {
		t.Errorf("unexpected lookup result: %+v", h)
	}
	if _, err := fake.GetAdmin(context.Background(), "popX"); err == nil {
		t.Error("expected error for unknown pop")
	}
}

// gatedAdminSecrets serves pop/<pop>/bmc-creds-admin. Each GetSecretValue
// captures the secret current when it is called, reports its 1-based call
// number on started, then waits for that call's gate before returning: a
// read that AWS answered before a rotation but whose response arrives later.
type gatedAdminSecrets struct {
	secretsAPI // only GetSecretValue is implemented

	mu      sync.Mutex
	body    string
	calls   int
	gates   map[int]chan struct{}
	started chan int
}

func (g *gatedAdminSecrets) GetSecretValue(_ context.Context, _ *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	g.mu.Lock()
	g.calls++
	n, body, gate := g.calls, g.body, g.gates[g.calls]
	g.mu.Unlock()
	g.started <- n
	if gate != nil {
		<-gate
	}
	return &secretsmanager.GetSecretValueOutput{SecretString: &body}, nil
}

func (g *gatedAdminSecrets) rotate(body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.body = body
}

func (g *gatedAdminSecrets) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func adminSecretWithBMCPassword(pw string) string {
	return `{"SN1":{"kind":"bmc","username":"ADMIN","password":"` + pw + `"}}`
}

// TestGetAdmin_CacheKeepsLaterIssuedRead: two callers miss the cache for
// the same pop together (two snapshot rebuilds, or an op creation racing
// `slagctl machine creds`, just after the TTL expired), and the admin
// password is rotated between the two reads. Whichever response arrives
// last, the cache must end up holding the rotated secret: an earlier-issued
// read that returns late must not put the pre-rotation password back for a
// fresh TTL.
func TestGetAdmin_CacheKeepsLaterIssuedRead(t *testing.T) {
	for _, tc := range []struct {
		name         string
		releaseOrder [2]int // call numbers, in the order their responses arrive
	}{
		{name: "responses arrive in issue order", releaseOrder: [2]int{1, 2}},
		{name: "pre-rotation read returns last", releaseOrder: [2]int{2, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gates := map[int]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}
			api := &gatedAdminSecrets{
				body:    adminSecretWithBMCPassword("old-pw"),
				gates:   gates,
				started: make(chan int, 2),
			}
			c := newClient(api, time.Hour)
			ctx := context.Background()

			results := map[int]chan string{1: make(chan string, 1), 2: make(chan string, 1)}
			getAdmin := func(call int) {
				go func() {
					s, err := c.GetAdmin(ctx, "pop1")
					if err != nil {
						results[call] <- "error: " + err.Error()
						return
					}
					results[call] <- s.Lookup("SN1").Password
				}()
				if n := <-api.started; n != call {
					t.Fatalf("GetAdmin issued read %d, want read %d", n, call)
				}
			}

			getAdmin(1) // reads old-pw, response held
			api.rotate(adminSecretWithBMCPassword("new-pw"))
			getAdmin(2) // reads new-pw, response held

			want := map[int]string{1: "old-pw", 2: "new-pw"}
			for _, call := range tc.releaseOrder {
				close(gates[call])
				if got := <-results[call]; got != want[call] {
					t.Fatalf("read %d returned %q to its caller, want %q", call, got, want[call])
				}
			}

			got, err := c.GetAdmin(ctx, "pop1")
			if err != nil {
				t.Fatal(err)
			}
			if n := api.callCount(); n != 2 {
				t.Fatalf("GetAdmin issued %d reads, want 2 (the last call should be a cache hit)", n)
			}
			if pw := got.Lookup("SN1").Password; pw != "new-pw" {
				t.Errorf("cached password is %q after the rotated secret was read and cached; "+
					"a read issued before the rotation replaced it (and restarted the TTL)", pw)
			}
		})
	}
}
