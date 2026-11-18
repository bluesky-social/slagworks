package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
)

// Server binds to a network interface and serves DHCP using a Handler.
type Server struct {
	handler  *Handler
	iface    string
	log      *slog.Logger
	srv      *server4.Server
	connHook func() (net.PacketConn, error)
	// relayDial, when non-nil, is used in place of net.DialUDP for
	// sending relay replies. The qemutest harness wires this to an
	// inNetns wrapper so the dial happens inside the test's network
	// namespace; production leaves it nil and the default DialUDP
	// runs in whatever netns the server lives in (host).
	relayDial func(dest *net.UDPAddr) (net.Conn, error)
}

func NewServer(iface string, handler *Handler) *Server {
	return &Server{
		handler: handler,
		iface:   iface,
		log:     handler.log,
	}
}

// SetConnHook injects a custom PacketConn factory used in place of
// the default server4 socket creation. The factory must return a
// conn already bound to the desired UDP/67 address. Used by the
// qemutest harness to open the socket inside a per-test netns via
// nsListenPacket.
//
// When set, iface (SO_BINDTODEVICE) is skipped — the netns
// isolation makes interface-binding redundant inside the test
// environment.
func (s *Server) SetConnHook(fn func() (net.PacketConn, error)) {
	s.connHook = fn
}

// SetRelayDial injects a custom Dial function used by
// sendRelayReply. Lets the qemutest harness route the relay reply
// through inNetns(networkNSName, ...). Production leaves this
// nil; the default uses net.DialUDP directly.
func (s *Server) SetRelayDial(fn func(dest *net.UDPAddr) (net.Conn, error)) {
	s.relayDial = fn
}

// ListenAndServe binds to the specified interface on port 67 and serves DHCP
// requests until the context is canceled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	addr := &net.UDPAddr{
		IP:   net.IPv4zero,
		Port: 67,
	}

	var srv *server4.Server
	var err error
	if s.connHook != nil {
		conn, herr := s.connHook()
		if herr != nil {
			return fmt.Errorf("dhcp: connHook: %w", herr)
		}
		srv, err = server4.NewServer("", nil, s.handle, server4.WithConn(conn))
	} else {
		srv, err = server4.NewServer(s.iface, addr, s.handle)
	}
	if err != nil {
		return fmt.Errorf("dhcp: failed to create server on %s: %w", s.iface, err)
	}
	s.srv = srv

	s.log.Info("DHCP server starting", "interface", s.iface, "addr", addr.String())

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve()
	}()

	select {
	case <-ctx.Done():
		if err := srv.Close(); err != nil {
			s.log.Error("closing DHCP server", "err", err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) handle(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
	// Pass our bound interface so the (shared) handler selects this
	// segment's per-interface config (serverIP/pool/DNS). For the
	// single-interface deployment s.iface is the one bound iface; the
	// handler falls back to its base config when no per-iface override is
	// registered for it.
	reply := s.handler.HandlePacketOn(req, s.iface)
	if reply == nil {
		return
	}

	if reply.GatewayIPAddr != nil && !reply.GatewayIPAddr.IsUnspecified() {
		if err := s.sendRelayReply(reply); err != nil {
			s.log.Error("failed to send relay reply",
				"err", err,
				"mac", req.ClientHWAddr.String(),
				"giaddr", reply.GatewayIPAddr.String(),
			)
		}
		return
	}

	if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
		s.log.Error("failed to send DHCP reply",
			"err", err,
			"mac", req.ClientHWAddr.String(),
			"peer", peer.String(),
		)
	}
}

// sendRelayReply sends a DHCP reply to a relay agent at giaddr:67 using a
// temporary socket. The main DHCP socket is bound to our serving interface
// via SO_BINDTODEVICE, which prevents it from routing to the relay's
// client-side address (giaddr) on a different network. A fresh socket lets
// the kernel route the reply via whatever path reaches the relay.
func (s *Server) sendRelayReply(reply *dhcpv4.DHCPv4) error {
	dest := &net.UDPAddr{IP: reply.GatewayIPAddr, Port: 67}
	var conn net.Conn
	var err error
	if s.relayDial != nil {
		conn, err = s.relayDial(dest)
	} else {
		conn, err = net.DialUDP("udp4", nil, dest)
	}
	if err != nil {
		return fmt.Errorf("dial relay %s: %w", dest, err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := conn.Write(reply.ToBytes()); err != nil {
		return fmt.Errorf("write to relay %s: %w", dest, err)
	}

	s.log.Info("relay reply sent",
		"mac", reply.ClientHWAddr.String(),
		"giaddr", reply.GatewayIPAddr.String(),
		"yiaddr", reply.YourIPAddr.String(),
	)
	return nil
}
