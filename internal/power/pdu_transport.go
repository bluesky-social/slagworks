package power

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshRedfishConn is the production pduConn: APC NMC CLI over SSH for
// actuation, Redfish over HTTPS for status reads. One per PDU host.
//
// Actuation uses Go's native crypto/ssh — NOT a shell-out. The one
// non-obvious requirement: the APC NMC's SSH server is cryptlib, which
// rejects Go's default client banner "SSH-2.0-Go" (its software-version
// token is too short for cryptlib's peer-ID length check) with
// SSH_DISCONNECT_PROTOCOL_ERROR. Setting ClientVersion to a longer
// banner (we mimic OpenSSH) is the entire fix; the negotiated algorithm
// suite is otherwise standard. See docs/dev/pdu-power-control.md.
//
// Native crypto/ssh keeps the password fully in-process (no argv/env
// leak — strictly better than the sshpass plumbing this replaced).
// Status reads use Go's net/http against the NMC's Redfish API.
type sshRedfishConn struct {
	host       string
	user, pass string
	httpClient *http.Client
	timeout    time.Duration
}

// newSSHRedfishConn is the production pduConnFactory.
func newSSHRedfishConn(host, user, pass string) pduConn {
	return &sshRedfishConn{
		host: host,
		user: user,
		pass: pass,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				// APC NMC serves a self-signed cert on the OOB segment;
				// same posture as BMC Redfish (curl -k). MITM detection
				// on the trusted OOB net is out of scope (doc §C).
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed PDU cert on trusted OOB net, see doc §C
			},
		},
		timeout: 30 * time.Second,
	}
}

// cli runs one APC CLI command over a fresh SSH session and returns the
// combined output. It parses the APC result code (E000/E1xx) and turns a
// non-success code into an error, so callers get a Go error on APC-level
// failure, not just transport failure.
//
// One command per session: the NMC's interactive CLI mangles multi-line
// input, and a single CombinedOutput is clean. cryptlib closes the exec
// channel WITHOUT an exit-status message, so session.CombinedOutput
// returns a benign "exited without exit status" error even on success —
// we ignore the exec error and rely entirely on the parsed APC E-code.
func (c *sshRedfishConn) cli(ctx context.Context, command string) (string, error) {
	dialTimeout := 10 * time.Second
	cfg := &ssh.ClientConfig{
		User: c.user,
		Auth: []ssh.AuthMethod{ssh.Password(c.pass)},
		// PDU is a replaceable OOB appliance; key-pinning would break on
		// swap, and the OOB segment is trusted (doc §C).
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // replaceable OOB appliance on trusted net, see doc §C
		// REQUIRED: the APC NMC runs cryptlib, which rejects the default
		// "SSH-2.0-Go" banner (software-version token too short) with a
		// protocol-error disconnect. Any longer banner works; mimic
		// OpenSSH for maximum appliance compatibility. This single line
		// is what makes native crypto/ssh work against the NMC.
		ClientVersion: "SSH-2.0-OpenSSH_8.9",
		Timeout:       dialTimeout,
		// Pin the cipher the NMC offers, for determinism across firmware.
		// KEX/MAC/host-key are left at x/crypto defaults (the server's
		// ecdh-sha2-nistp256 / hmac-sha2-256 / ecdsa-sha2-nistp256 are
		// all in the default sets and negotiate fine).
		Config: ssh.Config{Ciphers: []string{"aes256-ctr"}},
	}

	// Append the default SSH port only if the host has none. Use
	// SplitHostPort (not a naive colon check) so a bare IPv6 literal
	// isn't mistaken for host:port.
	addr := c.host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		// Trim any existing brackets before JoinHostPort re-adds them,
		// so an already-bracketed IPv6 literal isn't double-bracketed.
		addr = net.JoinHostPort(strings.Trim(addr, "[]"), "22")
	}
	conn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("pdu %s dial: %w", c.host, err)
	}
	// Bound the SSH handshake: ssh.NewClientConn takes no context, so a
	// server that accepts TCP but stalls the handshake (a wedged NMC)
	// would block forever — and the ctx watchdog below only covers the
	// command phase. A conn deadline caps the handshake; we clear it on
	// success so it doesn't then cut the command short.
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("pdu %s ssh handshake: %w", c.host, err)
	}
	_ = conn.SetDeadline(time.Time{}) // clear; the watchdog now governs cancellation
	client := ssh.NewClient(sshConn, chans, reqs)
	defer func() { _ = client.Close() }()

	// Watchdog: close the client if ctx is cancelled mid-command so the
	// session call unblocks (crypto/ssh has no per-session context).
	cmdCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		cmdCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-cmdCtx.Done():
			_ = client.Close()
		case <-stop:
		}
	}()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("pdu %s session: %w", c.host, err)
	}
	defer func() { _ = sess.Close() }()

	// IGNORE the exec error: cryptlib sends no exit-status, so this
	// always returns "exited without exit status" even on success. The
	// APC E-code parsed from output is the real signal.
	out, _ := sess.CombinedOutput(command)
	output := string(out)
	if strings.TrimSpace(output) == "" {
		if cmdCtx.Err() != nil {
			return "", fmt.Errorf("pdu %s %q: %w", c.host, command, cmdCtx.Err())
		}
		return "", fmt.Errorf("pdu %s %q: no output (transport failure)", c.host, command)
	}
	if err := parseAPCResult(output); err != nil {
		return output, fmt.Errorf("pdu %s %q: %w", c.host, command, err)
	}
	return output, nil
}

// outletState reads one outlet's switched state via Redfish
// Oem.SwitchedState ("On"/"Off"). The APC OEM tree (not standard DMTF
// PowerEquipment) is at /redfish/v1/DCIMPower/default/RackPDU/0/Outlets/<n>.
func (c *sshRedfishConn) outletState(ctx context.Context, outlet int) (State, error) {
	// Bracket a bare IPv6 literal so the URL host is valid
	// (https://[2001:db8::1]/...). IPv4 / hostnames pass through unchanged.
	urlHost := c.host
	if _, _, err := net.SplitHostPort(urlHost); err != nil {
		if ip := net.ParseIP(urlHost); ip != nil && strings.Contains(urlHost, ":") {
			urlHost = "[" + urlHost + "]"
		}
	}
	path := fmt.Sprintf("https://%s/redfish/v1/DCIMPower/default/RackPDU/0/Outlets/%d", urlHost, outlet)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return StateUnknown, fmt.Errorf("pdu %s build request: %w", c.host, err)
	}
	req.SetBasicAuth(c.user, c.pass)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return StateUnknown, fmt.Errorf("pdu %s redfish get outlet %d: %w", c.host, outlet, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return StateUnknown, fmt.Errorf("pdu %s redfish outlet %d: HTTP %d: %s", c.host, outlet, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Oem struct {
			SwitchedState string `json:"SwitchedState"`
		} `json:"Oem"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return StateUnknown, fmt.Errorf("pdu %s redfish outlet %d: parse: %w", c.host, outlet, err)
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Oem.SwitchedState)) {
	case "on":
		return StateOn, nil
	case "off":
		return StateOff, nil
	default:
		return StateUnknown, nil
	}
}

// close releases the HTTP transport's idle keep-alive connections.
// Each conn owns its own *http.Transport; without this the idle Redfish
// connections (and their reader/writer goroutines + FDs) would linger
// after the controller is closed.
func (c *sshRedfishConn) close() error {
	if t, ok := c.httpClient.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
	return nil
}
