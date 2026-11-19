package netbox

// Property tests for Client.List pagination, retry and decoding against a
// generated device world served by an in-process paginating server.
//
//   P1 exact set: List returns exactly the world's objects, in order, with
//      no drops or duplicates at page boundaries (world sizes at, below and
//      above multiples of the page size).
//   P2 decoding: serial / primary_ip4 / status / asset_tag decode to the
//      world value; null and missing both decode to the zero value and are
//      never coerced into some other device's value.
//   P3 retries: k consecutive 429s on one request succeed iff k is within
//      the documented retry budget (ClientConfig.MaxRetries: "MaxRetries for
//      rate-limited (429) responses (default 3)"); a 5xx fails the whole
//      List (no silent partial result). Requests are bounded.
//   P4 base path: a NetBox served under a path prefix (BASE_PATH) works.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"pgregory.net/rapid"
)

// The properties in this package run from a fixed seed unless one is given,
// so a plain go test explores the same cases every time. The default is set
// before flags are parsed: -rapid.seed and -rapid.checks on the command
// line still apply, and a deep run should pass a fresh seed.
func init() {
	if err := flag.Set("rapid.seed", "1"); err != nil {
		panic(err)
	}
}

type genDevice struct {
	ID int
	// SerialMode: 0 value, 1 empty string, 2 null, 3 missing
	SerialMode int
	Serial     string
	// IPMode: 0 object, 1 null, 2 missing
	IPMode   int
	IP       string
	Status   string // may be an unknown enum value
	AssetTag *string
}

func (d genDevice) render() map[string]any {
	m := map[string]any{"id": d.ID, "name": fmt.Sprintf("host-%d", d.ID)}
	switch d.SerialMode {
	case 0:
		m["serial"] = d.Serial
	case 1:
		m["serial"] = ""
	case 2:
		m["serial"] = nil
	}
	switch d.IPMode {
	case 0:
		m["primary_ip4"] = map[string]any{"id": d.ID + 10000, "address": d.IP}
	case 1:
		m["primary_ip4"] = nil
	}
	m["status"] = map[string]any{"value": d.Status, "label": strings.ToUpper(d.Status)}
	if d.AssetTag != nil {
		m["asset_tag"] = *d.AssetTag
	} else {
		m["asset_tag"] = nil
	}
	return m
}

func (d genDevice) wantSerial() string {
	if d.SerialMode == 0 {
		return d.Serial
	}
	return ""
}

type pageServer struct {
	mu       sync.Mutex
	devices  []genDevice
	basePath string
	// faults: request number (1-based) -> list of statuses to return on
	// consecutive attempts of that logical request before succeeding.
	faultReq    int
	faultSeq    []int
	served      int // successful page responses
	attempts    int // all requests
	inFaultLeft int
}

func (p *pageServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attempts++
	if !strings.HasPrefix(r.URL.Path, p.basePath+"/api/dcim/devices/") {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "Not found: " + r.URL.Path})
		return
	}
	logical := p.served + 1
	if logical == p.faultReq && p.inFaultLeft > 0 {
		st := p.faultSeq[len(p.faultSeq)-p.inFaultLeft]
		p.inFaultLeft--
		writeJSON(w, st, map[string]any{"detail": "injected"})
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	end := min(offset+limit, len(p.devices))
	results := []any{}
	for _, d := range p.devices[min(offset, len(p.devices)):end] {
		results = append(results, d.render())
	}
	var next any
	if end < len(p.devices) {
		nq := url.Values{}
		for k, v := range q {
			nq[k] = v
		}
		nq.Set("offset", strconv.Itoa(end))
		next = "http://" + r.Host + r.URL.Path + "?" + nq.Encode()
	}
	p.served++
	writeJSON(w, http.StatusOK, map[string]any{"count": len(p.devices), "next": next, "previous": nil, "results": results})
}

func genDevices(t *rapid.T, n int) []genDevice {
	statuses := []string{"active", "planned", "offline", "inventory", "decommissioning", "some-future-status"}
	out := make([]genDevice, n)
	for i := range n {
		d := genDevice{
			ID:         i*3 + 7,
			SerialMode: rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("d%d-serialmode", i)),
			Serial:     fmt.Sprintf("SN%04d", i),
			IPMode:     rapid.IntRange(0, 2).Draw(t, fmt.Sprintf("d%d-ipmode", i)),
			IP:         fmt.Sprintf("10.0.%d.%d/24", i/250, i%250+1),
			Status:     rapid.SampledFrom(statuses).Draw(t, fmt.Sprintf("d%d-status", i)),
		}
		if rapid.Bool().Draw(t, fmt.Sprintf("d%d-hastag", i)) {
			tag := fmt.Sprintf("AT%d", i)
			d.AssetTag = &tag
		}
		out[i] = d
	}
	return out
}

func checkDecoded(t *rapid.T, world []genDevice, got []Device) {
	if len(got) != len(world) {
		t.Fatalf("P1: got %d devices, want %d", len(got), len(world))
	}
	for i, w := range world {
		g := got[i]
		if g.ID != w.ID {
			t.Fatalf("P1: position %d: got id %d, want %d", i, g.ID, w.ID)
		}
		if g.Serial != w.wantSerial() {
			t.Fatalf("P2: device %d serial %q, want %q (mode %d)", w.ID, g.Serial, w.wantSerial(), w.SerialMode)
		}
		if (g.PrimaryIP4 != nil) != (w.IPMode == 0) {
			t.Fatalf("P2: device %d primary_ip4 presence %v, want %v", w.ID, g.PrimaryIP4 != nil, w.IPMode == 0)
		}
		if g.PrimaryIP4 != nil && g.PrimaryIP4.ID != w.ID+10000 {
			t.Fatalf("P2: device %d primary_ip4 id %d, want %d", w.ID, g.PrimaryIP4.ID, w.ID+10000)
		}
		if g.Status == nil || g.Status.Value != w.Status {
			t.Fatalf("P2: device %d status %+v, want %q", w.ID, g.Status, w.Status)
		}
		if (g.AssetTag == nil) != (w.AssetTag == nil) || (g.AssetTag != nil && *g.AssetTag != *w.AssetTag) {
			t.Fatalf("P2: device %d asset_tag %v, want %v", w.ID, g.AssetTag, w.AssetTag)
		}
	}
}

func TestPropListPagination(t *testing.T) {
	for _, base := range []string{"", "/netbox"} {
		t.Run("base="+base, func(t *testing.T) {
			ps := &pageServer{basePath: base}
			srv := httptest.NewServer(ps)
			defer srv.Close()
			c := quietClient(srv.URL + base)
			rapid.Check(t, func(t *rapid.T) {
				limit := rapid.IntRange(1, 6).Draw(t, "limit")
				// Bias sizes toward exact multiples of the page size.
				n := rapid.OneOf(
					rapid.IntRange(0, 20),
					rapid.Map(rapid.IntRange(0, 4), func(k int) int { return k * limit }),
				).Draw(t, "n")
				world := genDevices(t, n)
				ps.mu.Lock()
				ps.devices = world
				ps.faultReq, ps.faultSeq, ps.served, ps.attempts, ps.inFaultLeft = 0, nil, 0, 0, 0
				ps.mu.Unlock()
				var got []Device
				err := c.List(context.Background(), "/api/dcim/devices/", url.Values{"limit": {strconv.Itoa(limit)}}, &got)
				if err != nil {
					t.Fatalf("P1/P4: List failed (base path %q, n=%d, limit=%d): %v", base, n, limit, err)
				}
				checkDecoded(t, world, got)
			})
		})
	}
}

func TestPropListRetries(t *testing.T) {
	ps := &pageServer{}
	srv := httptest.NewServer(ps)
	defer srv.Close()
	c := quietClient(srv.URL)
	const documentedRetries = 3 // ClientConfig.MaxRetries default
	rapid.Check(t, func(t *rapid.T) {
		limit := rapid.IntRange(1, 4).Draw(t, "limit")
		n := rapid.IntRange(1, 12).Draw(t, "n")
		world := genDevices(t, n)
		pages := (n + limit - 1) / limit
		faultReq := rapid.IntRange(1, pages).Draw(t, "faultReq")
		seq := rapid.SliceOfN(rapid.SampledFrom([]int{429, 429, 429, 500, 503}), 1, 5).Draw(t, "faultSeq")
		ps.mu.Lock()
		ps.devices = world
		ps.faultReq, ps.faultSeq, ps.served, ps.attempts, ps.inFaultLeft = faultReq, seq, 0, 0, len(seq)
		ps.mu.Unlock()

		var got []Device
		err := c.List(context.Background(), "/api/dcim/devices/", url.Values{"limit": {strconv.Itoa(limit)}}, &got)

		// Oracle: the faulted logical request survives iff every injected
		// status before the first success is a 429 and there are at most
		// documentedRetries of them.
		n429 := 0
		non429 := false
		for _, st := range seq {
			if st != 429 {
				non429 = true
				break
			}
			n429++
		}
		wantOK := !non429 && n429 <= documentedRetries
		ps.mu.Lock()
		attempts := ps.attempts
		ps.mu.Unlock()
		if attempts > pages*(documentedRetries+1) {
			t.Fatalf("P3: %d requests for %d pages exceeds the retry bound", attempts, pages)
		}
		switch {
		case wantOK && err != nil:
			t.Fatalf("P3: %d consecutive 429s (within the documented %d retries) failed List: %v", n429, documentedRetries, err)
		case !wantOK && err == nil:
			t.Fatalf("P3: List succeeded despite fault sequence %v (non-429 or >%d 429s); got %d devices", seq, documentedRetries, len(got))
		case wantOK:
			checkDecoded(t, world, got)
		default:
			if got != nil {
				t.Fatalf("P3: failed List left a partial result of %d devices in dest", len(got))
			}
		}
	})
}

// FuzzDecodeDevice feeds arbitrary bytes through the Device and Interface
// decoders: decode may fail, but must not panic, and a successful decode of
// an object lacking "serial" must give "". Under plain go test it runs the
// seed corpus only.
func FuzzDecodeDevice(f *testing.F) {
	f.Add([]byte(`{"id":1,"serial":null,"primary_ip4":null,"status":{"value":"x"}}`))
	f.Add([]byte(`{"id":1,"oob_ip":{"id":2,"address":"10.0.0.1/24"},"interfaces":[{"id":3,"mac_addresses":[]}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		var d Device
		if err := json.Unmarshal(b, &d); err != nil {
			return
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal(b, &raw) == nil {
			if _, ok := raw["serial"]; !ok && d.Serial != "" {
				t.Fatalf("serial %q decoded from an object with no serial key", d.Serial)
			}
		}
		for _, i := range d.Interfaces {
			_ = i.GetMAC()
		}
	})
}
