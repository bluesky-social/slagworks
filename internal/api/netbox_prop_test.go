package api

// NetBox-backed properties: how identifiers resolve to machines and devices
// (and what happens when one is ambiguous), the hostname/serial/state list
// filters against brute-force filtering, and hostname enrichment.

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

type nrDevice struct {
	dev     netbox.Device
	machine *model.Machine // nil: not a slagworks machine
}

// TestProp_NameResolution generates a small NetBox whose device names and
// serials come from tiny pools (so several devices share a name across
// sites, or a serial), binds some devices to machines, and checks every
// identifier-taking read against a reference resolution written from
// docs/dev/api.md.
func TestProp_NameResolution(t *testing.T) {
	stats := newPropStats()
	defer stats.report(t)

	propCheck(t, 30, func(rt *rapid.T) {
		nb := newPropNetBox()
		secrets := &awssecrets.FakeReader{Secrets: map[string]*awssecrets.AdminSecret{
			"pop-a": {BySerial: map[string]awssecrets.AdminHost{}},
			"pop-b": {BySerial: map[string]awssecrets.AdminHost{}},
		}}
		env := newPropEnv(t, func(cfg *Config) {
			cfg.NetBox = nb.client(t)
			cfg.BMCSecrets = secrets
		})

		// NetBox enforces name uniqueness per site, not globally: draw each
		// site's names without repetition from one shared pool.
		namePool := []string{"node-1.rack", "node-2.rack", "pdu-left", "101"}
		serialPool := []string{"SN-0", "SN-1", "SN-2", "SN-0", "pdu-left", ""}
		var devs []*nrDevice
		id := 100
		machineSerials := map[string]bool{}
		for _, site := range []string{"pop-a", "pop-b"} {
			names := rapid.SliceOfNDistinct(rapid.SampledFrom(namePool), 0, 3, rapid.ID[string]).Draw(rt, "names_"+site)
			for _, name := range names {
				id++
				d := &nrDevice{dev: netbox.Device{
					ID: id, Name: name,
					Serial: rapid.SampledFrom(serialPool).Draw(rt, fmt.Sprintf("serial%d", id)),
					Site:   &netbox.NestedRef{ID: 1, Name: site, Slug: site},
				}}
				// A machine row needs a non-empty, unique serial.
				if d.dev.Serial != "" && !machineSerials[d.dev.Serial] && rapid.IntRange(0, 3).Draw(rt, fmt.Sprintf("machine%d", id)) > 0 {
					devID := d.dev.ID
					d.machine = &model.Machine{
						ID: uuid.New(), NetBoxSerial: d.dev.Serial, NetBoxDeviceID: &devID,
						State: rapid.SampledFrom([]model.MachineState{model.MachineStateNew, model.MachineStateCommissioned, model.MachineStateRunning}).Draw(rt, fmt.Sprintf("state%d", id)),
					}
					machineSerials[d.dev.Serial] = true
					if err := env.st.CreateMachine(d.machine); err != nil {
						rt.Fatalf("seed machine: %v", err)
					}
				}
				if d.dev.Serial != "" && rapid.IntRange(0, 4).Draw(rt, fmt.Sprintf("cred%d", id)) > 0 {
					secrets.Secrets[site].BySerial[d.dev.Serial] = awssecrets.AdminHost{
						Kind: awssecrets.KindBMC, Username: "admin-" + site, Password: fmt.Sprintf("pw-%s-%d", site, id),
					}
				}
				devs = append(devs, d)
			}
		}
		// NetBox answers in its own order; resolution must not depend on it.
		order := rapid.Permutation(devs).Draw(rt, "netboxOrder")
		for _, d := range order {
			nb.devices = append(nb.devices, d.dev)
		}
		byName := func(name string) []*nrDevice {
			var out []*nrDevice
			for _, d := range order {
				if d.dev.Name == name {
					out = append(out, d)
				}
			}
			return out
		}

		// (1) /machines/:id by hostname. docs/dev/api.md: "accepts either the
		// slagworks machine UUID or the NetBox device name (anything
		// containing a .)".
		for _, name := range []string{"node-1.rack", "node-2.rack", "node-9.rack"} {
			resp := env.get("/api/v1/machines/" + name)
			match := byName(name)
			switch {
			case len(match) == 0:
				if resp.code != http.StatusNotFound {
					rt.Fatalf("GET /machines/%s: %s, want 404 (no such device)", name, resp)
				}
			case len(match) > 1:
				// Two devices answer to this name. Picking one silently
				// would let a deploy/abort/power by name land on a host in
				// another site; it is refused, as an ambiguous serial is.
				if resp.code != http.StatusConflict {
					rt.Fatalf("GET /machines/%s: %s, but %d NetBox devices are named %q (ids %d and %d); want 409, not a silent pick",
						name, resp, len(match), name, match[0].dev.ID, match[1].dev.ID)
				}
				stats.add("machine name ambiguity refused")
			default:
				first := match[0]
				if first.machine == nil {
					if resp.code != http.StatusNotFound {
						rt.Fatalf("GET /machines/%s: %s, want 404 (device %d is not a machine)", name, resp, first.dev.ID)
					}
					break
				}
				var got model.Machine
				if resp.code != http.StatusOK || resp.json(&got) != nil || got.ID != first.machine.ID || got.Hostname != name {
					rt.Fatalf("GET /machines/%s: %s, want machine %s", name, resp, first.machine.ID)
				}
				stats.add("machine resolved by hostname")
			}
		}

		// (2) /devices/:ident/creds: "a NetBox device id, name or serial,
		// tried in that order"; "409 when a serial matches several devices".
		idents := []string{"node-1.rack", "pdu-left", "101", "102", "SN-0", "SN-1", "nope"}
		for _, ident := range idents {
			resp := env.do(propReq{method: http.MethodGet, path: "/api/v1/devices/" + ident + "/creds", token: propCredToken})
			var target *nrDevice
			code := 0
			if n, err := strconv.Atoi(ident); err == nil {
				for _, d := range order {
					if d.dev.ID == n {
						target = d
					}
				}
			}
			if target == nil {
				switch m := byName(ident); {
				case len(m) == 1:
					target = m[0]
				case len(m) > 1:
					code = http.StatusConflict
					stats.add("device name ambiguity refused")
				}
			}
			if target == nil && code == 0 {
				var m []*nrDevice
				for _, d := range order {
					if d.dev.Serial == ident {
						m = append(m, d)
					}
				}
				switch len(m) {
				case 0:
					code = http.StatusNotFound
				case 1:
					target = m[0]
				default:
					code = http.StatusConflict
					stats.add("device serial ambiguity refused")
				}
			}
			if target != nil {
				entry, ok := secrets.Secrets[target.dev.Site.Slug].BySerial[target.dev.Serial]
				if target.dev.Serial == "" || !ok {
					code = http.StatusNotFound
				} else {
					var got DeviceCreds
					if resp.code != http.StatusOK || resp.json(&got) != nil {
						rt.Fatalf("GET /devices/%s/creds: %s, want 200 for device %d", ident, resp, target.dev.ID)
					}
					if got.NetBoxID != target.dev.ID || got.Password != entry.Password || got.Pop != target.dev.Site.Slug {
						rt.Fatalf("GET /devices/%s/creds returned device %d (pop %s) credentials, want device %d (pop %s)",
							ident, got.NetBoxID, got.Pop, target.dev.ID, target.dev.Site.Slug)
					}
					stats.add("device creds resolved")
					continue
				}
			}
			if resp.code != code {
				rt.Fatalf("GET /devices/%s/creds: %s, want %d", ident, resp, code)
			}
		}

		// (3) list filters against a brute-force filter of the generated
		// world. hostname is a case-insensitive substring of the device name.
		pattern := rapid.SampledFrom([]string{"node", "NODE", "1", "rack", ".", "pdu", "zzz"}).Draw(rt, "pattern")
		state := rapid.SampledFrom([]string{"", "", "new", "commissioned", "running"}).Draw(rt, "state")
		serial := rapid.SampledFrom([]string{"", "", "", "SN-0", "SN-1"}).Draw(rt, "serial")
		path := "/api/v1/machines?hostname=" + pattern
		if state != "" {
			path += "&state=" + state
		}
		if serial != "" {
			path += "&serial=" + serial
		}
		resp := env.get(path)
		want := map[uuid.UUID]string{}
		for _, d := range order {
			if d.machine == nil || !strings.Contains(strings.ToLower(d.dev.Name), strings.ToLower(pattern)) {
				continue
			}
			if (state != "" && string(d.machine.State) != state) || (serial != "" && d.machine.NetBoxSerial != serial) {
				continue
			}
			want[d.machine.ID] = d.dev.Name
		}
		var got []model.Machine
		if resp.code != http.StatusOK || resp.json(&got) != nil {
			rt.Fatalf("GET %s: %s", path, resp)
		}
		if len(got) != len(want) {
			rt.Fatalf("GET %s: %d rows, brute force finds %d (%v)", path, len(got), len(want), want)
		}
		for _, g := range got {
			if name, ok := want[g.ID]; !ok || g.Hostname != name {
				rt.Fatalf("GET %s: row %s hostname %q, brute force expects %q (present=%v)", path, g.ID, g.Hostname, name, ok)
			}
		}
		stats.add(fmt.Sprintf("hostname filter rows=%d", len(got)))

		// (4) the unfiltered list carries every bound machine's hostname.
		resp = env.get("/api/v1/machines")
		got = nil
		if resp.code != http.StatusOK || resp.json(&got) != nil {
			rt.Fatalf("GET /machines: %s", resp)
		}
		names := map[uuid.UUID]string{}
		for _, d := range order {
			if d.machine != nil {
				names[d.machine.ID] = d.dev.Name
			}
		}
		if len(got) != len(names) {
			rt.Fatalf("GET /machines: %d rows, want %d", len(got), len(names))
		}
		for _, g := range got {
			if g.Hostname != names[g.ID] {
				rt.Fatalf("GET /machines: machine %s hostname %q, want %q", g.ID, g.Hostname, names[g.ID])
			}
		}
		keys := make([]string, 0, len(names))
		for _, n := range names {
			keys = append(keys, n)
		}
		sort.Strings(keys)
		stats.add(fmt.Sprintf("machines bound=%d", len(names)))
		stats.add("cases")
	})
}
