package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bluesky-social/slagworks/internal/model"
)

// oobProbeTimeout bounds how long a single OOB DHCP conflict probe watches
// the segment for OFFERs. Short enough that the startup / VIP-acquisition
// interlock doesn't noticeably stall service bring-up; long enough that a
// live server's OFFER isn't missed on a busy segment.
const oobProbeTimeout = 3 * time.Second

// syntheticOOBMAC derives a stable synthetic DHCP client MAC from the
// canonical OOB CIDR. The locally-administered bit distinguishes the intended
// use but does not make collisions impossible: real and virtual NICs may also
// use locally-administered addresses. Hashing the address and prefix makes a
// collision unlikely and keeps CIDRs with the same base address but different
// prefix lengths distinct.
//
// Stability is the point: the OOB server reserves a lease keyed by client
// MAC on DISCOVER (see dhcp.Handler.allocateIP). A fresh random MAC per
// attempt would consume a brand-new pool address every probe (unbounded
// phantom lease consumption). A stable subnet-derived MAC makes every probe
// reuse the same identity, so the server renews one lease in place.
func syntheticOOBMAC(subnet *net.IPNet) net.HardwareAddr {
	ones, bits := subnet.Mask.Size()
	network := subnet.IP.Mask(subnet.Mask)
	canonical := fmt.Sprintf("%s/%d/%d", network.String(), ones, bits)
	sum := sha256.Sum256([]byte(canonical))
	mac := net.HardwareAddr(append([]byte(nil), sum[:6]...))
	mac[0] = (mac[0] | 0x02) &^ 0x01 // locally administered, unicast
	return mac
}

// oobAllowedServerIDs builds the set of DHCP server identifiers (option 54)
// this worker treats as non-conflicting on the OOB segment: its own derived
// OOB address plus every valid owner address in the current snapshot's
// OOB.DynamicSlices. Missing/invalid owners are dropped (a gateway should
// never find itself excluded by a peer with a malformed owner field).
func oobAllowedServerIDs(localOOBIP net.IP, slices []model.OOBDynamicSlice) map[string]struct{} {
	allowed := make(map[string]struct{}, len(slices)+1)
	if ip := localOOBIP.To4(); ip != nil {
		allowed[ip.String()] = struct{}{}
	}
	for _, s := range slices {
		if ip := net.ParseIP(s.Owner).To4(); ip != nil {
			allowed[ip.String()] = struct{}{}
		}
	}
	return allowed
}

// oobOfferClass is the result of classifying one observed packet against the
// allowed server set.
type oobOfferClass uint8

const (
	// oobOfferIgnore is a packet that is not relevant to the probe (not an
	// OFFER, or from a different transaction) — keep observing.
	oobOfferIgnore oobOfferClass = iota
	// oobOfferAllowed is an OFFER whose option-54 server identifier is the
	// local OOB address or a valid dynamic-slice owner.
	oobOfferAllowed
	// oobOfferForeign is an OFFER from a foreign server, or one with a
	// missing/malformed option 54 (treated as foreign).
	oobOfferForeign
)

// classifyOOBOffer classifies a single observed packet against the allowed
// server set. Classification keys on DHCP option 54 (server identifier), NOT
// the packet's source IP — a server's reply may legitimately egress from a
// different address than the one it advertises.
func classifyOOBOffer(pkt *dhcpv4.DHCPv4, allowed map[string]struct{}) oobOfferClass {
	if pkt == nil {
		return oobOfferIgnore
	}
	if pkt.MessageType() != dhcpv4.MessageTypeOffer {
		return oobOfferIgnore
	}
	sid := pkt.ServerIdentifier()
	if sid == nil || sid.IsUnspecified() {
		return oobOfferForeign // missing or malformed option 54
	}
	if _, ok := allowed[sid.String()]; ok {
		return oobOfferAllowed
	}
	return oobOfferForeign
}

// oobProbeOutcome is the result of a single conflict probe.
type oobProbeOutcome uint8

const (
	probeOK oobProbeOutcome = iota
	probeConflict
	probeError
)

// evalOOBOffers reduces a stream of observed packets (from an injectable
// reader) into a probe outcome over the bounded observation window. The
// reader must eventually return a timeout-satisfying error to signal
// end-of-silence; any other error is a probe error. Mixed allowed + foreign
// responses are a conflict.
func evalOOBOffers(reader func() (*dhcpv4.DHCPv4, error), discover *dhcpv4.DHCPv4, allowed map[string]struct{}) (oobProbeOutcome, net.IP, error) {
	var sawForeign bool
	var foreignID net.IP
	for {
		pkt, err := reader()
		if err != nil {
			if isTimeoutErr(err) {
				break // silence through the bounded window == success
			}
			return probeError, nil, err
		}
		if pkt == nil {
			continue // unparseable / non-DHCP datagram — irrelevant
		}
		if pkt.TransactionID != discover.TransactionID || !bytes.Equal(pkt.ClientHWAddr, discover.ClientHWAddr) {
			continue // another client's session on the shared L2
		}
		switch classifyOOBOffer(pkt, allowed) {
		case oobOfferIgnore:
			continue // not our peer's session / not an OFFER
		case oobOfferAllowed:
			// An allowed response is compatible; only a foreign one is a
			// conflict. Mixed allowed + foreign still conflicts below.
		case oobOfferForeign:
			sawForeign = true
			if foreignID == nil {
				foreignID = pkt.ServerIdentifier()
			}
		}
	}
	if sawForeign {
		return probeConflict, foreignID, nil
	}
	return probeOK, nil, nil
}

// isTimeoutErr reports whether err is a net timeout (socket read deadline).
func isTimeoutErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// oobConflictGate is the process-lifetime latch for OOB DHCP conflict
// probing. Once latched (a foreign OFFER or a probe error), OOB DHCP is
// permanently suppressed for the rest of the process: no auto-resume, no
// acknowledgement API. The latch survives every standby VIP transition
// because it lives on siteServiceConfig, not on the per-acquisition handler.
type oobConflictGate struct {
	mu sync.RWMutex

	// latched is set once an outcome permanently suppresses OOB DHCP.
	latched bool
	// conflict is true when the latch came from a detected foreign server
	// (drives the slagworks_site_oob_dhcp_conflict gauge).
	conflict  bool
	foreignID net.IP
	errMsg    string

	// probeErrors counts fail-closed probe errors (monotonic counter).
	probeErrors atomic.Uint64
}

func newOOBConflictGate() *oobConflictGate { return &oobConflictGate{} }

// record applies a probe outcome to the latch. Probe errors always bump the
// error counter (even after a prior latch, so the counter is a true total);
// the latch itself is set only once.
func (g *oobConflictGate) record(outcome oobProbeOutcome, foreignID net.IP, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if outcome == probeError {
		g.probeErrors.Add(1)
	}
	if g.latched {
		return
	}
	switch outcome {
	case probeOK:
		// Silence within the window is success — nothing to latch.
	case probeConflict:
		g.latched = true
		g.conflict = true
		g.foreignID = foreignID
	case probeError:
		g.latched = true
		g.errMsg = "unknown probe error"
		if err != nil {
			g.errMsg = err.Error()
		}
	}
}

// suppressed reports whether OOB DHCP is latched off for this process.
func (g *oobConflictGate) suppressed() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.latched
}

// readyReason returns a stable, human-readable readiness reason when OOB
// DHCP is suppressed, or "" when the worker is ready on this front. It
// includes the foreign server identifier when one was observed.
func (g *oobConflictGate) readyReason() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if !g.latched {
		return ""
	}
	if g.conflict {
		if g.foreignID != nil && !g.foreignID.IsUnspecified() {
			return "foreign OOB DHCP server detected (server-id " + g.foreignID.String() + "); OOB DHCP suppressed"
		}
		return "foreign OOB DHCP server detected; OOB DHCP suppressed"
	}
	return "OOB DHCP conflict probe failed; OOB DHCP suppressed: " + g.errMsg
}

// oobConflictReadinessError is the readiness seam used by the real health
// closure and unit tests. Returning the gate's stable reason verbatim keeps
// /readyz useful to operators without coupling tests to the HTTP server.
func oobConflictReadinessError(g *oobConflictGate) error {
	if reason := g.readyReason(); reason != "" {
		return errors.New(reason)
	}
	return nil
}

// conflictGauge returns 1 when a foreign-server conflict latched the gate.
func (g *oobConflictGate) conflictGauge() float64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.latched && g.conflict {
		return 1
	}
	return 0
}

// probeErrorCount returns the number of fail-closed probe errors.
func (g *oobConflictGate) probeErrorCount() float64 {
	return float64(g.probeErrors.Load())
}

// oobOfferSink abstracts the raw packet I/O a conflict probe performs:
// transmitting the DISCOVER and observing OFFERs on the bound OOB interface.
// Production is ethOOBOfferSink (SO_BINDTODEVICE / IP_BOUND_IF); tests
// inject a fake so the decision logic is testable without a real NIC.
type oobOfferSink interface {
	// SendDiscover transmits the probe DISCOVER on the bound OOB L2 and
	// returns the packet (for its transaction id).
	SendDiscover() (*dhcpv4.DHCPv4, error)
	// ReadOffer returns the next observed DHCP packet, or nil,nil for an
	// unparseable datagram. It returns a timeout-satisfying error when the
	// probe's bounded observation window has elapsed (silence == success).
	ReadOffer() (*dhcpv4.DHCPv4, error)
	Close() error
}

// ethOOBOfferSink is the production probe I/O. It binds a UDP socket to the
// OOB interface on the DHCP client port (68), which forces both the
// broadcast DISCOVER egress and the OFFER ingress onto the intended OOB L2,
// independent of the host's default route — the same interface-binding the
// existing DHCP server uses to bind UDP/67. Unlike server4.NewIPv4UDPConn,
// this socket deliberately does not enable SO_REUSEADDR or SO_REUSEPORT. If
// another DHCP client owns UDP/68, probing must fail closed instead of letting
// the kernel distribute matching OFFERs between listeners and report false
// silence here.
type ethOOBOfferSink struct {
	conn     *net.UDPConn
	timeout  time.Duration
	deadline time.Time
	discover *dhcpv4.DHCPv4
	buf      []byte
}

func newEthOOBOfferSink(ni *net.Interface, subnet *net.IPNet, timeout time.Duration, _ *slog.Logger) (*ethOOBOfferSink, error) {
	discover, err := dhcpv4.NewDiscovery(syntheticOOBMAC(subnet), dhcpv4.WithBroadcast(true))
	if err != nil {
		return nil, fmt.Errorf("build OOB DHCP DISCOVER: %w", err)
	}
	conn, err := listenOOBProbeSocket(ni.Name, dhcpv4.ClientPort)
	if err != nil {
		return nil, fmt.Errorf("open OOB probe socket on %s: %w", ni.Name, err)
	}
	return &ethOOBOfferSink{
		conn:     conn,
		timeout:  timeout,
		discover: discover,
		buf:      make([]byte, 4096),
	}, nil
}

// listenOOBProbeSocket obtains exclusive ownership of port on iface. An empty
// iface is used only by the socket exclusivity unit test.
func listenOOBProbeSocket(iface string, port int) (*net.UDPConn, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var socketErr error
			if err := c.Control(func(fd uintptr) {
				if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1); err != nil {
					socketErr = fmt.Errorf("enable broadcast: %w", err)
					return
				}
				if iface == "" {
					return
				}
				if err := dhcpv4.BindToInterface(int(fd), iface); err != nil {
					socketErr = fmt.Errorf("bind to interface %s: %w", iface, err)
				}
			}); err != nil {
				return err
			}
			return socketErr
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return nil, err
	}
	conn, ok := pc.(*net.UDPConn)
	if !ok {
		_ = pc.Close()
		return nil, fmt.Errorf("unexpected packet connection %T", pc)
	}
	return conn, nil
}

func (s *ethOOBOfferSink) SendDiscover() (*dhcpv4.DHCPv4, error) {
	dst := &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpv4.ServerPort}
	if _, err := s.conn.WriteTo(s.discover.ToBytes(), dst); err != nil {
		return nil, fmt.Errorf("write OOB DHCP DISCOVER to %s: %w", dst, err)
	}
	return s.discover, nil
}

func (s *ethOOBOfferSink) ReadOffer() (*dhcpv4.DHCPv4, error) {
	if s.deadline.IsZero() {
		s.deadline = time.Now().Add(s.timeout)
	}
	if err := s.conn.SetReadDeadline(s.deadline); err != nil {
		return nil, fmt.Errorf("set OOB probe read deadline: %w", err)
	}
	n, _, err := s.conn.ReadFromUDP(s.buf)
	if err != nil {
		return nil, err
	}
	pkt, _ := dhcpv4.FromBytes(s.buf[:n])
	if pkt == nil {
		// Unparseable datagram on the L2 — irrelevant to the probe. A nil
		// return with no error is the sink's "keep observing this round"
		// contract; the timeout sentinel is the real end-of-window.
		return nil, nil
	}
	return pkt, nil
}

func (s *ethOOBOfferSink) Close() error { return s.conn.Close() }

// runOOBProbe executes one conflict probe against the sink and records its
// outcome on the process-lifetime gate. It returns true (allow the OOB DHCP
// bind) only on silence-within-timeout; a foreign OFFER or any error returns
// false and latches the gate.
func runOOBProbe(gate *oobConflictGate, sink oobOfferSink, allowed map[string]struct{}, log *slog.Logger) (allow bool) {
	discover, err := sink.SendDiscover()
	if err != nil {
		gate.record(probeError, nil, fmt.Errorf("send OOB DHCP DISCOVER: %w", err))
		log.Error("OOB DHCP conflict probe failed sending DISCOVER", "err", err)
		return false
	}
	outcome, sid, err := evalOOBOffers(sink.ReadOffer, discover, allowed)
	gate.record(outcome, sid, err)
	switch outcome {
	case probeOK:
		log.Info("OOB DHCP conflict probe clean: no OFFERs within timeout")
		return true
	case probeConflict:
		if sid != nil && !sid.IsUnspecified() {
			log.Error("OOB DHCP conflict: foreign server offered",
				"server_id", sid.String(), "client_mac", discover.ClientHWAddr.String())
		} else {
			log.Error("OOB DHCP conflict: foreign OFFER detected",
				"client_mac", discover.ClientHWAddr.String())
		}
		return false
	case probeError:
		log.Error("OOB DHCP conflict probe failed", "err", err)
		return false
	}
	return false
}

// bindOOBSegment drives one OOB-takeover step for a single bind attempt:
// consult the process-lifetime latch, run the conflict probe (injected), and
// start the OOB listeners. OOB DHCP is started only on a clean probe; the
// OOB TFTP/HTTP assist always starts, so a latched conflict suppresses DHCP
// alone rather than the whole OOB bundle. startDHCP/startAssist are injected
// so the decision path is unit-testable without binding sockets.
func bindOOBSegment(gate *oobConflictGate, probe func(oobBind, map[string]struct{}) bool, oob model.OOBConfig, bind oobBind, startDHCP, startAssist func(), log *slog.Logger) {
	allowed := oobAllowedServerIDs(bind.serverIP, oob.DynamicSlices)
	if gate.suppressed() {
		log.Error("OOB DHCP suppressed: conflict/error latched for this process",
			"reason", gate.readyReason())
		startAssist()
		return
	}
	if probe(bind, allowed) {
		startDHCP()
	} else {
		log.Error("OOB DHCP suppressed after conflict probe",
			"reason", gate.readyReason())
	}
	startAssist()
}

// registerOOBDHCPConflictMetrics wires the OOB DHCP conflict gauge and the
// probe-error counter into reg. Label-free (single series each), read from
// the process-lifetime gate at scrape time via GaugeFunc/CounterFunc —
// matching the repo's metric conventions (no per-event labels that could
// grow unboundedly).
func registerOOBDHCPConflictMetrics(reg prometheus.Registerer, gate *oobConflictGate) {
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "slagworks_site_oob_dhcp_conflict",
		Help: "1 when a foreign OOB DHCP server was detected and OOB DHCP has been suppressed for this process, else 0.",
	}, func() float64 { return gate.conflictGauge() }))
	reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name: "slagworks_site_oob_probe_errors_total",
		Help: "OOB DHCP conflict-probe errors that failed closed and suppressed OOB DHCP.",
	}, func() float64 { return gate.probeErrorCount() }))
}
