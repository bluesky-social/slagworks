package store

// Model-based property test for the DHCP lease-history ledger fold
// (RecordDHCPLeaseEvent), ListDHCPLeaseHistory and PruneDHCPLeaseLedger.
//
// The reference model is a plain slice of rows folded by the three rules in
// RecordDHCPLeaseEvent's contract. Events come from small pools (2 sites, 2
// workers, 4 MACs, 4 addresses) so supersede-by-MAC, supersede-by-IP,
// re-open after close, replayed deletes and cross-site/worker isolation all
// interact, and a third of the steps replay an earlier event (the outbox is
// at-least-once).
//
// The ledger table is emptied between rapid cases; one Postgres container
// serves the run.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

type ledgerEvent struct {
	site, worker, event, mac, ip, kind, pool string
	machine                                  *uuid.UUID
	expires                                  time.Time
}

type ledgerRow struct {
	site, worker, mac, ip, kind, pool string
	machine                           string
	expires                           time.Time
	open                              bool
	reason                            string
}

func (r ledgerRow) String() string {
	state := "open"
	if !r.open {
		state = "closed:" + r.reason
	}
	return fmt.Sprintf("%s/%s %s %s %s %s m=%s exp=%s %s", r.site, r.worker, r.mac, r.ip, r.kind, r.pool, r.machine, r.expires.UTC().Format(time.RFC3339Nano), state)
}

type ledgerModel struct{ rows []*ledgerRow }

func (m *ledgerModel) fold(e ledgerEvent) error {
	switch e.event {
	case "allocate", "renew", "delete":
	default:
		return fmt.Errorf("unknown event")
	}
	machine := ""
	if e.machine != nil {
		machine = e.machine.String()
	}
	var same *ledgerRow
	for _, r := range m.rows {
		if !r.open || r.site != e.site || r.worker != e.worker {
			continue
		}
		macMatch, ipMatch := r.mac == e.mac, r.ip == e.ip
		switch {
		case macMatch && ipMatch:
			same = r
		case e.event != "delete" && (macMatch || ipMatch):
			r.open, r.reason = false, "superseded"
		}
	}
	if e.event == "delete" {
		if same != nil {
			same.open, same.reason = false, "deleted"
		}
		return nil
	}
	if same != nil {
		same.expires, same.kind, same.pool, same.machine = e.expires, e.kind, e.pool, machine
		return nil
	}
	m.rows = append(m.rows, &ledgerRow{site: e.site, worker: e.worker, mac: e.mac, ip: e.ip, kind: e.kind, pool: e.pool, machine: machine, expires: e.expires, open: true})
	return nil
}

func ledgerRowOf(e model.DHCPLeaseLedgerEntry) ledgerRow {
	r := ledgerRow{site: e.SiteSlug, worker: e.WorkerID, mac: e.MAC, ip: e.IP, kind: e.Kind, pool: e.Pool, expires: e.ExpiresAt, open: e.ReleasedAt == nil, reason: e.ReleaseReason}
	if e.MachineID != nil {
		r.machine = e.MachineID.String()
	}
	return r
}

func sortedRows[T any](rows []T, str func(T) string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, str(r))
	}
	sort.Strings(out)
	return out
}

func TestProp_DHCPLedgerFoldMatchesModel(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	base := time.Now().Add(time.Hour).Truncate(time.Second)
	machines := []*uuid.UUID{nil, ptrUUID(uuid.MustParse("11111111-1111-1111-1111-111111111111")), ptrUUID(uuid.MustParse("22222222-2222-2222-2222-222222222222"))}
	stats := map[string]int{}

	rapid.Check(t, func(rt *rapid.T) {
		if err := st.db.Exec("DELETE FROM dhcp_lease_ledger").Error; err != nil {
			rt.Fatalf("clear ledger: %v", err)
		}
		m := &ledgerModel{}
		var history []ledgerEvent

		steps := rapid.IntRange(1, 24).Draw(rt, "steps")
		for step := 0; step < steps; step++ {
			var e ledgerEvent
			if len(history) > 0 && rapid.IntRange(0, 2).Draw(rt, "replay") == 0 {
				e = history[rapid.IntRange(0, len(history)-1).Draw(rt, "replayIndex")]
				stats["replayed event"]++
			} else {
				e = ledgerEvent{
					site:    rapid.SampledFrom([]string{"pop-a", "pop-b"}).Draw(rt, "site"),
					worker:  rapid.SampledFrom([]string{"w1", "w2"}).Draw(rt, "worker"),
					event:   rapid.SampledFrom([]string{"allocate", "allocate", "renew", "renew", "delete", "release", ""}).Draw(rt, "event"),
					mac:     fmt.Sprintf("aa:bb:cc:00:00:%02x", rapid.IntRange(1, 4).Draw(rt, "mac")),
					ip:      fmt.Sprintf("10.9.0.%d", rapid.IntRange(10, 13).Draw(rt, "ip")),
					kind:    rapid.SampledFrom([]string{"dynamic", "static"}).Draw(rt, "kind"),
					pool:    rapid.SampledFrom([]string{"10.9.0.0/24", "none", ""}).Draw(rt, "pool"),
					machine: rapid.SampledFrom(machines).Draw(rt, "machine"),
					expires: base.Add(time.Duration(rapid.IntRange(-7200, 7200).Draw(rt, "expiresOffset")) * time.Second),
				}
			}
			if e.event == "delete" || e.event == "renew" {
				// Aim most deletes and renews at a lease the ledger has open.
				var open []*ledgerRow
				for _, r := range m.rows {
					if r.open {
						open = append(open, r)
					}
				}
				if len(open) > 0 && rapid.IntRange(0, 2).Draw(rt, "targetOpen") > 0 {
					r := open[rapid.IntRange(0, len(open)-1).Draw(rt, "openIndex")]
					e.site, e.worker, e.mac, e.ip = r.site, r.worker, r.mac, r.ip
				}
			}
			history = append(history, e)

			before := len(m.rows)
			openBefore := 0
			for _, r := range m.rows {
				if r.open {
					openBefore++
				}
			}
			wantErr := m.fold(e)
			err := st.RecordDHCPLeaseEvent(ctx, e.site, e.worker, e.event, e.mac, e.ip, e.kind, e.pool, e.machine, e.expires)
			if (err != nil) != (wantErr != nil) {
				rt.Fatalf("step %d %+v: error %v, model error %v", step, e, err, wantErr)
			}
			stats["event "+e.event]++
			openAfter := 0
			for _, r := range m.rows {
				if r.open {
					openAfter++
				}
			}
			switch {
			case err != nil:
				stats["rejected unknown event"]++
			case len(m.rows) > before && openAfter <= openBefore:
				stats["supersede + insert"]++
			case len(m.rows) > before:
				stats["insert"]++
			case openAfter < openBefore:
				stats["close"]++
			default:
				stats["bump or no-op"]++
			}

			// readLedger returns every stored row after checking the table
			// against the model.
			readLedger := func(after string) []model.DHCPLeaseLedgerEntry {
				var got []model.DHCPLeaseLedgerEntry
				if err := st.db.Order("id").Find(&got).Error; err != nil {
					rt.Fatalf("read ledger: %v", err)
				}
				gotRows := sortedRows(got, func(e model.DHCPLeaseLedgerEntry) string { return ledgerRowOf(e).String() })
				wantRows := sortedRows(m.rows, func(r *ledgerRow) string { return r.String() })
				if strings.Join(gotRows, "\n") != strings.Join(wantRows, "\n") {
					rt.Fatalf("step %d after %s:\n store:\n  %s\n model:\n  %s", step, after, strings.Join(gotRows, "\n  "), strings.Join(wantRows, "\n  "))
				}
				return got
			}
			got := readLedger(fmt.Sprintf("%+v", e))
			openMAC, openIP := map[string]bool{}, map[string]bool{}
			for _, r := range got {
				if r.FirstSeen.After(r.LastSeen) {
					rt.Fatalf("step %d: row %d first_seen after last_seen", step, r.ID)
				}
				if r.ReleasedAt == nil {
					if r.ReleaseReason != "" {
						rt.Fatalf("step %d: open row %d has a release reason", step, r.ID)
					}
					km, ki := r.SiteSlug+"|"+r.WorkerID+"|"+r.MAC, r.SiteSlug+"|"+r.WorkerID+"|"+r.IP
					if openMAC[km] || openIP[ki] {
						rt.Fatalf("step %d: two open rows share a MAC or an IP: %+v", step, got)
					}
					openMAC[km], openIP[ki] = true, true
				} else if r.ReleasedAt.Before(r.FirstSeen) || (r.ReleaseReason != "superseded" && r.ReleaseReason != "deleted") {
					rt.Fatalf("step %d: closed row %d is malformed: %+v", step, r.ID, r)
				}
			}

			// History read: site-scoped (no other site's rows), filtered,
			// newest first, limited.
			if rapid.IntRange(0, 3).Draw(rt, "list") == 0 {
				site := rapid.SampledFrom([]string{"pop-a", "pop-b", "pop-c"}).Draw(rt, "listSite")
				mac := rapid.SampledFrom([]string{"", "", "aa:bb:cc:00:00:01", "aa:bb:cc:00:00:02"}).Draw(rt, "listMAC")
				ip := rapid.SampledFrom([]string{"", "", "10.9.0.10", "10.9.0.11"}).Draw(rt, "listIP")
				limit := rapid.SampledFrom([]int{-1, 0, 1, 2, 100, 5000}).Draw(rt, "limit")
				rows, err := st.ListDHCPLeaseHistory(ctx, site, mac, ip, limit)
				if err != nil {
					rt.Fatalf("list: %v", err)
				}
				want := 0
				for _, r := range got {
					if r.SiteSlug == site && (mac == "" || r.MAC == mac) && (ip == "" || r.IP == ip) {
						want++
					}
				}
				capN := limit
				if capN <= 0 {
					capN = 100
				}
				if capN > 1000 {
					capN = 1000
				}
				if want > capN {
					want = capN
				}
				if len(rows) != want {
					rt.Fatalf("step %d: list(%s,%q,%q,%d) returned %d rows, want %d", step, site, mac, ip, limit, len(rows), want)
				}
				for i, r := range rows {
					if r.SiteSlug != site || (mac != "" && r.MAC != mac) || (ip != "" && r.IP != ip) {
						rt.Fatalf("step %d: list(%s,%q,%q) returned a row outside its scope: %+v", step, site, mac, ip, r)
					}
					if i > 0 && rows[i-1].LastSeen.Before(r.LastSeen) {
						rt.Fatalf("step %d: history not newest-first", step)
					}
				}
				stats["list"]++
			}

			// Retention: closed rows older than the cutoff go, open rows
			// never do.
			if rapid.IntRange(0, 9).Draw(rt, "prune") == 0 {
				future := rapid.Bool().Draw(rt, "pruneEverythingClosed")
				cutoff := time.Now().Add(-time.Hour)
				if future {
					cutoff = time.Now().Add(time.Hour)
				}
				n, err := st.PruneDHCPLeaseLedger(ctx, cutoff)
				if err != nil {
					rt.Fatalf("prune: %v", err)
				}
				closed := 0
				kept := m.rows[:0]
				for _, r := range m.rows {
					if !r.open {
						closed++
						if future {
							continue
						}
					}
					kept = append(kept, r)
				}
				m.rows = kept
				if (future && int(n) != closed) || (!future && n != 0) {
					rt.Fatalf("step %d: prune(future=%v) removed %d rows, %d were closed", step, future, n, closed)
				}
				readLedger(fmt.Sprintf("prune(future=%v)", future))
				stats["prune"]++
			}
		}
	})
	keys := make([]string, 0, len(stats))
	for k := range stats {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-26s %d\n", k, stats[k])
	}
	t.Logf("coverage:\n%s", b.String())
}

func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }
