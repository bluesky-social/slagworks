package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/slagworks/internal/model"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// timeoutErr is a net.Error that reports Timeout()=true, used as the fake
// "end of the bounded observation window" sentinel.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "read deadline exceeded" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func testDiscover() (*dhcpv4.DHCPv4, error) {
	return dhcpv4.NewDiscovery(net.HardwareAddr{0x02, 0x01, 0x02, 0x03, 0x04, 0x05})
}

// testOffer builds an OFFER replying to discover. sid="" omits option 54
// (missing); malformed emits a truncated option 54.
func testOffer(discover *dhcpv4.DHCPv4, sid string, malformed bool) (*dhcpv4.DHCPv4, error) {
	mods := []dhcpv4.Modifier{dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer)}
	switch {
	case malformed:
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptGeneric(dhcpv4.OptionServerIdentifier, []byte{0xC0, 0xA8})))
	case sid != "":
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP(sid))))
	}
	return dhcpv4.NewReplyFromRequest(discover, mods...)
}

func TestSyntheticOOBMAC(t *testing.T) {
	_, subnet, err := net.ParseCIDR("10.254.240.0/22")
	if err != nil {
		t.Fatal(err)
	}
	mac := syntheticOOBMAC(subnet)
	if len(mac) != 6 {
		t.Fatalf("MAC length = %d, want 6", len(mac))
	}
	if mac[0]&0x02 == 0 {
		t.Errorf("MAC %s must set the locally-administered bit (0x02)", mac)
	}
	if mac[0]&0x01 != 0 {
		t.Errorf("MAC %s must not set the multicast bit", mac)
	}

	t.Run("stable across calls for the same subnet", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			if got := syntheticOOBMAC(subnet); !bytes.Equal(got, mac) {
				t.Fatalf("MAC changed: %v != %v", got, mac)
			}
		}
	})

	t.Run("distinct across subnets", func(t *testing.T) {
		_, other, _ := net.ParseCIDR("10.99.0.0/16")
		if otherMAC := syntheticOOBMAC(other); bytes.Equal(otherMAC, mac) {
			t.Fatalf("expected a different MAC for a different subnet, both %v", mac)
		}
	})

	t.Run("distinct across prefix lengths with the same base address", func(t *testing.T) {
		_, narrower, _ := net.ParseCIDR("10.254.240.0/23")
		if otherMAC := syntheticOOBMAC(narrower); bytes.Equal(otherMAC, mac) {
			t.Fatalf("expected prefix length to affect MAC, both %v", mac)
		}
	})
}

func TestOOBAllowedServerIDs(t *testing.T) {
	allowed := oobAllowedServerIDs(net.ParseIP("10.254.240.11"), []model.OOBDynamicSlice{
		{Start: "10.254.241.100", End: "10.254.241.149", Owner: "10.254.240.11"},
		{Start: "10.254.241.150", End: "10.254.241.199", Owner: "10.254.240.12"},
		{Start: "10.254.241.200", End: "10.254.241.249", Owner: "not-an-ip"},
		{Start: "10.254.242.100", End: "10.254.242.149", Owner: "2001:db8::1"},
	})
	want := map[string]bool{"10.254.240.11": true, "10.254.240.12": true}
	if len(allowed) != len(want) {
		t.Fatalf("allowed = %v, want %v", allowed, want)
	}
	for k := range want {
		if _, ok := allowed[k]; !ok {
			t.Errorf("missing allowed server %q in %v", k, allowed)
		}
	}
}

func TestClassifyOOBOffer(t *testing.T) {
	d, err := testDiscover()
	if err != nil {
		t.Fatal(err)
	}
	allowed := oobAllowedServerIDs(net.ParseIP("10.254.240.11"), []model.OOBDynamicSlice{
		{Owner: "10.254.240.11"},
		{Owner: "10.254.240.12"},
	})

	cases := []struct {
		name string
		pkt  func() (*dhcpv4.DHCPv4, error)
		want oobOfferClass
	}{
		{"local OOB address is allowed", func() (*dhcpv4.DHCPv4, error) { return testOffer(d, "10.254.240.11", false) }, oobOfferAllowed},
		{"dynamic-slice owner is allowed", func() (*dhcpv4.DHCPv4, error) { return testOffer(d, "10.254.240.12", false) }, oobOfferAllowed},
		{"foreign option-54 is foreign", func() (*dhcpv4.DHCPv4, error) { return testOffer(d, "10.9.9.9", false) }, oobOfferForeign},
		{"missing option-54 is foreign", func() (*dhcpv4.DHCPv4, error) { return testOffer(d, "", false) }, oobOfferForeign},
		{"malformed option-54 is foreign", func() (*dhcpv4.DHCPv4, error) { return testOffer(d, "", true) }, oobOfferForeign},
		{"nil packet is ignored", func() (*dhcpv4.DHCPv4, error) { return nil, nil }, oobOfferIgnore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pkt, err := tc.pkt()
			if err != nil {
				t.Fatal(err)
			}
			if got := classifyOOBOffer(pkt, allowed); got != tc.want {
				t.Fatalf("classify = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("non-OFFER message type is ignored", func(t *testing.T) {
		ack, err := dhcpv4.NewReplyFromRequest(d, dhcpv4.WithMessageType(dhcpv4.MessageTypeAck),
			dhcpv4.WithOption(dhcpv4.OptServerIdentifier(net.ParseIP("10.9.9.9"))))
		if err != nil {
			t.Fatal(err)
		}
		if got := classifyOOBOffer(ack, allowed); got != oobOfferIgnore {
			t.Fatalf("ACK classify = %v, want ignore", got)
		}
	})
}

// fakeProbeReader replays a fixed sequence of packets/errors, then falls
// through to a timeout sentinel (end of the observation window).
type fakeProbeReader struct {
	items []interface{}
	idx   int
}

func (f *fakeProbeReader) next() (*dhcpv4.DHCPv4, error) {
	if f.idx >= len(f.items) {
		return nil, timeoutErr{}
	}
	it := f.items[f.idx]
	f.idx++
	switch v := it.(type) {
	case *dhcpv4.DHCPv4:
		return v, nil
	case error:
		return nil, v
	}
	return nil, nil
}

func TestEvalOOBOffers(t *testing.T) {
	d, err := testDiscover()
	if err != nil {
		t.Fatal(err)
	}
	other, err := testDiscover() // a different transaction id
	if err != nil {
		t.Fatal(err)
	}
	allowed := oobAllowedServerIDs(net.ParseIP("10.254.240.11"), []model.OOBDynamicSlice{{Owner: "10.254.240.11"}, {Owner: "10.254.240.12"}})

	mustOffer := func(dis *dhcpv4.DHCPv4, sid string) *dhcpv4.DHCPv4 {
		t.Helper()
		p, err := testOffer(dis, sid, false)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		name  string
		items []interface{}
		want  oobProbeOutcome
	}{
		{"silence through timeout is success", []interface{}{}, probeOK},
		{"allowed-only is success", []interface{}{mustOffer(d, "10.254.240.11")}, probeOK},
		{"foreign OFFER is a conflict", []interface{}{mustOffer(d, "10.9.9.9")}, probeConflict},
		{"mixed allowed and foreign is a conflict", []interface{}{mustOffer(d, "10.254.240.11"), mustOffer(d, "10.9.9.9")}, probeConflict},
		{"irrelevant traffic ignored", []interface{}{mustOffer(other, "10.9.9.9"), mustOffer(d, "10.254.240.11")}, probeOK},
		{"read error fails closed", []interface{}{errors.New("socket vanished")}, probeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeProbeReader{items: tc.items}
			outcome, _, err := evalOOBOffers(r.next, d, allowed)
			if outcome != tc.want {
				t.Fatalf("outcome = %v, want %v (err=%v)", outcome, tc.want, err)
			}
			if tc.want == probeError && err == nil {
				t.Fatal("expected a probe error to surface an error")
			}
		})
	}

	t.Run("matching transaction with wrong client MAC is ignored", func(t *testing.T) {
		wrongClient, err := testDiscover()
		if err != nil {
			t.Fatal(err)
		}
		wrongClient.ClientHWAddr = net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}
		offer := mustOffer(wrongClient, "10.7.7.7")
		offer.TransactionID = d.TransactionID
		r := &fakeProbeReader{items: []interface{}{offer}}
		outcome, _, err := evalOOBOffers(r.next, d, allowed)
		if outcome != probeOK || err != nil {
			t.Fatalf("outcome=%v err=%v, want ignored packet and success", outcome, err)
		}
	})

	t.Run("foreign server id is returned on conflict", func(t *testing.T) {
		r := &fakeProbeReader{items: []interface{}{mustOffer(d, "10.7.7.7")}}
		outcome, sid, err := evalOOBOffers(r.next, d, allowed)
		if outcome != probeConflict || err != nil {
			t.Fatalf("outcome=%v err=%v", outcome, err)
		}
		if sid == nil || sid.String() != "10.7.7.7" {
			t.Fatalf("foreign id = %v, want 10.7.7.7", sid)
		}
	})
}

func TestOOBConflictGate(t *testing.T) {
	t.Run("starts unlocked", func(t *testing.T) {
		g := newOOBConflictGate()
		if g.suppressed() {
			t.Fatal("new gate must not be suppressed")
		}
		if reason := g.readyReason(); reason != "" {
			t.Fatalf("readyReason = %q, want empty", reason)
		}
		if g.conflictGauge() != 0 || g.probeErrorCount() != 0 {
			t.Fatalf("gauge=%v count=%v, want 0/0", g.conflictGauge(), g.probeErrorCount())
		}
		if err := oobConflictReadinessError(g); err != nil {
			t.Fatalf("readiness error = %v, want nil", err)
		}
	})

	t.Run("conflict latches with foreign server id in readiness", func(t *testing.T) {
		g := newOOBConflictGate()
		g.record(probeConflict, net.ParseIP("10.4.4.4"), nil)
		if !g.suppressed() {
			t.Fatal("conflict must latch")
		}
		if g.conflictGauge() != 1 {
			t.Fatalf("conflict gauge = %v, want 1", g.conflictGauge())
		}
		if g.probeErrorCount() != 0 {
			t.Fatalf("probe error count = %v, want 0", g.probeErrorCount())
		}
		reason := g.readyReason()
		if !strings.Contains(reason, "10.4.4.4") || !strings.Contains(reason, "suppressed") {
			t.Fatalf("readyReason = %q, want foreign id + suppressed", reason)
		}
		if err := oobConflictReadinessError(g); err == nil || err.Error() != reason {
			t.Fatalf("readiness error = %v, want %q", err, reason)
		}
	})

	t.Run("probe error latches and counts, conflict gauge stays 0", func(t *testing.T) {
		g := newOOBConflictGate()
		g.record(probeError, nil, errors.New("open socket: boom"))
		if !g.suppressed() {
			t.Fatal("probe error must latch")
		}
		if g.conflictGauge() != 0 {
			t.Fatalf("conflict gauge = %v, want 0 for error", g.conflictGauge())
		}
		if g.probeErrorCount() != 1 {
			t.Fatalf("probe error count = %v, want 1", g.probeErrorCount())
		}
		reason := g.readyReason()
		if !strings.Contains(reason, "failed") || !strings.Contains(reason, "boom") {
			t.Fatalf("readyReason = %q", reason)
		}
	})

	t.Run("latch is sticky across further records", func(t *testing.T) {
		g := newOOBConflictGate()
		g.record(probeConflict, net.ParseIP("10.4.4.4"), nil)
		reasonBefore := g.readyReason()
		g.record(probeError, nil, errors.New("later error"))
		if g.readyReason() != reasonBefore {
			t.Fatalf("reason changed after latch: %q != %q", g.readyReason(), reasonBefore)
		}
		if g.probeErrorCount() != 1 {
			t.Fatalf("probe error counter must still count: %v", g.probeErrorCount())
		}
		if g.conflictGauge() != 1 {
			t.Fatalf("conflict gauge = %v, want 1", g.conflictGauge())
		}
	})
}

// fakeSink is an oobOfferSink that replays a fixed packet stream.
type fakeSink struct {
	discover *dhcpv4.DHCPv4
	pkts     []*dhcpv4.DHCPv4
	sendErr  error
	idx      int
}

func (f *fakeSink) SendDiscover() (*dhcpv4.DHCPv4, error) {
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	return f.discover, nil
}
func (f *fakeSink) ReadOffer() (*dhcpv4.DHCPv4, error) {
	if f.idx < len(f.pkts) {
		p := f.pkts[f.idx]
		f.idx++
		return p, nil
	}
	return nil, timeoutErr{}
}
func (f *fakeSink) Close() error { return nil }

func TestRunOOBProbe(t *testing.T) {
	d, _ := testDiscover()
	allowed := oobAllowedServerIDs(net.ParseIP("10.254.240.11"), []model.OOBDynamicSlice{{Owner: "10.254.240.11"}, {Owner: "10.254.240.12"}})
	log := discardLogger()

	t.Run("silence is success and does not suppress", func(t *testing.T) {
		g := newOOBConflictGate()
		ok := runOOBProbe(g, &fakeSink{discover: d}, allowed, log)
		if !ok {
			t.Fatal("expected allow on silence")
		}
		if g.suppressed() || g.readyReason() != "" {
			t.Fatalf("gate must stay unlatched on success: %q", g.readyReason())
		}
	})

	t.Run("foreign OFFER suppresses and latching persists", func(t *testing.T) {
		g := newOOBConflictGate()
		foreign, _ := testOffer(d, "10.6.6.6", false)
		ok := runOOBProbe(g, &fakeSink{discover: d, pkts: []*dhcpv4.DHCPv4{foreign}}, allowed, log)
		if ok {
			t.Fatal("expected suppress on foreign OFFER")
		}
		if !g.suppressed() || g.conflictGauge() != 1 {
			t.Fatalf("gate must latch conflict, gauge=%v", g.conflictGauge())
		}
		if !strings.Contains(g.readyReason(), "10.6.6.6") {
			t.Fatalf("reason missing foreign id: %q", g.readyReason())
		}
	})

	t.Run("send error fails closed and counts a probe error", func(t *testing.T) {
		g := newOOBConflictGate()
		ok := runOOBProbe(g, &fakeSink{discover: d, sendErr: errors.New("dispatch: err")}, allowed, log)
		if ok {
			t.Fatal("expected suppress on send error")
		}
		if !g.suppressed() || g.probeErrorCount() != 1 {
			t.Fatalf("gate must latch error, count=%v", g.probeErrorCount())
		}
	})
}

func TestOOBProbeSocketIsExclusive(t *testing.T) {
	first, err := listenOOBProbeSocket("", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	port := first.LocalAddr().(*net.UDPAddr).Port

	second, err := listenOOBProbeSocket("", port)
	if err == nil {
		_ = second.Close()
		t.Fatalf("second listener unexpectedly acquired UDP/%d", port)
	}
}

func TestBindOOBSegment(t *testing.T) {
	oob := model.OOBConfig{
		Subnet:  "10.254.240.0/22",
		Gateway: "10.254.243.253",
		DynamicSlices: []model.OOBDynamicSlice{
			{Owner: "10.254.240.11"},
			{Owner: "10.254.240.12"},
		},
	}
	bind := oobBind{iface: "eno3", serverIP: net.ParseIP("10.254.240.11")}
	log := discardLogger()

	type rec struct{ dhcp, assist, probes int }

	t.Run("clean probe starts DHCP and assist", func(t *testing.T) {
		r := &rec{}
		gate := newOOBConflictGate()
		probe := func(oobBind, map[string]struct{}) bool { r.probes++; return true }
		bindOOBSegment(gate, probe, oob, bind, func() { r.dhcp++ }, func() { r.assist++ }, log)
		if r.dhcp != 1 || r.assist != 1 || r.probes != 1 {
			t.Fatalf("dhcp=%d assist=%d probes=%d, want 1/1/1", r.dhcp, r.assist, r.probes)
		}
		if gate.suppressed() {
			t.Fatal("clean probe must not latch")
		}
	})

	t.Run("conflict suppresses DHCP but keeps assist", func(t *testing.T) {
		r := &rec{}
		gate := newOOBConflictGate()
		probe := func(oobBind, map[string]struct{}) bool {
			r.probes++
			gate.record(probeConflict, net.ParseIP("10.5.5.5"), nil)
			return false
		}
		bindOOBSegment(gate, probe, oob, bind, func() { r.dhcp++ }, func() { r.assist++ }, log)
		if r.dhcp != 0 || r.assist != 1 || !gate.suppressed() {
			t.Fatalf("dhcp=%d assist=%d suppressed=%v, want 0/1/true", r.dhcp, r.assist, gate.suppressed())
		}
	})

	t.Run("latched gate skips probe and suppresses DHCP", func(t *testing.T) {
		r := &rec{}
		gate := newOOBConflictGate()
		gate.record(probeConflict, net.ParseIP("10.5.5.5"), nil)
		probe := func(oobBind, map[string]struct{}) bool { r.probes++; return true }
		bindOOBSegment(gate, probe, oob, bind, func() { r.dhcp++ }, func() { r.assist++ }, log)
		if r.probes != 0 || r.dhcp != 0 || r.assist != 1 {
			t.Fatalf("probes=%d dhcp=%d assist=%d, want 0/0/1", r.probes, r.dhcp, r.assist)
		}
	})
}

func TestBindOOBSegmentAcquisitionSequence(t *testing.T) {
	// Simulates standby VIP acquisitions: each acquisition re-runs
	// bindOOBSegment. Before the latch, later acquisitions re-probe; once
	// latched, no further probes and DHCP stays suppressed.
	oob := model.OOBConfig{
		Subnet:        "10.254.240.0/22",
		Gateway:       "10.254.243.253",
		DynamicSlices: []model.OOBDynamicSlice{{Owner: "10.254.240.11"}, {Owner: "10.254.240.12"}},
	}
	bind := oobBind{iface: "eno3", serverIP: net.ParseIP("10.254.240.11")}
	log := discardLogger()

	r := struct{ dhcp, assist, probes int }{}
	gate := newOOBConflictGate()

	// First two acquisitions: clean probes, DHCP starts each time.
	for i := 0; i < 2; i++ {
		bindOOBSegment(gate, func(oobBind, map[string]struct{}) bool { r.probes++; return true },
			oob, bind, func() { r.dhcp++ }, func() { r.assist++ }, log)
	}
	if r.probes != 2 || r.dhcp != 2 {
		t.Fatalf("after 2 clean acquisitions: probes=%d dhcp=%d, want 2/2", r.probes, r.dhcp)
	}

	// Third acquisition detects a foreign server and latches.
	bindOOBSegment(gate, func(oobBind, map[string]struct{}) bool {
		r.probes++
		gate.record(probeConflict, net.ParseIP("10.5.5.5"), nil)
		return false
	}, oob, bind, func() { r.dhcp++ }, func() { r.assist++ }, log)
	if r.probes != 3 || r.dhcp != 2 || !gate.suppressed() {
		t.Fatalf("after conflict acquisition: probes=%d dhcp=%d suppressed=%v, want 3/2/true", r.probes, r.dhcp, gate.suppressed())
	}

	// Fourth acquisition: latched — no probe, no DHCP, assist still up.
	probesBefore := r.probes
	bindOOBSegment(gate, func(oobBind, map[string]struct{}) bool { r.probes++; return true },
		oob, bind, func() { r.dhcp++ }, func() { r.assist++ }, log)
	if r.probes != probesBefore || r.dhcp != 2 || r.assist != 4 {
		t.Fatalf("after latched acquisition: probes=%d dhcp=%d assist=%d, want %d/2/4", r.probes, r.dhcp, r.assist, probesBefore)
	}
}

func TestRegisterOOBDHCPConflictMetrics(t *testing.T) {
	gather := func(reg *prometheus.Registry) (conflict float64, errors float64) {
		t.Helper()
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				switch mf.GetName() {
				case "slagworks_site_oob_dhcp_conflict":
					conflict = m.GetGauge().GetValue()
				case "slagworks_site_oob_probe_errors_total":
					errors = m.GetCounter().GetValue()
				}
			}
		}
		return conflict, errors
	}

	t.Run("conflict latches the gauge but not the error counter", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		gate := newOOBConflictGate()
		registerOOBDHCPConflictMetrics(reg, gate)
		if c, e := gather(reg); c != 0 || e != 0 {
			t.Fatalf("initial: conflict=%v errors=%v, want 0/0", c, e)
		}
		gate.record(probeConflict, net.ParseIP("10.3.3.3"), nil)
		if c, e := gather(reg); c != 1 || e != 0 {
			t.Fatalf("after conflict: conflict=%v errors=%v, want 1/0", c, e)
		}
	})

	t.Run("probe error bumps the counter, gauge stays 0", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		gate := newOOBConflictGate()
		registerOOBDHCPConflictMetrics(reg, gate)
		gate.record(probeError, nil, errors.New("socket: boom"))
		if c, e := gather(reg); c != 0 || e != 1 {
			t.Fatalf("after error: conflict=%v errors=%v, want 0/1", c, e)
		}
	})
}
