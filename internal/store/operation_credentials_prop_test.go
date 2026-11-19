package store

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/proptest/paramsgen"
)

// Property coverage for ScrubTerminalOperationCredentials: the generated SQL
// (a jsonb path program) against paramsgen's reference model (a tree walk
// over its own AST, sharing no code with either the SQL builder or
// model.RedactParams).
//
// Each case seeds a batch of ops over a fixed machine pool, every op with a
// generated params document and a random status, runs the scrub, and checks:
//
//   - a terminal op's stored params mean exactly RefScrub(params);
//   - no canary planted at a walkable credential path remains in a terminal
//     op's stored bytes;
//   - an active op's stored params are untouched;
//   - the returned row count is the number of terminal ops the reference
//     says change, and a second run rewrites nothing;
//   - what the worker's decoder would read from a scrubbed terminal op
//     holds no credential;
//   - the Go redaction finds nothing left to redact in a scrubbed op of a
//     typed-shape document (the two walkers agree on the path set).
//
// One odd row must never fail the statement: the scrub erroring is a
// failure in every case.

const scrubPoolSize = 8

type scrubStats struct{ m map[string]int }

func (s *scrubStats) add(k string, n int) { s.m[k] += n }
func (s *scrubStats) report(t *testing.T) {
	keys := make([]string, 0, len(s.m))
	for k := range s.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%d", k, s.m[k])
	}
	t.Logf("coverage:%s", b.String())
}

type seededOp struct {
	id     uuid.UUID
	status model.OperationStatus
	doc    *paramsgen.Doc
	raw    []byte
}

func scrubProperty(t *testing.T, opts paramsgen.Options, typedShapes bool) {
	st := testStore(t)
	stats := &scrubStats{m: map[string]int{}}
	defer stats.report(t)

	machines := make([]uuid.UUID, scrubPoolSize)
	for i := range machines {
		m := &model.Machine{ID: uuid.New(), NetBoxSerial: fmt.Sprintf("SN-PROP-%d", i), State: model.MachineStateRunning}
		if err := st.CreateMachine(m); err != nil {
			t.Fatal(err)
		}
		machines[i] = m.ID
	}
	statuses := []model.OperationStatus{model.OpCompleted, model.OpFailed, model.OpCompleted, model.OpFailed,
		model.OpPending, model.OpClaimed, model.OpRunning}

	rapid.Check(t, func(rt *rapid.T) {
		if err := st.db.Exec("DELETE FROM operations").Error; err != nil {
			rt.Fatalf("reset operations: %v", err)
		}
		n := rapid.IntRange(1, scrubPoolSize).Draw(rt, "ops")
		var ops []seededOp
		for i := range n {
			d := paramsgen.Draw(rt, opts)
			status := rapid.SampledFrom(statuses).Draw(rt, "status")
			raw := d.Raw()
			op := &model.Operation{MachineID: machines[i], Type: model.OpPowerCycle, Status: status, Params: raw, Version: 3}
			if status == model.OpCompleted || status == model.OpFailed {
				done := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
				op.CompletedAt = &done
			}
			if err := st.CreateOperation(op); err != nil {
				rt.Fatalf("seed op with params %s: %v", raw, err)
			}
			ops = append(ops, seededOp{id: op.ID, status: status, doc: d, raw: raw})
			for k, c := range d.Features {
				stats.add(k, c)
			}
		}
		stats.add("cases", 1)
		stats.add("ops", n)

		affected, err := st.ScrubTerminalOperationCredentials()
		if err != nil {
			var all []string
			for _, o := range ops {
				all = append(all, string(o.raw))
			}
			rt.Fatalf("the scrub statement failed, so no terminal op in the table was scrubbed: %v\nparams:\n%s", err, strings.Join(all, "\n"))
		}

		wantAffected := int64(0)
		for _, o := range ops {
			got, err := st.GetOperation(o.id)
			if err != nil {
				rt.Fatal(err)
			}
			stored, err := paramsgen.Parse(got.Params)
			if err != nil {
				rt.Fatalf("stored params do not parse: %v: %s", err, got.Params)
			}
			terminal := o.status == model.OpCompleted || o.status == model.OpFailed
			before := paramsgen.Canon(paramsgen.Normalize(o.doc.Root))
			if !terminal {
				stats.add("active_ops", 1)
				if c := paramsgen.Canon(stored); c != before {
					rt.Fatalf("the scrub changed an active (%s) op's params\n  was %s\n  now %s", o.status, before, c)
				}
				continue
			}
			stats.add("terminal_ops", 1)
			want := paramsgen.Canon(paramsgen.RefScrub(o.doc.Root))
			if want != before {
				wantAffected++
				stats.add("terminal_ops_changed", 1)
			}
			if c := paramsgen.Canon(stored); c != want {
				rt.Fatalf("scrubbed params differ from the reference model\n   in %s\n  got %s\n want %s", o.raw, c, want)
			}
			for _, c := range o.doc.WalkableCanaries {
				stats.add("walkable_canaries", 1)
				if bytes.Contains(got.Params, []byte(c)) {
					rt.Fatalf("credential canary %s is still stored on a %s op\n   in %s\n  now %s", c, o.status, o.raw, got.Params)
				}
			}
			if p, err := model.DecodeParams(got.Params); err == nil {
				stats.add("terminal_decodable", 1)
				if p.BMCRuntime != nil && p.BMCRuntime.Password != "" ||
					p.AMTRuntime != nil && p.AMTRuntime.Password != "" ||
					p.TailscaleRuntime != nil && p.TailscaleRuntime.AuthKey != "" ||
					p.MonitorRuntime != nil && p.MonitorRuntime.Password != "" {
					rt.Fatalf("a scrubbed %s op still decodes to a credential\n   in %s\n  now %s", o.status, o.raw, got.Params)
				}
				if p.PDURuntime != nil {
					for _, out := range p.PDURuntime.Outlets {
						if out.Password != "" {
							rt.Fatalf("a scrubbed %s op still decodes to a PDU credential\n   in %s\n  now %s", o.status, o.raw, got.Params)
						}
					}
				}
			}
			red := model.RedactParams(got.Params)
			if !bytes.Equal(red, got.Params) {
				stats.add("redaction_still_finds_something_after_scrub", 1)
				if typedShapes {
					rt.Fatalf("the redaction still finds a credential path after the scrub; the two walkers disagree on the path set\n   in %s\n  now %s\n  red %s", o.raw, got.Params, red)
				}
			}
			if got.Status != o.status || got.Version != 3 || got.CompletedAt == nil {
				rt.Fatalf("the scrub touched a column other than params: status=%s version=%d completed_at=%v", got.Status, got.Version, got.CompletedAt)
			}
		}
		if affected != wantAffected {
			rt.Fatalf("scrub reported %d rewritten rows, the reference model says %d", affected, wantAffected)
		}
		again, err := st.ScrubTerminalOperationCredentials()
		if err != nil {
			rt.Fatalf("second scrub: %v", err)
		}
		if again != 0 {
			rt.Fatalf("second scrub rewrote %d rows; it must be idempotent", again)
		}
	})
}

// TestProp_Scrub_TypedShapes: documents shaped like the typed union's output
// (plus unknown keys and every odd credential value).
func TestProp_Scrub_TypedShapes(t *testing.T) {
	scrubProperty(t, paramsgen.Options{}, true)
}

// TestProp_Scrub_OddShapes adds wrong-kind values on the credential paths,
// duplicate keys (which jsonb collapses) and non-object params.
func TestProp_Scrub_OddShapes(t *testing.T) {
	scrubProperty(t, paramsgen.Options{Malformed: true, NonObjectRoot: true, DupKeys: true}, false)
}

// TestProp_Scrub_CaseVariantKeys: encoding/json matches field names
// case-insensitively, while the scrub's jsonb paths are exact. Whatever the
// key spelling, a scrubbed terminal op must not decode to a credential
// (DecodeParams refuses a case-variant credential key outright).
func TestProp_Scrub_CaseVariantKeys(t *testing.T) {
	st := testStore(t)
	m := &model.Machine{ID: uuid.New(), NetBoxSerial: "SN-PROP-CASE", State: model.MachineStateRunning}
	if err := st.CreateMachine(m); err != nil {
		t.Fatal(err)
	}
	rapid.Check(t, func(rt *rapid.T) {
		if err := st.db.Exec("DELETE FROM operations").Error; err != nil {
			rt.Fatal(err)
		}
		d := paramsgen.Draw(rt, paramsgen.Options{CaseKeys: true, TypedValues: true})
		op := &model.Operation{MachineID: m.ID, Type: model.OpPowerCycle, Status: model.OpCompleted, Params: d.Raw()}
		if err := st.CreateOperation(op); err != nil {
			rt.Fatal(err)
		}
		if _, err := st.ScrubTerminalOperationCredentials(); err != nil {
			rt.Fatal(err)
		}
		got, err := st.GetOperation(op.ID)
		if err != nil {
			rt.Fatal(err)
		}
		p, err := model.DecodeParams(got.Params)
		if err != nil {
			return
		}
		for _, c := range d.Canaries {
			held := p.BMCRuntime != nil && strings.Contains(p.BMCRuntime.Password, c) ||
				p.AMTRuntime != nil && strings.Contains(p.AMTRuntime.Password, c) ||
				p.TailscaleRuntime != nil && strings.Contains(p.TailscaleRuntime.AuthKey, c) ||
				p.MonitorRuntime != nil && strings.Contains(p.MonitorRuntime.Password, c)
			if p.PDURuntime != nil {
				for _, o := range p.PDURuntime.Outlets {
					held = held || strings.Contains(o.Password, c)
				}
			}
			if held {
				rt.Fatalf("a scrubbed terminal op still decodes to credential %s\n   in %s\n  now %s", c, d.Raw(), got.Params)
			}
		}
	})
}
