package power

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePDUConn is an in-memory pduConn for testing the controller's
// orchestration without real SSH/Redfish. It records every CLI command
// and lets a test script per-command errors and outlet states.
type fakePDUConn struct {
	host string
	mu   sync.Mutex
	// state per outlet index.
	states map[int]State
	// cmds records every cli() invocation in order.
	cmds []string
	// failCmd: if the command string contains this substring, cli errors.
	failCmd string
	// stateErr: outletState returns this error when non-nil.
	stateErr error
}

func newFakePDUConn(host string) *fakePDUConn {
	return &fakePDUConn{host: host, states: map[int]State{}}
}

func (f *fakePDUConn) cli(_ context.Context, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, command)
	if f.failCmd != "" && strings.Contains(command, f.failCmd) {
		return "", fmt.Errorf("fake: command %q failed", command)
	}
	// Mutate fake state to mirror real outlet behavior.
	var idx int
	switch {
	case strings.HasPrefix(command, "olOff "):
		fmt.Sscanf(command, "olOff %d", &idx)
		f.states[idx] = StateOff
	case strings.HasPrefix(command, "olOn "):
		fmt.Sscanf(command, "olOn %d", &idx)
		f.states[idx] = StateOn
	case strings.HasPrefix(command, "olDlyReboot "):
		// Atomic cycle: ends with the outlet back On.
		fmt.Sscanf(command, "olDlyReboot %d", &idx)
		f.states[idx] = StateOn
	}
	return "E000: Success", nil
}

func (f *fakePDUConn) outletState(_ context.Context, outlet int) (State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stateErr != nil {
		return StateUnknown, f.stateErr
	}
	if st, ok := f.states[outlet]; ok {
		return st, nil
	}
	return StateOn, nil // default: powered
}

func (f *fakePDUConn) close() error { return nil }

// newTestPDUController builds a PDUController whose connFactory returns the
// provided fakes (keyed by host), with a fast poll loop for cycle tests.
func newTestPDUController(t *testing.T, specs []PDUOutletSpec, fakes map[string]*fakePDUConn) *PDUController {
	t.Helper()
	factory := func(host, _, _ string) pduConn {
		if f, ok := fakes[host]; ok {
			return f
		}
		t.Fatalf("no fake for host %q", host)
		return nil
	}
	p, err := NewPDUController(context.Background(), specs, nil,
		WithPDUConnFactory(factory),
		WithRebootDuration(0), // no real off-window wait in tests
		WithPollTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("NewPDUController: %v", err)
	}
	p.pollInterval = time.Millisecond // fast poll in tests
	return p
}

func TestPDU_PowerOnOff_SingleOutlet(t *testing.T) {
	f := newFakePDUConn("pdu-a")
	specs := []PDUOutletSpec{{Host: "pdu-a", Outlet: 5}}
	p := newTestPDUController(t, specs, map[string]*fakePDUConn{"pdu-a": f})

	if err := p.PowerOff(context.Background()); err != nil {
		t.Fatalf("PowerOff: %v", err)
	}
	if f.states[5] != StateOff {
		t.Errorf("outlet 5 should be Off, got %s", f.states[5])
	}
	if err := p.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if f.states[5] != StateOn {
		t.Errorf("outlet 5 should be On, got %s", f.states[5])
	}
	// Exact CLI strings.
	want := []string{"olOff 5", "olOn 5"}
	if len(f.cmds) != 2 || f.cmds[0] != want[0] || f.cmds[1] != want[1] {
		t.Errorf("cmds = %v, want %v", f.cmds, want)
	}
}

func TestPDU_PowerCycle_DualFeed_DispatchesAtomicAndWaitsOn(t *testing.T) {
	fa := newFakePDUConn("pdu-left")
	fb := newFakePDUConn("pdu-right")
	specs := []PDUOutletSpec{
		{Host: "pdu-left", Outlet: 4},
		{Host: "pdu-right", Outlet: 4},
	}
	p := newTestPDUController(t, specs, map[string]*fakePDUConn{"pdu-left": fa, "pdu-right": fb})

	if err := p.PowerCycle(context.Background()); err != nil {
		t.Fatalf("PowerCycle: %v", err)
	}
	// Each feed must get olRbootTime then olDlyReboot, in that order.
	for name, f := range map[string]*fakePDUConn{"left": fa, "right": fb} {
		if len(f.cmds) != 2 {
			t.Fatalf("%s: got cmds %v, want 2", name, f.cmds)
		}
		if !strings.HasPrefix(f.cmds[0], "olRbootTime 4 ") {
			t.Errorf("%s: first cmd %q, want olRbootTime 4 <n>", name, f.cmds[0])
		}
		if f.cmds[1] != "olDlyReboot 4" {
			t.Errorf("%s: second cmd %q, want olDlyReboot 4", name, f.cmds[1])
		}
	}
}

// TestPDU_PowerCycle_NeverComposesOffOn is the interface-contract guard:
// PowerCycle must use the firmware-atomic olDlyReboot, NEVER a
// controller-composed olOff+olOn (which could strand a device carrying
// our own control path).
func TestPDU_PowerCycle_NeverComposesOffOn(t *testing.T) {
	f := newFakePDUConn("pdu-a")
	p := newTestPDUController(t, []PDUOutletSpec{{Host: "pdu-a", Outlet: 5}}, map[string]*fakePDUConn{"pdu-a": f})
	if err := p.PowerCycle(context.Background()); err != nil {
		t.Fatalf("PowerCycle: %v", err)
	}
	for _, cmd := range f.cmds {
		if strings.HasPrefix(cmd, "olOff ") || cmd == "olOn 5" {
			t.Errorf("PowerCycle must not issue discrete off/on, got %q in %v", cmd, f.cmds)
		}
	}
}

// TestPDU_PowerOff_PartialFailureRollsBack verifies the directional
// rollback: if the second feed's olOff fails, the first (already cut)
// feed is restored with olOn so the box returns to fully-redundant.
func TestPDU_PowerOff_PartialFailureRollsBack(t *testing.T) {
	fa := newFakePDUConn("pdu-left")
	fb := newFakePDUConn("pdu-right")
	fb.failCmd = "olOff" // right feed fails to turn off
	specs := []PDUOutletSpec{
		{Host: "pdu-left", Outlet: 4},
		{Host: "pdu-right", Outlet: 4},
	}
	p := newTestPDUController(t, specs, map[string]*fakePDUConn{"pdu-left": fa, "pdu-right": fb})

	err := p.PowerOff(context.Background())
	if err == nil {
		t.Fatal("expected PowerOff to fail when one feed errors")
	}
	// Left feed was cut then rolled back On.
	wantLeft := []string{"olOff 4", "olOn 4"}
	if len(fa.cmds) != 2 || fa.cmds[0] != wantLeft[0] || fa.cmds[1] != wantLeft[1] {
		t.Errorf("left cmds = %v, want %v (cut then rollback)", fa.cmds, wantLeft)
	}
	if fa.states[4] != StateOn {
		t.Errorf("left outlet should be rolled back to On, got %s", fa.states[4])
	}
}

func TestPDU_PowerStatus_AnyOnPinsOn(t *testing.T) {
	fa := newFakePDUConn("pdu-left")
	fb := newFakePDUConn("pdu-right")
	fa.states[4] = StateOn
	fb.states[4] = StateOff // one feed off, one on -> box still powered
	specs := []PDUOutletSpec{{Host: "pdu-left", Outlet: 4}, {Host: "pdu-right", Outlet: 4}}
	p := newTestPDUController(t, specs, map[string]*fakePDUConn{"pdu-left": fa, "pdu-right": fb})

	st, err := p.PowerStatus(context.Background())
	if err != nil {
		t.Fatalf("PowerStatus: %v", err)
	}
	if st != StateOn {
		t.Errorf("mixed feeds: got %s, want On (any live feed -> powered)", st)
	}
}

func TestPDU_PowerStatus_AllOff(t *testing.T) {
	fa := newFakePDUConn("pdu-left")
	fa.states[4] = StateOff
	p := newTestPDUController(t, []PDUOutletSpec{{Host: "pdu-left", Outlet: 4}}, map[string]*fakePDUConn{"pdu-left": fa})
	st, err := p.PowerStatus(context.Background())
	if err != nil {
		t.Fatalf("PowerStatus: %v", err)
	}
	if st != StateOff {
		t.Errorf("got %s, want Off", st)
	}
}

func TestPDU_PowerStatus_NoLiveFeedPlusErrorIsUnknown(t *testing.T) {
	fa := newFakePDUConn("pdu-left")
	fa.stateErr = fmt.Errorf("redfish unreachable")
	p := newTestPDUController(t, []PDUOutletSpec{{Host: "pdu-left", Outlet: 4}}, map[string]*fakePDUConn{"pdu-left": fa})
	st, err := p.PowerStatus(context.Background())
	if st != StateUnknown {
		t.Errorf("got %s, want Unknown when no live feed and a read errored", st)
	}
	if err == nil {
		t.Error("expected error surfaced with Unknown")
	}
}

// TestPDU_PowerStatus_UnrecognizedPlusOffIsUnknown: one feed answers with a
// state that is neither On nor Off, the other reads Off. The unrecognized
// feed may be the one powering the box, so the aggregate must be Unknown,
// not Off (Off would make the executor PowerOn instead of PowerCycle a box
// that is already running).
func TestPDU_PowerStatus_UnrecognizedPlusOffIsUnknown(t *testing.T) {
	fa := newFakePDUConn("pdu-left")
	fb := newFakePDUConn("pdu-right")
	fa.states[4] = StateUnknown
	fb.states[4] = StateOff
	specs := []PDUOutletSpec{{Host: "pdu-left", Outlet: 4}, {Host: "pdu-right", Outlet: 4}}
	p := newTestPDUController(t, specs, map[string]*fakePDUConn{"pdu-left": fa, "pdu-right": fb})

	st, err := p.PowerStatus(context.Background())
	if st != StateUnknown || err != nil {
		t.Errorf("PowerStatus = (%s, %v), want (unknown, nil): the unrecognized feed may be live", st, err)
	}
}

// TestPDU_PowerStatus_UnrecognizedRedfishPlusOffIsUnknown drives the same
// case through the production Redfish reader: two TLS servers stand in for
// the two NMCs. Feed A answers HTTP 200 with a SwitchedState the reader does
// not recognize (or none at all), which it reports as (Unknown, nil); feed B
// answers Off.
func TestPDU_PowerStatus_UnrecognizedRedfishPlusOffIsUnknown(t *testing.T) {
	serve := func(t *testing.T, body string) string {
		t.Helper()
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		return strings.TrimPrefix(srv.URL, "https://")
	}
	for _, bodyA := range []string{
		`{"Oem":{"SwitchedState":"Rebooting"}}`,
		`{"Oem":{}}`,
		`{}`,
	} {
		t.Run(bodyA, func(t *testing.T) {
			hostA := serve(t, bodyA)
			hostB := serve(t, `{"Oem":{"SwitchedState":"Off"}}`)
			p, err := NewPDUController(context.Background(),
				[]PDUOutletSpec{{Host: hostA, Outlet: 1}, {Host: hostB, Outlet: 1}}, nil)
			if err != nil {
				t.Fatalf("NewPDUController: %v", err)
			}
			t.Cleanup(func() { _ = p.Close(context.Background()) })

			st, err := p.PowerStatus(context.Background())
			if st != StateUnknown || err != nil {
				t.Errorf("PowerStatus = (%s, %v), want (unknown, nil): feed A's state is unreadable and may be live", st, err)
			}
		})
	}
}

func TestPDU_SetBootDevice_Contract(t *testing.T) {
	f := newFakePDUConn("pdu-a")
	p := newTestPDUController(t, []PDUOutletSpec{{Host: "pdu-a", Outlet: 5}}, map[string]*fakePDUConn{"pdu-a": f})
	ctx := context.Background()
	if err := p.SetBootDevice(ctx, BootPXE); err != nil {
		t.Errorf("BootPXE should be nil (permanent-PXE satisfies it), got %v", err)
	}
	if err := p.SetBootDevice(ctx, BootDefault); err != nil {
		t.Errorf("BootDefault should be nil, got %v", err)
	}
	if err := p.SetBootDevice(ctx, BootDisk); err == nil {
		t.Error("BootDisk must error (PDU cannot set boot order; silent success would lie to the executor)")
	}
}

func TestPDU_ChassisSerial_Empty(t *testing.T) {
	f := newFakePDUConn("pdu-a")
	p := newTestPDUController(t, []PDUOutletSpec{{Host: "pdu-a", Outlet: 5}}, map[string]*fakePDUConn{"pdu-a": f})
	s, err := p.ChassisSerial(context.Background())
	if err != nil || s != "" {
		t.Errorf("ChassisSerial = (%q, %v), want (\"\", nil)", s, err)
	}
}

func TestPDU_NewController_Validation(t *testing.T) {
	if _, err := NewPDUController(context.Background(), nil, nil); err == nil {
		t.Error("expected error for no outlets")
	}
	if _, err := NewPDUController(context.Background(), []PDUOutletSpec{{Host: "", Outlet: 5}}, nil); err == nil {
		t.Error("expected error for empty host")
	}
	if _, err := NewPDUController(context.Background(), []PDUOutletSpec{{Host: "pdu-a", Outlet: 0}}, nil); err == nil {
		t.Error("expected error for outlet < 1")
	}
}

func TestParseAPCResult(t *testing.T) {
	if err := parseAPCResult("E000: Success\n"); err != nil {
		t.Errorf("E000 should be success, got %v", err)
	}
	if err := parseAPCResult(" 5: Outlet 5: On \nE000: Success"); err != nil {
		t.Errorf("trailing E000 after output should be success, got %v", err)
	}
	if err := parseAPCResult("E102: Parameter Error"); err == nil {
		t.Error("E102 should be an error")
	}
	if err := parseAPCResult("no code here"); err == nil {
		t.Error("missing E-code should be an error (can't confirm success)")
	}
}
