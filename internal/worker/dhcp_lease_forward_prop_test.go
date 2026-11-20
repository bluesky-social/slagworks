package worker

// Property test for DHCPLeaseForwarder: random allocate/renew/delete event
// streams over a small (MAC, IP) pool, with expiries far enough out that
// every renew debounce window spans the whole case. Reference model: a renew
// for a key is emitted iff no renew for that key was emitted since the last
// allocate/delete of the same key; allocate and delete always emit. The
// emitted observation must carry the event's MAC/IP/pool/expiry, kind static
// iff the MAC's reservation equals the leased IP, and the machine id of a
// MAC that resolves to a machine (uuid.Nil otherwise). Observations go
// through a real LocalDB so the payload also round-trips SQLite.

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/dhcp"
	"github.com/bluesky-social/slagworks/internal/model"
)

func TestDHCPLeaseForwarder_Model_Prop(t *testing.T) {
	db := openPropDB(t)
	defer func() { closeQuiet(db) }()
	macs := []string{"aa:bb:cc:00:00:01", "aa:bb:cc:00:00:02", "aa:bb:cc:00:00:03"}
	ips := []string{"10.9.0.10", "10.9.0.11", "10.9.0.12"}
	machineA := uuid.New()
	rapid.Check(t, func(rt *rapid.T) {
		// Each case starts from an empty outbox.
		for _, p := range mustListOutbox(rt, db) {
			if err := db.AckOutboxRow(p.RowID); err != nil {
				rt.Fatalf("clear outbox: %v", err)
			}
		}
		c := NewCache(db)
		// macs[0] belongs to a machine reserved at ips[0]; macs[1] has an
		// OOB-only reservation at ips[1]; macs[2] is unknown.
		c.ApplySnapshot(&model.SiteSnapshot{
			SeqNum: 1,
			Machines: []model.MachineSnapshot{{
				ID: machineA, State: model.MachineStateRunning,
				Interfaces: []model.SnapshotInterface{{Name: "eno1", MAC: macs[0], Type: "physical", IP: ips[0] + "/24"}},
			}},
			Reservations: []model.Reservation{{MAC: macs[1], IP: ips[1]}},
		})
		f := NewDHCPLeaseForwarder(c, 10*time.Minute)

		emittedRenew := map[string]bool{}
		type want struct {
			event, mac, ip, pool, expires string
		}
		var wantObs []want
		n := rapid.IntRange(1, 40).Draw(rt, "n")
		for i := 0; i < n; i++ {
			ev := rapid.SampledFrom([]string{"allocate", "renew", "renew", "renew", "delete"}).Draw(rt, "event")
			mac := rapid.SampledFrom(macs).Draw(rt, "mac")
			ip := rapid.SampledFrom(ips).Draw(rt, "ip")
			pool := rapid.SampledFrom([]string{"", "10.9.0.0/24", "oob"}).Draw(rt, "pool")
			exp := time.Now().Add(time.Duration(rapid.IntRange(60, 600).Draw(rt, "expMin")) * time.Minute)
			hw, _ := net.ParseMAC(mac)
			key := mac + "|" + ip
			emit := true
			switch ev {
			case "renew":
				emit = !emittedRenew[key]
				emittedRenew[key] = true
			default:
				delete(emittedRenew, key)
			}
			if emit {
				wantObs = append(wantObs, want{ev, mac, ip, pool, exp.UTC().Format(time.RFC3339)})
			}
			f.OnLeaseEvent(dhcp.LeaseEvent{Event: ev, MAC: hw, IP: net.ParseIP(ip), Expiry: exp, Pool: pool})
		}

		// Read back through a restart so the payload round-trips SQLite.
		db = reopenLocalDB(rt, db)
		pend := mustListOutbox(rt, db)
		if len(pend) != len(wantObs) {
			rt.Fatalf("forwarder emitted %d observations, model expects %d", len(pend), len(wantObs))
		}
		for i, p := range pend {
			var d dhcpLeaseObservation
			if err := json.Unmarshal(p.Obs.Data, &d); err != nil {
				rt.Fatalf("obs %d: undecodable data %q: %v", i, p.Obs.Data, err)
			}
			w := wantObs[i]
			if p.Obs.Event != "dhcp_lease" || d.Event != w.event || d.MAC != w.mac || d.IP != w.ip || d.Pool != w.pool || d.ExpiresAt != w.expires {
				rt.Fatalf("obs %d = %s %+v, want %+v", i, p.Obs.Event, d, w)
			}
			wantKind := "dynamic"
			if (w.mac == macs[0] && w.ip == ips[0]) || (w.mac == macs[1] && w.ip == ips[1]) {
				wantKind = "static"
			}
			if d.Kind != wantKind {
				rt.Fatalf("obs %d kind %q, want %q (%s on %s)", i, d.Kind, wantKind, w.mac, w.ip)
			}
			wantMachine := uuid.Nil
			if w.mac == macs[0] {
				wantMachine = machineA
			}
			if p.Obs.MachineID != wantMachine {
				rt.Fatalf("obs %d machine %s, want %s", i, p.Obs.MachineID, wantMachine)
			}
		}
	})
}
