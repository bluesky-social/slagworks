package api

// Credential handling at operation creation, with NetBox and the secret
// store wired.
//
// docs/dev/state-machine.md, "Credentials in operations": operators never
// see a credential (every operator response shows "[redacted]"); workers get
// the real value; terminal ops drop them from the stored row. docs/dev/api.md
// gives the refusal codes: 422 when no credential resolves, and `via`
// selects the transport, defaulting to amt when the device's admin entry is
// kind=amt and bmc otherwise.
//
// The generated world gives each machine one of the credential situations
// below; histories create operations of every type over every transport,
// finish some, scrub, and read everything back. Two properties are checked
// throughout: the status of each create against a reference decision table,
// and that no secret value ever appears in an operator response or in the
// server's log.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// crSituations are the credential situations a machine can be in.
var crSituations = []string{
	"bmc-ok", "bmc-ok", "bmc-ok", // a complete kind=bmc entry
	"bmc-incomplete", // kind=bmc, empty password
	"amt-ok",         // kind=amt entry and an active oob_ip
	"amt-no-oob",     // kind=amt entry, device has no oob_ip
	"amt-reserved",   // kind=amt entry, oob_ip is status=reserved
	"pdu-kind",       // the serial's entry is kind=pdu
	"no-entry",       // the pop's secret has no entry for the serial
	"no-serial",      // the NetBox device has no serial
	"unbound",        // the machine has no NetBox device
	"gone",           // the machine's NetBox device no longer exists
}

type crMachine struct {
	id        uuid.UUID
	situation string
	secret    string // the admin password, when the situation has one
	monitor   string // the stored monitor password ("" = none stored)
	active    bool   // an active operation exists
}

type crLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *crLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *crLog) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Contains(l.b.Bytes(), []byte(s))
}

func TestProp_OperationCredentials(t *testing.T) {
	stats := newPropStats()
	defer stats.report(t)

	propCheck(t, 40, func(rt *rapid.T) {
		nb := newPropNetBox()
		secrets := &awssecrets.FakeReader{
			Secrets: map[string]*awssecrets.AdminSecret{"pop-a": {BySerial: map[string]awssecrets.AdminHost{}}},
			Monitor: map[string]map[string]awssecrets.BMCMonitorEntry{"pop-a": {}},
		}
		logs := &crLog{}
		env, _ := propFullEnv(t, nb, logs)
		env.srv.bmcSecrets = secrets
		env.srv.snapshotBuilder = nil // preflight has its own tests; a nil builder passes
		if err := env.st.CreateSSHKey(&model.SSHKey{PublicKey: propSSHKey(0)}); err != nil {
			rt.Fatalf("seed key: %v", err)
		}
		site := &netbox.NestedRef{ID: 1, Name: "pop-a", Slug: "pop-a"}

		var secretValues []string
		n := rapid.IntRange(1, 4).Draw(rt, "machines")
		machines := make([]*crMachine, n)
		for i := range machines {
			m := &crMachine{id: uuid.New(), situation: rapid.SampledFrom(crSituations).Draw(rt, fmt.Sprintf("situation%d", i))}
			devID := 500 + i
			serial := fmt.Sprintf("SN-CR-%d", i)
			row := &model.Machine{ID: m.id, NetBoxSerial: serial, State: model.MachineStateCommissioned, PXEEligible: true}
			dev := netbox.Device{ID: devID, Name: fmt.Sprintf("host-%d.pop-a", i), Serial: serial, Site: site}
			m.secret = fmt.Sprintf("admin-secret-%d-%s", i, uuid.NewString()[:8])
			entry := awssecrets.AdminHost{Kind: awssecrets.KindBMC, Username: "ADMIN", Password: m.secret}
			switch m.situation {
			case "bmc-ok":
			case "bmc-incomplete":
				entry.Password, m.secret = "", ""
			case "amt-ok", "amt-no-oob", "amt-reserved":
				entry.Kind = awssecrets.KindAMT
				if m.situation != "amt-no-oob" {
					ipID := 800 + i
					ip := netbox.IPAddress{ID: ipID, Address: fmt.Sprintf("10.8.0.%d/24", i+1)}
					if m.situation == "amt-reserved" {
						ip.Status = &netbox.NestedValue{Value: "reserved"}
					}
					nb.ips = append(nb.ips, ip)
					dev.OOBIP = &netbox.NestedIPAddress{ID: ipID, Address: ip.Address}
				}
			case "pdu-kind":
				entry.Kind = awssecrets.KindPDU
			case "no-entry":
				entry.Kind, m.secret = "", ""
			case "no-serial":
				dev.Serial = ""
			}
			if entry.Kind != "" {
				secrets.Secrets["pop-a"].BySerial[serial] = entry
			}
			switch m.situation {
			case "unbound":
			case "gone":
				row.NetBoxDeviceID = &devID // the device is never added to NetBox
			default:
				row.NetBoxDeviceID = &devID
				nb.devices = append(nb.devices, dev)
			}
			if rapid.Bool().Draw(rt, fmt.Sprintf("monitor%d", i)) {
				m.monitor = fmt.Sprintf("monitor-secret-%d-%s", i, uuid.NewString()[:8])
				secrets.Monitor["pop-a"][serial] = awssecrets.BMCMonitorEntry{Username: bmcreconcile.MonitorUser, Password: m.monitor}
			}
			if err := env.st.CreateMachine(row); err != nil {
				rt.Fatalf("seed machine: %v", err)
			}
			for _, s := range []string{m.secret, m.monitor} {
				if s != "" {
					secretValues = append(secretValues, s)
				}
			}
			machines[i] = m
		}

		// checkSafe: nothing an operator can read, and nothing logged,
		// contains a secret value.
		checkSafe := func(what string, body []byte) {
			for _, s := range secretValues {
				if bytes.Contains(body, []byte(s)) {
					rt.Fatalf("%s returns a plaintext credential to the operator: %s", what, body)
				}
				if logs.contains(s) {
					rt.Fatalf("after %s the server log contains a plaintext credential", what)
				}
			}
		}

		steps := rapid.IntRange(1, 20).Draw(rt, "steps")
		for i := range steps {
			m := machines[rapid.IntRange(0, len(machines)-1).Draw(rt, fmt.Sprintf("m%d", i))]
			base := "/api/v1/machines/" + m.id.String()
			switch action := rapid.SampledFrom([]string{"deploy", "commission", "power", "power", "boot", "bmc-reconcile", "finish", "finish", "read"}).Draw(rt, fmt.Sprintf("action%d", i)); action {
			case "finish":
				ops, err := env.st.ListOperations(&m.id)
				if err != nil {
					rt.Fatalf("list ops: %v", err)
				}
				for _, op := range ops {
					if op.Status == model.OpPending {
						if _, err := env.st.CompleteOperationWithState(op.ID, "", nil); err != nil {
							rt.Fatalf("complete: %v", err)
						}
					}
				}
				m.active = false
				if _, err := env.st.ScrubTerminalOperationCredentials(); err != nil {
					rt.Fatalf("scrub: %v", err)
				}
				// Terminal ops no longer hold a credential in the table.
				ops, _ = env.st.ListOperations(&m.id)
				for _, op := range ops {
					for _, s := range secretValues {
						if op.Status != model.OpPending && bytes.Contains(op.Params, []byte(s)) {
							rt.Fatalf("terminal op %s still stores a credential after the scrub: %s", op.ID, op.Params)
						}
					}
				}

			case "read":
				for _, path := range []string{"/api/v1/operations", "/api/v1/operations?machine=" + m.id.String(), base, "/api/v1/machines"} {
					resp := env.get(path)
					anyGone := false
					for _, x := range machines {
						anyGone = anyGone || x.situation == "gone"
					}
					// The machine list answers 502 while any listed machine's
					// NetBox device is missing: hostname enrichment fails as a
					// whole rather than serving a row without its hostname.
					refused := anyGone && path == "/api/v1/machines" && resp.code == http.StatusBadGateway
					if resp.code != http.StatusOK && !refused {
						rt.Fatalf("GET %s: %s", path, resp)
					}
					checkSafe("GET "+path, resp.body)
				}

			default:
				via := rapid.SampledFrom([]string{"", "", "", "bmc", "amt", "pdu"}).Draw(rt, fmt.Sprintf("via%d", i))
				body := map[string]any{}
				if via != "" && action != "boot" && action != "bmc-reconcile" {
					body["via"] = via
				} else {
					via = ""
				}
				bootDevice := ""
				switch action {
				case "deploy":
					body["destructive"] = true
				case "power":
					body["action"] = "cycle"
					if bootDevice = rapid.SampledFrom([]string{"", "", "pxe", "disk"}).Draw(rt, fmt.Sprintf("boot%d", i)); bootDevice != "" {
						body["boot_device"] = bootDevice
					}
				case "boot":
					body["device"] = "pxe"
				case "bmc-reconcile":
					body["rotate_monitor"] = rapid.Bool().Draw(rt, fmt.Sprintf("rotate%d", i))
				}
				raw, _ := json.Marshal(body)
				resp := env.post(base+"/"+action, string(raw))
				checkSafe(action, resp.body)

				// Reference decision. The transport: an explicit via wins;
				// omitted, the admin entry's kind decides.
				amtEntry := strings.HasPrefix(m.situation, "amt-")
				transport := via
				if transport == "" {
					transport = "bmc"
					if amtEntry {
						transport = "amt"
					}
				}
				want := func() int {
					if action == "power" && via == "pdu" && bootDevice != "" {
						return http.StatusBadRequest
					}
					if m.situation == "gone" && via == "" {
						return http.StatusServiceUnavailable // NetBox cannot return the device to pick a transport
					}
					switch action {
					case "boot", "bmc-reconcile":
						if transport == "amt" {
							return http.StatusUnprocessableEntity // BMC-only verbs
						}
					}
					switch transport {
					case "pdu":
						return http.StatusUnprocessableEntity // no power-ports are modeled in this world
					case "amt":
						if bootDevice == "disk" {
							return http.StatusBadRequest
						}
						if m.situation == "gone" {
							return http.StatusServiceUnavailable // NetBox cannot return the device
						}
						if m.situation != "amt-ok" {
							return http.StatusUnprocessableEntity // no active oob_ip or no complete kind=amt entry
						}
					default:
						if m.situation == "gone" {
							return http.StatusServiceUnavailable
						}
						if m.situation != "bmc-ok" {
							return http.StatusUnprocessableEntity
						}
					}
					if m.active {
						return http.StatusConflict
					}
					return http.StatusCreated
				}()
				if resp.code != want {
					rt.Fatalf("%s via %q on a %s machine: %s, reference expects %d (transport %s, active op %v)",
						action, via, m.situation, resp, want, transport, m.active)
				}
				stats.add(fmt.Sprintf("%s %s -> %d", action, m.situation, resp.code))
				if resp.code != http.StatusCreated {
					continue
				}
				m.active = true
				var op model.Operation
				if err := resp.json(&op); err != nil {
					rt.Fatalf("%s: %v", action, err)
				}
				if !bytes.Contains(resp.body, []byte(model.RedactedMarker)) {
					rt.Fatalf("%s: 201 body shows no redaction marker where the credential is: %s", action, resp)
				}
				// The stored row (what workers receive) carries the real
				// credential for exactly this machine.
				stored, err := env.st.GetOperation(op.ID)
				if err != nil {
					rt.Fatalf("stored op: %v", err)
				}
				if !bytes.Contains(stored.Params, []byte(m.secret)) {
					rt.Fatalf("%s: the stored op does not carry the machine's credential: %s", action, stored.Params)
				}
				for _, other := range machines {
					if other != m && other.secret != "" && bytes.Contains(stored.Params, []byte(other.secret)) {
						rt.Fatalf("%s on machine %s carries ANOTHER machine's credential (%s)", action, m.id, other.id)
					}
				}
				var p model.OpParams
				if err := json.Unmarshal(stored.Params, &p); err != nil {
					rt.Fatalf("params: %v", err)
				}
				if (transport == "amt") != (p.AMTRuntime != nil) || (transport == "bmc") != (p.BMCRuntime != nil) {
					rt.Fatalf("%s: transport %s but params are %s", action, transport, stored.Params)
				}
				if action == "bmc-reconcile" {
					rotate, _ := body["rotate_monitor"].(bool)
					wantMonitor := m.monitor != "" && !rotate
					if (p.MonitorRuntime != nil) != wantMonitor || (wantMonitor && p.MonitorRuntime.Password != m.monitor) {
						rt.Fatalf("bmc-reconcile rotate=%v stored monitor=%v: params %s", rotate, m.monitor != "", stored.Params)
					}
				}
				got := env.get("/api/v1/operations/" + op.ID.String())
				checkSafe("GET operation", got.body)
			}
		}
		stats.add("cases")
	})
}
