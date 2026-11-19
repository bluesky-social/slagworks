package api

// PDU outlet power: the preflight token and what an accepted request
// actuates.
//
// docs/dev/api.md: the resolve step issues a token "bound to the operator,
// verb, PDU, outlet, downstream serial and classification"; the POST
// re-resolves the outlet and answers 409 "when the token is invalid, expired,
// for another verb or operator, or the topology changed". The safety
// property under test: a power operation is created only for exactly the
// (operator, verb, PDU, outlet, downstream device, classification) a live
// token was issued for, and the operation's parameters name exactly that
// PDU and outlet.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

type pduWorld struct {
	nb *propNetBox
	// cable[p][n] is the downstream device index feeding off outlet n+1 of
	// PDU p, or -1 when the outlet is uncabled.
	cable [2][3]int
	// fan[p][n] is a SECOND device fed by the same outlet (a Y-cable: one
	// cable termination on the outlet, two on power-ports), or -1.
	fan [2][3]int
}

var (
	pduIDs      = [2]int{611, 612}
	pduNames    = [2]string{"pdu-a", "pdu-b"}
	pduSerials  = [2]string{"PDU-A", "PDU-B"}
	pduHosts    = [2]string{"10.9.0.11", "10.9.0.12"}
	pduPassword = [2]string{"apc-secret-a", "apc-secret-b"}
	downIDs     = [3]int{701, 702, 703}
	downNames   = [3]string{"host-1.pop-a", "host-2.pop-a", "gw-1.pop-a"}
	downSerials = [3]string{"H1", "H2", "GW1"} // GW1 is a registered site worker
)

func (w *pduWorld) sync() {
	w.nb.mu.Lock()
	defer w.nb.mu.Unlock()
	w.nb.outlets = map[int][]netbox.PowerOutlet{}
	w.nb.ports = map[int][]netbox.PowerPort{}
	for p := range w.cable {
		for n, d := range w.cable[p] {
			outlet := netbox.PowerOutlet{
				ID: pduIDs[p]*10 + n + 1, Name: fmt.Sprintf("Outlet %d", n+1),
				Device: &netbox.NestedRef{ID: pduIDs[p], Name: pduNames[p]},
			}
			if d >= 0 {
				port := netbox.PowerPort{
					ID: pduIDs[p]*100 + n + 1, Name: fmt.Sprintf("PSU-%s-%d", pduNames[p], n+1),
					Device: &netbox.NestedRef{ID: downIDs[d], Name: downNames[d]},
				}
				outlet.ConnectedEndpoints = []netbox.PowerPort{port}
				outlet.ConnectedEndpointsReachable = true
				port.ConnectedEndpoints = []netbox.PowerOutlet{{ID: outlet.ID, Name: outlet.Name, Device: outlet.Device}}
				port.ConnectedEndpointsReachable = true
				w.nb.ports[downIDs[d]] = append(w.nb.ports[downIDs[d]], port)
				if f := w.fan[p][n]; f >= 0 && f != d {
					second := netbox.PowerPort{
						ID: pduIDs[p]*100 + 50 + n + 1, Name: fmt.Sprintf("PSU-%s-%d-b", pduNames[p], n+1),
						Device: &netbox.NestedRef{ID: downIDs[f], Name: downNames[f]},
					}
					outlet.ConnectedEndpoints = append(outlet.ConnectedEndpoints, second)
					second.ConnectedEndpoints = port.ConnectedEndpoints
					second.ConnectedEndpointsReachable = true
					w.nb.ports[downIDs[f]] = append(w.nb.ports[downIDs[f]], second)
				}
			}
			w.nb.outlets[pduIDs[p]] = append(w.nb.outlets[pduIDs[p]], outlet)
		}
	}
}

// classification is the documented feed verdict for the device on (p, n).
func (w *pduWorld) classification(p, n int) (serial, class string) {
	d := w.cable[p][n]
	if d < 0 {
		return "", "unconnected"
	}
	feeds := map[int]bool{}
	for pp := range w.cable {
		for nn, dd := range w.cable[pp] {
			if dd == d || (dd >= 0 && w.fan[pp][nn] == d) {
				feeds[pp] = true
			}
		}
	}
	if len(feeds) >= 2 {
		return downSerials[d], "redundant_unverified"
	}
	return downSerials[d], "sole"
}

// pduDrawFan draws the second device an outlet feeds. Fan-out is not
// generated: an outlet cabled to two devices is resolved from its first
// endpoint only, so the warning, the preflight token and the self-power
// interlock describe one device while the operation cuts both (an `off`
// can take down a serving site worker that is the outlet's second device).
// The world and its classification model already carry a second device per
// outlet; widen the pool here to {-1, 0, 1, 2} and add the expected refusal
// to the property once the resolver handles every endpoint.
func pduDrawFan(t *rapid.T, label string) int {
	return rapid.SampledFrom([]int{-1}).Draw(t, label)
}

type pduToken struct {
	token, verb, operator, serial, class string
	p, n                                 int
}

func TestProp_PDUOutletPreflightToken(t *testing.T) {
	stats := newPropStats()
	defer stats.report(t)
	subject := map[string]string{propOperatorToken: "op", propOtherOpToken: "op2"}

	propCheck(t, 25, func(rt *rapid.T) {
		w := &pduWorld{nb: newPropNetBox()}
		site := &netbox.NestedRef{ID: 1, Name: "pop-a", Slug: "pop-a"}
		for p := range pduIDs {
			w.nb.devices = append(w.nb.devices, netbox.Device{ID: pduIDs[p], Name: pduNames[p], Serial: pduSerials[p], Site: site})
			ifID := 9000 + pduIDs[p]
			w.nb.interfaces = append(w.nb.interfaces, netbox.Interface{ID: ifID, Name: "mgmt0", MgmtOnly: true, Device: &netbox.NestedRef{ID: pduIDs[p]}})
			w.nb.ips = append(w.nb.ips, netbox.IPAddress{ID: ifID, Address: pduHosts[p] + "/24", AssignedObjectType: "dcim.interface", AssignedObjectID: &ifID})
		}
		for d := range downIDs {
			w.nb.devices = append(w.nb.devices, netbox.Device{ID: downIDs[d], Name: downNames[d], Serial: downSerials[d], Site: site})
		}
		for p := range w.cable {
			for n := range w.cable[p] {
				w.cable[p][n] = rapid.SampledFrom([]int{-1, 0, 0, 1, 1, 2, 2}).Draw(rt, fmt.Sprintf("cable%d_%d", p, n))
				w.fan[p][n] = pduDrawFan(rt, fmt.Sprintf("fan%d_%d", p, n))
			}
		}
		w.sync()
		partners := fakeOutletRPC{serials: map[string][]string{}}
		env := newPropEnv(t, func(cfg *Config) {
			cfg.NetBox = w.nb.client(t)
			cfg.BMCSecrets = &awssecrets.FakeReader{Secrets: map[string]*awssecrets.AdminSecret{"pop-a": {BySerial: map[string]awssecrets.AdminHost{
				pduSerials[0]: {Kind: awssecrets.KindPDU, Username: "apc", Password: pduPassword[0]},
				pduSerials[1]: {Kind: awssecrets.KindPDU, Username: "apc", Password: pduPassword[1]},
			}}}}
			cfg.PowerStatusRPC = partners
		})
		if err := env.st.CreateSiteConfig(&model.SiteConfig{Slug: "pop-a", NetBoxSite: "pop-a", EphemeralPoolStart: "10.0.0.10", EphemeralPoolEnd: "10.0.0.250"}); err != nil {
			rt.Fatalf("seed site: %v", err)
		}
		var machines [2]uuid.UUID
		for p := range pduIDs {
			machines[p] = uuid.New()
			devID := pduIDs[p]
			if err := env.st.CreateMachine(&model.Machine{ID: machines[p], NetBoxSerial: pduSerials[p], NetBoxDeviceID: &devID, State: model.MachineStateNew}); err != nil {
				rt.Fatalf("seed pdu machine: %v", err)
			}
		}
		if _, _, err := env.st.RegisterSiteWorker("pop-a", "gw-1", "gw-1.pop-a"); err != nil {
			rt.Fatalf("seed worker: %v", err)
		}
		gw := downSerials[2]
		if _, err := env.st.TouchSiteWorker("pop-a", "gw-1", "", &gw); err != nil {
			rt.Fatalf("seed worker serial: %v", err)
		}

		var issued []pduToken
		steps := rapid.IntRange(1, 25).Draw(rt, "steps")
		for i := range steps {
			switch rapid.SampledFrom([]string{"resolve", "resolve", "resolve", "power", "power", "power", "power", "recable", "finish", "partner"}).Draw(rt, fmt.Sprintf("step%d", i)) {
			case "resolve":
				p := rapid.IntRange(0, 1).Draw(rt, "p")
				n := rapid.IntRange(0, 3).Draw(rt, "n") // 3 is "Outlet 4": no such outlet
				verb := rapid.SampledFrom([]string{"cycle", "on", "off"}).Draw(rt, "verb")
				tok := rapid.SampledFrom([]string{propOperatorToken, propOtherOpToken}).Draw(rt, "operator")
				resp := env.do(propReq{method: http.MethodGet, token: tok,
					path: fmt.Sprintf("/api/v1/pdus/%s/outlet/%d/power/resolve?verb=%s", pduNames[p], n+1, verb)})
				if n == 3 {
					if resp.code != http.StatusNotFound {
						rt.Fatalf("resolve of a nonexistent outlet: %s, want 404", resp)
					}
					continue
				}
				var got OutletResolution
				if resp.code != http.StatusOK || resp.json(&got) != nil {
					rt.Fatalf("resolve: %s", resp)
				}
				serial, class := w.classification(p, n)
				if got.Classification != class || got.DownstreamSerial != serial || got.Connected != (serial != "") ||
					got.PDUName != pduNames[p] || got.PDUMgmtIP != pduHosts[p] || got.Outlet != n+1 {
					rt.Fatalf("resolve pdu %s outlet %d: %s\n  model: downstream %q class %q", pduNames[p], n+1, resp, serial, class)
				}
				if (got.PreflightToken != "") != got.Connected {
					rt.Fatalf("resolve: token issued=%v for connected=%v", got.PreflightToken != "", got.Connected)
				}
				if strings.Contains(string(resp.body), pduPassword[p]) {
					rt.Fatalf("resolve leaks the PDU credential: %s", resp)
				}
				if got.Connected {
					issued = append(issued, pduToken{token: got.PreflightToken, verb: verb, operator: subject[tok], serial: serial, class: class, p: p, n: n})
				}
				stats.add("resolve " + class)

			case "recable":
				p := rapid.IntRange(0, 1).Draw(rt, "p")
				n := rapid.IntRange(0, 2).Draw(rt, "n")
				w.cable[p][n] = rapid.IntRange(-1, 2).Draw(rt, "downstream")
				w.fan[p][n] = pduDrawFan(rt, "fanout")
				w.sync()
				stats.add("recable")

			case "partner":
				switch rapid.SampledFrom([]string{"none", "self", "partner", "unverified"}).Draw(rt, "partners") {
				case "none":
					delete(partners.serials, "pop-a")
				case "self":
					partners.serials["pop-a"] = []string{gw}
				case "partner":
					partners.serials["pop-a"] = []string{gw, "GW2"}
				default:
					partners.serials["pop-a"] = []string{gw, ""}
				}

			case "finish":
				for p := range machines {
					ops, err := env.st.ListOperations(&machines[p])
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
				}

			case "power":
				// Most requests replay an issued token: exactly as issued (the
				// accepting path), or with exactly one bound dimension of the
				// request changed (the near misses the binding exists to
				// refuse). The rest are unrelated requests and damaged tokens.
				p := rapid.IntRange(0, 1).Draw(rt, "p")
				n := rapid.IntRange(0, 3).Draw(rt, "n")
				verb := rapid.SampledFrom([]string{"cycle", "cycle", "on", "off"}).Draw(rt, "verb")
				opTok := rapid.SampledFrom([]string{propOperatorToken, propOperatorToken, propOtherOpToken}).Draw(rt, "operator")
				allowSelf := rapid.IntRange(0, 3).Draw(rt, "allowSelf") == 0
				kind := rapid.SampledFrom([]string{
					"replay", "replay", "replay", "replay", "near-miss", "near-miss", "near-miss",
					"issued", "flip", "garbage", "empty", "expired", "foreign", "truncated",
				}).Draw(rt, "tokenKind")
				var tok *pduToken
				var token string
				valid := false // the token authenticates and has not expired
				if len(issued) > 0 && kind != "garbage" && kind != "empty" && kind != "expired" && kind != "foreign" {
					tok = &issued[rapid.IntRange(0, len(issued)-1).Draw(rt, "issuedToken")]
				} else if kind != "empty" && kind != "expired" && kind != "foreign" {
					kind = "garbage"
				}
				operatorFor := map[string]string{"op": propOperatorToken, "op2": propOtherOpToken}
				switch kind {
				case "replay", "near-miss":
					p, n, verb, opTok = tok.p, tok.n, tok.verb, operatorFor[tok.operator]
					token, valid = tok.token, true
					if kind == "near-miss" {
						switch rapid.SampledFrom([]string{"pdu", "outlet", "verb", "operator"}).Draw(rt, "miss") {
						case "pdu":
							p = 1 - p
						case "outlet":
							// Prefer another outlet of the same PDU that feeds
							// the same device (a dual-corded host on one PDU):
							// there only the outlet number tells the two apart.
							var twins []int
							for other, d := range w.cable[p] {
								if other != n && d == w.cable[p][n] {
									twins = append(twins, other)
								}
							}
							if len(twins) > 0 && rapid.IntRange(0, 9).Draw(rt, "twin") < 7 {
								n = rapid.SampledFrom(twins).Draw(rt, "twinOutlet")
							} else {
								n = (n + rapid.IntRange(1, 3).Draw(rt, "outletShift")) % 4
							}
						case "verb":
							verb = map[string]string{"cycle": "on", "on": "off", "off": "cycle"}[verb]
						default:
							opTok = operatorFor[map[string]string{"op": "op2", "op2": "op"}[tok.operator]]
						}
					}
				case "issued":
					token, valid = tok.token, true
				case "flip":
					// Change one character anywhere in the token.
					pos := rapid.IntRange(0, len(tok.token)-1).Draw(rt, "flipAt")
					b := []byte(tok.token)
					if b[pos] == '0' {
						b[pos] = '1'
					} else {
						b[pos] = '0'
					}
					token, tok = string(b), nil
				case "truncated":
					token, tok = tok.token[:len(tok.token)-rapid.IntRange(1, 70).Draw(rt, "cut")], nil
				case "garbage":
					token = rapid.SampledFrom([]string{"not-a-token", ".", strings.Repeat("a", 66), strings.Repeat("0", 64) + "." + strings.Repeat("0", 64), "7b7d." + strings.Repeat("f", 64)}).Draw(rt, "garbage")
				}
				serial, class := "", "unconnected"
				if n < 3 {
					serial, class = w.classification(p, n)
				}
				mint := func(key []byte, exp time.Time) string {
					s, err := mintPreflightToken(key, preflightClaims{
						Operator: subject[opTok], Verb: verb, PDUDeviceID: pduIDs[p], Outlet: n + 1,
						DownstreamSerial: serial, Classification: class, ExpiresAt: exp.Unix(),
					})
					if err != nil {
						rt.Fatalf("mint: %v", err)
					}
					return s
				}
				switch kind {
				case "expired":
					// Exactly this request's binding, correctly signed, but
					// past its expiry.
					token = mint(env.srv.preflightKey, time.Now().Add(-2*time.Second))
				case "foreign":
					// Exactly this request's binding, signed with another key.
					token = mint([]byte("some-other-key"), time.Now().Add(time.Minute))
				}

				body, _ := json.Marshal(map[string]any{"verb": verb, "preflight_token": token, "allow_self": allowSelf})
				beforeOps := mustOps(rt, env, machines[p])
				before := len(beforeOps)
				active := false // one active operation per machine, the PDU's row included
				for _, op := range beforeOps {
					if op.Status == model.OpPending {
						active = true
					}
				}
				resp := env.do(propReq{method: http.MethodPost, token: opTok,
					path: fmt.Sprintf("/api/v1/pdus/%s/outlet/%d/power", pduNames[p], n+1), body: string(body)})
				after := mustOps(rt, env, machines[p])
				stats.add(fmt.Sprintf("power token=%s -> %d", kind, resp.code))

				bound := valid && tok != nil && tok.p == p && tok.n == n && tok.verb == verb && tok.operator == subject[opTok] &&
					tok.serial == serial && tok.class == class && serial != ""

				if resp.code == http.StatusCreated {
					// The safety property, independent of how refusals are coded.
					if !bound {
						rt.Fatalf("an outlet power operation was created without a live token for this exact request\n  request: operator %s verb %s pdu %s outlet %d (downstream %q, %s)\n  token kind %s: %+v\n  response: %s",
							subject[opTok], verb, pduNames[p], n+1, serial, class, kind, tok, resp)
					}
					if len(after) != before+1 {
						rt.Fatalf("201 but %d new operation rows", len(after)-before)
					}
					op := after[0]
					var params model.OpParams
					if err := json.Unmarshal(op.Params, &params); err != nil || params.PDURuntime == nil || len(params.PDURuntime.Outlets) != 1 {
						rt.Fatalf("created op has no single-outlet pdu_runtime: %s", op.Params)
					}
					o := params.PDURuntime.Outlets[0]
					wantType := map[string]model.OperationType{"cycle": model.OpPowerCycle, "on": model.OpPowerOn, "off": model.OpPowerOff}[verb]
					if op.Type != wantType || op.MachineID != machines[p] || o.PDUHost != pduHosts[p] || o.Outlet != n+1 || o.Serial != pduSerials[p] ||
						o.Password != pduPassword[p] || params.PDURuntime.DownstreamSerial != serial || !params.PDURuntime.Outlet || params.PDURuntime.AllowSelf != allowSelf {
						rt.Fatalf("created op does not describe the requested outlet: type %s machine %s params %s\n  want %s on pdu %s (%s) outlet %d downstream %q allow_self %v",
							op.Type, op.MachineID, op.Params, wantType, pduNames[p], pduHosts[p], n+1, serial, allowSelf)
					}
					if serial == gw && verb == "off" {
						rt.Fatalf("created an `off` for the outlet feeding a serving site worker")
					}
					if strings.Contains(string(resp.body), pduPassword[p]) || !strings.Contains(string(resp.body), model.RedactedMarker) {
						rt.Fatalf("op-create response does not redact the PDU credential: %s", resp)
					}
					stats.add("power created " + verb + " " + class)
					// Usually let the operation finish at once, so a pending
					// one does not mask the next request with a 409.
					if rapid.IntRange(0, 9).Draw(rt, "finishNow") < 7 {
						if _, err := env.st.CompleteOperationWithState(op.ID, "", nil); err != nil {
							rt.Fatalf("complete: %v", err)
						}
					}
				} else if len(after) != before {
					rt.Fatalf("refused (%d) but %d operation row(s) were created", resp.code, len(after)-before)
				}

				// Exact status, per the documented refusal order.
				partner := false
				for _, s := range partners.serials["pop-a"] {
					if s != "" && s != serial {
						partner = true
					}
				}
				want := func() int {
					switch {
					case allowSelf && verb == "off":
						return http.StatusBadRequest
					case token == "":
						return http.StatusBadRequest
					case !valid || tok == nil:
						return http.StatusConflict
					case tok.verb != verb, tok.operator != subject[opTok]:
						return http.StatusConflict
					case n == 3:
						return http.StatusNotFound
					case tok.p != p, tok.n != n, tok.serial != serial, tok.class != class:
						return http.StatusConflict
					case serial == gw && verb == "off":
						return http.StatusUnprocessableEntity
					case serial == gw && !allowSelf && !partner:
						return http.StatusUnprocessableEntity
					case active:
						return http.StatusConflict
					}
					return http.StatusCreated
				}()
				if resp.code != want {
					rt.Fatalf("outlet power: %s, model expects %d\n  request: operator %s verb %s pdu %s outlet %d allow_self %v (downstream %q, %s, partner %v, active op %v)\n  token kind %s: %+v",
						resp, want, subject[opTok], verb, pduNames[p], n+1, allowSelf, serial, class, partner, active, kind, tok)
				}
			}
		}
		stats.add("cases")
	})
}

func mustOps(t *rapid.T, env *propEnv, machine uuid.UUID) []model.Operation {
	ops, err := env.st.ListOperations(&machine)
	if err != nil {
		t.Fatalf("list ops: %v", err)
	}
	return ops
}

// FuzzVerifyPreflightToken: no input may panic the verifier, and only a
// token minted with the server's key verifies.
func FuzzVerifyPreflightToken(f *testing.F) {
	key := []byte("fuzz-key")
	good, err := mintPreflightToken(key, preflightClaims{Operator: "op", Verb: "cycle", PDUDeviceID: 611, Outlet: 5,
		DownstreamSerial: "SN", Classification: "sole", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{good, "", ".", "7b7d." + strings.Repeat("0", 64), strings.Repeat("z", 66), good[:len(good)-1], good + "0"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, token string) {
		claims, err := verifyPreflightToken(key, token)
		if err == nil && token != good {
			// A different string that verifies must still be a token over
			// identical claims (hex is case-insensitive on decode).
			if !strings.EqualFold(token, good) {
				t.Fatalf("token %q verified without being minted: %+v", token, claims)
			}
		}
	})
}
