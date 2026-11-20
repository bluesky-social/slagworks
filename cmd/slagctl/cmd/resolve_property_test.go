package cmd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/cmd/slagctl/cmd"
	"github.com/bluesky-social/slagworks/internal/model"
)

// TestResolveMachine_Property_IdentifiesExactlyOne: resolveMachine picks a
// machine only when the argument identifies exactly one, per the documented
// shapes (docs/user/operations.md "Argument shapes"; resolve.go): UUID, exact
// NetBox serial, exact hostname, or a unique hostname substring. Anything that
// matches more than one machine at the deciding tier is an error, never a
// silent pick: every machine command (deploy --destructive included) acts on
// the result.
//
// The stub server implements the server's documented filters (api.md:
// ?serial= exact, ?hostname= case-insensitive substring of the NetBox name)
// over a small generated fleet. Fleet names come from a tiny pool with case
// variants and repeats so collisions are common; NetBox device names are
// unique only per site, so the same name at two sites is a legal state.
func TestResolveMachine_Property_IdentifiesExactlyOne(t *testing.T) {
	var mu sync.Mutex
	var fleet []model.Machine
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		q := r.URL.Query()
		out := []model.Machine{}
		for _, m := range fleet {
			if s := q.Get("serial"); s != "" && m.NetBoxSerial != s {
				continue
			}
			if h := q.Get("hostname"); h != "" && !strings.Contains(strings.ToLower(m.Hostname), strings.ToLower(h)) {
				continue
			}
			out = append(out, m)
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	cl := testServer(t, mux)

	rapid.Check(t, func(t *rapid.T) {
		names := []string{"cpu1.pop1", "CPU1.pop1", "cpu10.pop1", "cpu1.pop2", "db1", "db1", "db11", ""}
		serials := []string{"S1", "S2", "S3", "S1", "cpu1.pop1", "s1"}
		n := rapid.IntRange(1, 5).Draw(t, "n")
		ms := make([]model.Machine, n)
		for i := range ms {
			ms[i] = model.Machine{
				ID:           uuid.New(),
				Hostname:     rapid.SampledFrom(names).Draw(t, fmt.Sprintf("name%d", i)),
				NetBoxSerial: rapid.SampledFrom(serials).Draw(t, fmt.Sprintf("serial%d", i)),
			}
		}
		mu.Lock()
		fleet = ms
		mu.Unlock()
		argPool := append(append([]string{}, names...), serials...)
		argPool = append(argPool, "cpu1", "pop1", "CPU1", "db", "zzz", " db1 ", "DB1", "Cpu1.Pop1")
		arg := rapid.SampledFrom(argPool).Draw(t, "arg")

		// Reference resolution.
		a := strings.TrimSpace(arg)
		var want []uuid.UUID
		tier := ""
		if a != "" {
			var bySerial []uuid.UUID
			for _, m := range ms {
				if m.NetBoxSerial == a {
					bySerial = append(bySerial, m.ID)
				}
			}
			if len(bySerial) > 0 {
				want, tier = bySerial, "serial"
			} else {
				var exact, sub []uuid.UUID
				for _, m := range ms {
					if strings.EqualFold(m.Hostname, a) {
						exact = append(exact, m.ID)
					}
					if strings.Contains(strings.ToLower(m.Hostname), strings.ToLower(a)) {
						sub = append(sub, m.ID)
					}
				}
				if len(exact) > 0 {
					want, tier = exact, "exact"
				} else {
					want, tier = sub, "substring"
				}
			}
		}

		got, err := cmd.ResolveMachine(context.Background(), cl, arg)
		if len(want) == 1 {
			if err != nil {
				t.Fatalf("arg %q identifies %s by %s; got error %v (fleet %s)", arg, want[0], tier, err, describeFleet(ms))
			}
			if got.ID != want[0] {
				t.Fatalf("arg %q resolved to %s, want %s (tier %s)", arg, got.ID, want[0], tier)
			}
			return
		}
		if err == nil {
			t.Fatalf("arg %q matches %d machines at the %s tier (%v) but resolved silently to %s; fleet %s",
				arg, len(want), tier, want, got.ID, describeFleet(ms))
		}
	})
}

func describeFleet(ms []model.Machine) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "[%s host=%q serial=%q] ", m.ID.String()[:8], m.Hostname, m.NetBoxSerial)
	}
	return b.String()
}
