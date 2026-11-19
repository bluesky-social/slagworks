package power

// Model-based property test for PDUController: a chassis with 1..3
// switched outlets (possibly two on the same PDU), whose host is powered
// while any outlet is on, driven through a fake pduConn with a generated
// fault schedule. The oracle reads the chassis model, never the
// controller's own status aggregation.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pgregory.net/rapid"
)

const (
	pduFaultCLIBefore    = "cli-before"    // command not applied, error
	pduFaultCLIAfter     = "cli-after"     // command applied, output lost (error)
	pduFaultStateErr     = "state-err"     // Redfish read fails
	pduFaultStateUnknown = "state-unknown" // Redfish read succeeds, unrecognized SwitchedState
)

type pduOutletModel struct {
	on        bool
	offPolls  int  // reads remaining before a dispatched cycle restores power
	cycling   bool // an olDlyReboot is in progress in firmware
	stuck     bool // firmware never restores this outlet after a cycle
	rbootTime int
}

type pduChassis struct {
	outlets map[string]*pduOutletModel // "host#n"
	dropped []string                   // "host command" lines that failed before applying
	faults  map[int]string
	calls   int
	log     []string
}

func (c *pduChassis) poweredTruth() bool {
	for _, o := range c.outlets {
		if o.on {
			return true
		}
	}
	return false
}

type pduPropConn struct {
	host string
	ch   *pduChassis
}

func (f *pduPropConn) next() string {
	k := f.ch.calls
	f.ch.calls++
	return f.ch.faults[k]
}

func (f *pduPropConn) cli(_ context.Context, command string) (string, error) {
	fault := f.next()
	f.ch.log = append(f.ch.log, f.host+" "+command)
	if fault == pduFaultCLIBefore {
		f.ch.dropped = append(f.ch.dropped, f.host+" "+command)
		return "", fmt.Errorf("pdu %s %q: no output (transport failure)", f.host, command)
	}
	var verb string
	var n, v int
	fields := strings.Fields(command)
	verb = fields[0]
	n, _ = strconv.Atoi(fields[1])
	if len(fields) > 2 {
		v, _ = strconv.Atoi(fields[2])
	}
	o := f.ch.outlets[fmt.Sprintf("%s#%d", f.host, n)]
	if o == nil {
		return "E102: Parameter Error", errors.New("APC error E102: Parameter Error")
	}
	switch verb {
	case "olOn":
		o.on, o.cycling = true, false
	case "olOff":
		o.on, o.cycling = false, false
	case "olRbootTime":
		o.rbootTime = v
	case "olDlyReboot":
		o.on, o.cycling = false, true
	default:
		return "E101: Command Not Found", errors.New("APC error E101")
	}
	if fault == pduFaultCLIAfter {
		return "", fmt.Errorf("pdu %s %q: no output (transport failure)", f.host, command)
	}
	return "E000: Success", nil
}

func (f *pduPropConn) outletState(_ context.Context, n int) (State, error) {
	fault := f.next()
	o := f.ch.outlets[fmt.Sprintf("%s#%d", f.host, n)]
	if o.cycling && !o.stuck {
		if o.offPolls <= 0 {
			o.on, o.cycling = true, false
		} else {
			o.offPolls--
		}
	}
	switch fault {
	case pduFaultStateErr:
		return StateUnknown, fmt.Errorf("pdu %s redfish outlet %d: HTTP 503", f.host, n)
	case pduFaultStateUnknown:
		return StateUnknown, nil
	}
	if o.on {
		return StateOn, nil
	}
	return StateOff, nil
}

func (f *pduPropConn) close() error { return nil }

var pduStats struct {
	cases, ops, on, off, cycle, status, errs, faultsHit, rollbacks atomic.Int64
}

func TestPDU_Prop_FaultSchedules(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		pduStats.cases.Add(1)
		ch := &pduChassis{outlets: map[string]*pduOutletModel{}, faults: map[int]string{}}
		nOut := rapid.IntRange(1, 3).Draw(rt, "outlets")
		var specs []PDUOutletSpec
		for i := 0; i < nOut; i++ {
			host := rapid.SampledFrom([]string{"pdu-a", "pdu-b"}).Draw(rt, fmt.Sprintf("host%d", i))
			n := rapid.IntRange(1, 4).Draw(rt, fmt.Sprintf("outlet%d", i))
			key := fmt.Sprintf("%s#%d", host, n)
			if ch.outlets[key] != nil {
				continue // the same physical outlet listed twice is not a redundant feed
			}
			ch.outlets[key] = &pduOutletModel{
				on:       rapid.Bool().Draw(rt, fmt.Sprintf("on%d", i)),
				offPolls: rapid.IntRange(0, 2).Draw(rt, fmt.Sprintf("offPolls%d", i)),
				stuck:    rapid.IntRange(0, 9).Draw(rt, fmt.Sprintf("stuck%d", i)) == 0,
			}
			specs = append(specs, PDUOutletSpec{Host: host, Outlet: n, Username: "apc", Password: "pw"})
		}
		nf := rapid.IntRange(0, 3).Draw(rt, "nfaults")
		for i := 0; i < nf; i++ {
			ch.faults[rapid.IntRange(0, 12).Draw(rt, fmt.Sprintf("fidx%d", i))] =
				rapid.SampledFrom([]string{pduFaultCLIBefore, pduFaultCLIAfter, pduFaultStateErr, pduFaultStateUnknown}).Draw(rt, fmt.Sprintf("fkind%d", i))
		}
		p, err := NewPDUController(context.Background(), specs, slog.New(slog.NewTextHandler(io.Discard, nil)),
			WithPDUConnFactory(func(host, _, _ string) pduConn { return &pduPropConn{host: host, ch: ch} }),
			WithRebootDuration(0), WithPollTimeout(30*time.Millisecond))
		if err != nil {
			rt.Fatal(err)
		}
		p.pollInterval = time.Millisecond

		snapshot := func() map[string]bool {
			m := map[string]bool{}
			for k, o := range ch.outlets {
				m[k] = o.on
			}
			return m
		}
		nOps := rapid.IntRange(1, 4).Draw(rt, "nops")
		for i := 0; i < nOps; i++ {
			op := rapid.SampledFrom([]string{"on", "off", "cycle", "status"}).Draw(rt, fmt.Sprintf("op%d", i))
			pduStats.ops.Add(1)
			before := snapshot()
			callsBefore := ch.calls
			logBefore := len(ch.log)
			droppedBefore := len(ch.dropped)
			ctx := context.Background()
			label := fmt.Sprintf("op %d %s (before %v, faults %v)", i, op, before, ch.faults)
			var opErr error
			switch op {
			case "on":
				pduStats.on.Add(1)
				opErr = p.PowerOn(ctx)
				for k, wasOn := range before {
					if wasOn && !ch.outlets[k].on {
						rt.Fatalf("%s: PowerOn turned outlet %s off", label, k)
					}
				}
				if opErr == nil {
					for k, o := range ch.outlets {
						if !o.on {
							rt.Fatalf("%s: PowerOn returned nil but outlet %s is off", label, k)
						}
					}
				}
				// Best effort: every outlet whose own olOn was not dropped
				// is on, even when another feed failed.
				for k, o := range ch.outlets {
					host, n, _ := strings.Cut(k, "#")
					if !o.on && !slices.Contains(ch.dropped[droppedBefore:], host+" olOn "+n) {
						rt.Fatalf("%s: PowerOn (err %v) left outlet %s off although its olOn was never refused\nlog: %q", label, opErr, k, ch.log[logBefore:])
					}
				}
			case "off":
				pduStats.off.Add(1)
				opErr = p.PowerOff(ctx)
				if opErr == nil {
					for k, o := range ch.outlets {
						if o.on {
							rt.Fatalf("%s: PowerOff returned nil but outlet %s is on", label, k)
						}
					}
				} else if !strings.Contains(opErr.Error(), "rollback failed") {
					pduStats.rollbacks.Add(1)
					// Contract (docs/dev/pdu-power-control.md §D): a failed
					// PowerOff rolls back to the pre-op state; no feed that
					// was on before may be left off.
					for k, wasOn := range before {
						if wasOn && !ch.outlets[k].on {
							rt.Fatalf("%s: PowerOff failed (%v) and reported a rollback, but outlet %s that was on is left off\nlog: %q",
								label, opErr, k, ch.log[logBefore:])
						}
					}
				}
			case "cycle":
				pduStats.cycle.Add(1)
				opErr = p.PowerCycle(ctx)
				for _, l := range ch.log[logBefore:] {
					if strings.Contains(l, " olOff ") || strings.Contains(l, " olOn ") {
						rt.Fatalf("%s: PowerCycle composed a cycle from discrete commands: %q", label, ch.log[logBefore:])
					}
				}
				if opErr == nil {
					for k, o := range ch.outlets {
						if !o.on {
							rt.Fatalf("%s: PowerCycle returned nil but outlet %s is off", label, k)
						}
						host, n, _ := strings.Cut(k, "#")
						if !slices.Contains(ch.log[logBefore:], host+" olDlyReboot "+n) {
							rt.Fatalf("%s: PowerCycle returned nil but outlet %s was never cycled\nlog: %q", label, k, ch.log[logBefore:])
						}
					}
				}
				// Settle in-flight firmware cycles before the next op.
				for _, o := range ch.outlets {
					if o.cycling && !o.stuck {
						o.on, o.cycling = true, false
					}
				}
			case "status":
				pduStats.status.Add(1)
				st, serr := p.PowerStatus(ctx)
				opErr = serr
				truth := ch.poweredTruth()
				faulted := false
				for k := range ch.faults {
					faulted = faulted || (k >= callsBefore && k < ch.calls)
				}
				want := StateOff
				if truth {
					want = StateOn
				}
				switch {
				case !faulted && (serr != nil || st != want):
					// Without this, a controller that always answered
					// Unknown would satisfy the two cases below.
					rt.Fatalf("%s: PowerStatus with every read answered = %v, %v; want %v", label, st, serr, want)
				case serr == nil && st == StateOff && truth:
					rt.Fatalf("%s: PowerStatus reported Off but outlets %v are powering the host", label, snapshot())
				case serr == nil && st == StateOn && !truth:
					rt.Fatalf("%s: PowerStatus reported On but every outlet is off", label)
				}
			}
			if opErr != nil {
				pduStats.errs.Add(1)
			}
			for k := range ch.faults {
				if k >= callsBefore && k < ch.calls {
					pduStats.faultsHit.Add(1)
				}
			}
		}
	})
	t.Logf("coverage: cases=%d ops=%d on=%d off=%d cycle=%d status=%d errs=%d faultsHit=%d failedOffs=%d",
		pduStats.cases.Load(), pduStats.ops.Load(), pduStats.on.Load(), pduStats.off.Load(), pduStats.cycle.Load(),
		pduStats.status.Load(), pduStats.errs.Load(), pduStats.faultsHit.Load(), pduStats.rollbacks.Load())
}
