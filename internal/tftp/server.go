package tftp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	gotftp "github.com/pin/tftp/v3"
)

type Config struct {
	BindAddr string // e.g. "0.0.0.0:69" or ":6969" for tests
	ServeDir string // directory containing iPXE binaries

	// KnownIPs, when non-nil, filters TFTP requests by client IP.
	// Requests from IPs not returning true are dropped. Pair with
	// the DHCP handler's HasLease to prevent devices without active
	// DHCP leases from fetching boot files.
	// Note: filtering is bypassed when the remote address cannot be
	// determined (client does not implement OutgoingTransfer).
	KnownIPs func(ip string) bool

	// MachineForIP, when non-nil, resolves a client IP to (mac,
	// machine_id, hostname). Empty strings for any field that can't
	// be resolved. Used to scope TFTP log envelopes to the
	// requesting machine so per-machine views surface
	// PXE-phase fetch traffic.
	MachineForIP func(ip string) (mac, machineID, hostname string)

	// OnFetch, when non-nil, is called once per read request with the
	// outcome ("ok" | "denied_unknown_ip" | "not_found" | "error").
	// The site worker wires this to its Prometheus fetch counter; the
	// callback keeps this package free of a prometheus dependency.
	OnFetch func(outcome string)

	// Logger, when non-nil, is the server's structured logger. Nil
	// defaults to slog.Default().
	Logger *slog.Logger
}

// ReadHandler serves files from a directory. It is the unit-testable core
// of the TFTP server -- no socket dependency. It does not log; per-request
// logging lives on Server.readHandler.
type ReadHandler struct {
	dir string
}

func NewReadHandler(dir string) *ReadHandler {
	return &ReadHandler{dir: dir}
}

// Open validates the filename and returns a reader for the file. Returns an
// error for missing files, path traversal attempts, or empty filenames.
func (h *ReadHandler) Open(filename string) (io.ReadCloser, error) {
	if filename == "" {
		return nil, fmt.Errorf("tftp: empty filename")
	}
	if strings.Contains(filename, "..") || filepath.IsAbs(filename) {
		return nil, fmt.Errorf("tftp: rejected path traversal: %q", filename)
	}

	clean := filepath.Clean(filename)
	full := filepath.Join(h.dir, clean)

	// Double-check the resolved path is still within the serve directory
	// and is not the directory itself. Compare with filepath.Rel, which
	// cleans both sides: Join has already cleaned full, so a string-prefix
	// test against h.dir as configured would reject every file when the
	// directory is spelled "/srv/tftp/", "./ipxe" or ".".
	if rel, err := filepath.Rel(h.dir, full); err != nil || rel == "." || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("tftp: rejected path traversal: %q", filename)
	}

	f, err := os.Open(full)
	if err != nil {
		return nil, fmt.Errorf("tftp: open %q: %w", filename, err)
	}
	return f, nil
}

// Server wraps pin/tftp v3 and serves files from a directory.
type Server struct {
	cfg     Config
	handler *ReadHandler
	srv     *gotftp.Server
	log     *slog.Logger
}

func NewServer(cfg Config) *Server {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:     cfg,
		handler: NewReadHandler(cfg.ServeDir),
		log:     log,
	}
}

// ListenAndServe binds the configured UDP address and serves TFTP read
// requests until the context is canceled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	s.srv = gotftp.NewServer(s.readHandler, nil)
	// Cap the TFTP block size conservatively at 1408 and DISABLE the
	// library's MTU-based negotiation. 1468 (the old value) sits right at
	// the 1500-byte Ethernet edge: 1468 data + 4 TFTP + 8 UDP + 20 IP =
	// 1500 exactly, leaving zero headroom. Any encapsulation on the path to
	// the booting host (a VLAN tag is +4, and the deploy net is VLAN'd)
	// pushes the frame over MTU, so the oversized data block is silently
	// dropped — the firmware never ACKs, and the server's send retries
	// until it gives up ("read udp ... i/o timeout"). That is exactly the
	// flaky `snponly.efi` TFTP failure we see on the Supermicro boards.
	// 1408 leaves ~92 bytes of headroom (covers VLAN/QinQ/PPPoE/tunnels),
	// and disabling smart negotiation stops the lib from re-deriving a
	// path-unsafe size from the local interface MTU. Boot files are small;
	// the minor throughput cost is irrelevant next to not stalling. The
	// real fix is UEFI HTTP boot (see dhcp HTTPClient handling); this makes
	// the TFTP fallback robust for firmware that doesn't HTTP-boot.
	s.srv.SetBlockSizeNegotiation(false)
	s.srv.SetBlockSize(1408)

	s.log.Info("TFTP server starting", "addr", s.cfg.BindAddr, "dir", s.cfg.ServeDir)

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.srv.ListenAndServe(s.cfg.BindAddr)
	}()

	select {
	case <-ctx.Done():
		s.srv.Shutdown()
		return nil
	case err := <-errCh:
		return err
	}
}

func (s *Server) readHandler(filename string, rf io.ReaderFrom) error {
	// Get client address first so we can filter before opening the file.
	var remoteAddr string
	var remoteIP string
	if t, ok := rf.(gotftp.OutgoingTransfer); ok {
		addr := t.RemoteAddr()
		remoteAddr = addr.String()
		remoteIP = addr.IP.String()
	}

	// Per-request log: stamp filename + remote + (when known) MAC,
	// machine_id, hostname so the per-machine log view shows TFTP
	// activity during PXE. slogkit hoists machine_id to
	// envelope.MachineID; the rest land in extra.
	logAttrs := []any{
		"filename", filename,
		"remote", remoteAddr,
		"client_ip", remoteIP,
	}
	if s.cfg.MachineForIP != nil && remoteIP != "" {
		if mac, machineID, hostname := s.cfg.MachineForIP(remoteIP); machineID != "" {
			logAttrs = append(logAttrs,
				"client_mac", mac,
				"machine_id", machineID,
				"hostname", hostname,
			)
		}
	}
	log := s.log.With(logAttrs...)
	log.Info("TFTP request")

	countFetch := func(outcome string) {
		if s.cfg.OnFetch != nil {
			s.cfg.OnFetch(outcome)
		}
	}

	if s.cfg.KnownIPs != nil && remoteIP != "" && !s.cfg.KnownIPs(remoteIP) {
		log.Info("dropping TFTP request from unregistered IP")
		countFetch("denied_unknown_ip")
		return fmt.Errorf("tftp: access denied")
	}

	rc, err := s.handler.Open(filename)
	if err != nil {
		// Surface "file not found" as a distinct envelope so the
		// operator can grep for it without inferring from the
		// generic err message. This is the silent-loss class
		// of failure.
		if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
			log.Warn("TFTP not-found")
			countFetch("not_found")
		} else {
			log.Error("TFTP open failed", "err", err)
			countFetch("error")
		}
		return err
	}
	defer rc.Close()

	n, err := rf.ReadFrom(rc)
	if err != nil {
		log.Error("TFTP transfer failed", "err", err)
		countFetch("error")
		return err
	}

	log.Info("TFTP served", "bytes", n)
	countFetch("ok")
	return nil
}
