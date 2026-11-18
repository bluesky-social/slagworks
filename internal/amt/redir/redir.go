// Package redir is a native Intel AMT redirection-protocol client, limited
// to serial-over-LAN (SOL). It speaks the binary protocol AMT serves on TLS
// port 16995: StartRedirectionSession("SOL "), HTTP-digest session
// authentication, StartSOLRedirection with session settings, then a
// bidirectional stream of SOL data frames plus heartbeats, and an orderly
// EndSOLRedirection / EndRedirectionSession on close.
//
// References (no maintained Go implementation exists):
//   - amtterm redir.c / RedirectionConstants.h (Gerd Hoffmann,
//     https://github.com/kraxel/amtterm): message numbers, frame layouts and
//     lengths, the SOL session-settings values (MAX_TRANSMIT_BUFFER 1000,
//     TRANSMIT_BUFFER_TIMEOUT 100, HOST_SESSION_RX_TIMEOUT 10000,
//     HEARTBEAT_INTERVAL 5000), and the SOL_CONTROLS_FROM_HOST frame.
//   - MeshCommander amt-redir-ws-0.1.0.js (Ylian Saint-Hilaire,
//     https://github.com/Ylianst/MeshCommander): the authentication query
//     (auth type 0) and the HTTP-digest exchange (auth type 4, uri
//     "/RedirectionService", qop=auth, nc 00000002), the OEM-length-
//     terminated StartRedirectionSessionReply, and the console-side
//     heartbeat (0x2B with a sequence number every 2s), which modern AMT
//     needs inside HOST_SESSION_RX_TIMEOUT to keep an idle session open.
//   - Intel AMT SDK, "Redirection Library" / "SOL and IDE-R protocol"
//     chapters, which both of the above implement.
//
// All multi-byte integers on the wire are little-endian.
package redir

import (
	"bufio"
	"context"
	"crypto/md5" //nolint:gosec // AMT redirection digest auth is MD5 by protocol
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Port is AMT's TLS redirection port. The plaintext :16994 is never used:
// the session carries the host console and the digest exchange.
const Port = 16995

// Message types (amtterm RedirectionConstants.h).
const (
	msgStartRedirectionSession      = 0x10
	msgStartRedirectionSessionReply = 0x11
	msgEndRedirectionSession        = 0x12
	msgAuthenticateSession          = 0x13
	msgAuthenticateSessionReply     = 0x14
	msgStartSOLRedirection          = 0x20
	msgStartSOLRedirectionReply     = 0x21
	msgEndSOLRedirection            = 0x22
	msgEndSOLRedirectionReply       = 0x23
	msgSOLKeepAlivePing             = 0x24
	msgSOLKeepAlivePong             = 0x25
	msgSOLDataToHost                = 0x28
	msgSOLControlsFromHost          = 0x29
	msgSOLDataFromHost              = 0x2A
	msgSOLHeartbeat                 = 0x2B
)

const (
	statusSuccess = 0
	// statusAuthContinue is AuthenticateSessionReply's "not yet": the
	// digest challenge (realm/nonce/qop) rides in the auth data.
	statusAuthContinue = 1

	authQuery  = 0
	authDigest = 4

	authURI = "/RedirectionService"
	// digestNC is the fixed nonce count: each session authenticates
	// exactly once against a fresh nonce (MeshCommander uses the same).
	digestNC = "00000002"

	startSOLReplyLen = 23 // amtterm START_SOL_REDIRECTION_REPLY_LENGTH
	heartbeatLen     = 8
	controlsLen      = 10
	dataHeaderLen    = 10
	// maxTxChunk is the per-frame console->host payload cap; it matches the
	// MAX_TRANSMIT_BUFFER this client advertises in the session settings.
	maxTxChunk = 1000
	// maxAuthData bounds the auth-reply allocation against a corrupt length.
	maxAuthData = 4096
)

// SOL session settings sent in StartSOLRedirection (amtterm's values).
const (
	settingMaxTxBuffer       = maxTxChunk
	settingTxBufferTimeout   = 100
	settingTxOverflowTimeout = 0
	settingHostRxTimeout     = 10000
	settingHostRxFlush       = 0
	settingHeartbeatInterval = 5000
)

// DefaultKeepAlive is how often the console sends SOL_HEARTBEAT. It must
// stay well inside the 10s HOST_SESSION_RX_TIMEOUT advertised above, or AMT
// drops an idle session.
const DefaultKeepAlive = 2 * time.Second

// ErrAuth is returned (wrapped) when AMT rejects the digest credential.
var ErrAuth = errors.New("amt redirection: authentication rejected")

// Options tunes a session. The zero value is production-ready.
type Options struct {
	// KeepAlive overrides DefaultKeepAlive (tests).
	KeepAlive time.Duration
}

// Session is one live AMT SOL redirection session. Read returns host
// serial output; Write sends console input to the host. Close ends the
// SOL and redirection sessions and closes the connection; it is safe to
// call concurrently with Read/Write and more than once. Read returns io.EOF
// after Close or after AMT ends the session.
type Session struct {
	conn net.Conn
	br   *bufio.Reader

	writeMu sync.Mutex
	seq     uint32 // console->AMT sequence number; guarded by writeMu

	pr *io.PipeReader
	pw *io.PipeWriter

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
}

// DialSOL connects to the AMT redirection endpoint at host (bare IP gets
// Port; "host:port" overrides it, for test fakes) over TLS and starts an
// authenticated SOL session. ctx bounds the dial and handshake only.
//
// TLS verification is off: AMT presents a self-signed certificate minted at
// provisioning with no CA to pin. This matches the WS-Man path (iamt) and
// the trust model there: the endpoint is pinned by NetBox's oob_ip and the
// digest response is not replayable.
func DialSOL(ctx context.Context, host, user, pass string, opts Options) (*Session, error) {
	addr := host
	if _, _, err := net.SplitHostPort(host); err != nil {
		addr = net.JoinHostPort(host, strconv.Itoa(Port))
	}
	d := tls.Dialer{Config: &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // AMT self-signed cert; see DialSOL doc
		MinVersion:         tls.VersionTLS12,
	}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("amt redirection dial %s: %w", addr, err)
	}
	s, err := StartSOL(ctx, conn, user, pass, opts)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return s, nil
}

// StartSOL runs the redirection + SOL handshake over an established
// connection and returns the running session. On error the caller still
// owns conn.
func StartSOL(ctx context.Context, conn net.Conn, user, pass string, opts Options) (*Session, error) {
	if user == "" || pass == "" {
		return nil, errors.New("amt redirection: incomplete credential")
	}
	if len(user) > 255 || len(pass) > 255 {
		return nil, errors.New("amt redirection: username/password longer than 255 bytes")
	}
	keepAlive := opts.KeepAlive
	if keepAlive <= 0 {
		keepAlive = DefaultKeepAlive
	}
	s := &Session{conn: conn, br: bufio.NewReader(conn), done: make(chan struct{})}

	// Bound the handshake by ctx; clear the deadline once streaming.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	err := s.handshake(user, pass)
	stopped := stop()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("amt redirection handshake: %w (%w)", err, ctx.Err())
		}
		return nil, err
	}
	if !stopped {
		return nil, fmt.Errorf("amt redirection handshake: %w", ctx.Err())
	}
	_ = conn.SetDeadline(time.Time{})

	s.pr, s.pw = io.Pipe()
	s.wg.Add(2)
	go s.readLoop()
	go s.keepAliveLoop(keepAlive)
	return s, nil
}

func (s *Session) handshake(user, pass string) error {
	// StartRedirectionSession("SOL ").
	if err := s.write([]byte{msgStartRedirectionSession, 0, 0, 0, 'S', 'O', 'L', ' '}); err != nil {
		return fmt.Errorf("amt redirection: send start: %w", err)
	}
	f, err := s.readFrame()
	if err != nil {
		return fmt.Errorf("amt redirection: start reply: %w", err)
	}
	if f[0] != msgStartRedirectionSessionReply {
		return fmt.Errorf("amt redirection: start: unexpected message 0x%02x", f[0])
	}
	if f[1] != statusSuccess {
		return fmt.Errorf("amt redirection: start refused (status %d): SOL may be disabled in AMT or another console holds the session", f[1])
	}

	// Query the supported authentication methods.
	if err := s.write(authFrame(authQuery, nil)); err != nil {
		return fmt.Errorf("amt redirection: send auth query: %w", err)
	}
	status, typ, data, err := s.readAuthReply()
	if err != nil {
		return err
	}
	if typ != authQuery || status != statusSuccess {
		return fmt.Errorf("amt redirection: auth query: unexpected reply (type %d, status %d)", typ, status)
	}
	if !containsByte(data, authDigest) {
		return fmt.Errorf("amt redirection: AMT offers auth types %v; only HTTP digest (4) is supported", data)
	}

	// Digest round 1: user + uri, empty realm/nonce/cnonce/nc/response/qop,
	// to obtain the challenge.
	if err := s.write(authFrame(authDigest, lpFields(user, "", "", authURI, "", "", "", ""))); err != nil {
		return fmt.Errorf("amt redirection: send digest init: %w", err)
	}
	status, typ, data, err = s.readAuthReply()
	if err != nil {
		return err
	}
	if typ != authDigest || status != statusAuthContinue {
		return fmt.Errorf("amt redirection: digest challenge: unexpected reply (type %d, status %d)", typ, status)
	}
	ch, err := splitLP(data, 3)
	if err != nil {
		return fmt.Errorf("amt redirection: digest challenge: %w", err)
	}
	realm, nonce, qop := ch[0], ch[1], ch[2]

	// Digest round 2: the response.
	cnonce, err := randomHex(16)
	if err != nil {
		return err
	}
	resp := DigestResponse(user, pass, realm, nonce, cnonce, qop)
	if err := s.write(authFrame(authDigest, lpFields(user, realm, nonce, authURI, cnonce, digestNC, resp, qop))); err != nil {
		return fmt.Errorf("amt redirection: send digest response: %w", err)
	}
	status, _, _, err = s.readAuthReply()
	if err != nil {
		return err
	}
	if status != statusSuccess {
		return fmt.Errorf("%w (status %d): check the device's kind=amt bmc-creds-admin entry", ErrAuth, status)
	}

	// StartSOLRedirection with the session settings.
	start := make([]byte, 24)
	start[0] = msgStartSOLRedirection
	for i, v := range []uint16{settingMaxTxBuffer, settingTxBufferTimeout, settingTxOverflowTimeout,
		settingHostRxTimeout, settingHostRxFlush, settingHeartbeatInterval} {
		binary.LittleEndian.PutUint16(start[8+2*i:], v)
	}
	if err := s.writeSeq(start); err != nil {
		return fmt.Errorf("amt redirection: send start sol: %w", err)
	}
	// AMT may report its COM-port control lines before the reply.
	for {
		f, err = s.readFrame()
		if err != nil {
			return fmt.Errorf("amt redirection: start sol reply: %w", err)
		}
		if f[0] == msgSOLControlsFromHost || f[0] == msgSOLHeartbeat {
			continue
		}
		break
	}
	if f[0] != msgStartSOLRedirectionReply {
		return fmt.Errorf("amt redirection: start sol: unexpected message 0x%02x", f[0])
	}
	if f[1] != statusSuccess {
		return fmt.Errorf("amt redirection: start sol refused (status %d): SOL may be disabled in AMT", f[1])
	}
	return nil
}

// readAuthReply reads one AuthenticateSessionReply: status, auth type, data.
func (s *Session) readAuthReply() (byte, byte, []byte, error) {
	f, err := s.readFrame()
	if err != nil {
		return 0, 0, nil, fmt.Errorf("amt redirection: auth reply: %w", err)
	}
	if f[0] != msgAuthenticateSessionReply {
		return 0, 0, nil, fmt.Errorf("amt redirection: auth: unexpected message 0x%02x", f[0])
	}
	return f[1], f[4], f[9:], nil
}

// readFrame reads one complete AMT->console message. Lengths per message
// type follow amtterm/MeshCommander; an unknown type is a protocol error
// (the stream can't be resynchronized without its length).
func (s *Session) readFrame() ([]byte, error) {
	typ, err := s.br.ReadByte()
	if err != nil {
		return nil, err
	}
	var hdr int
	switch typ {
	case msgStartRedirectionSessionReply:
		hdr = 13
	case msgAuthenticateSessionReply:
		hdr = 9
	case msgStartSOLRedirectionReply:
		hdr = startSOLReplyLen
	case msgEndSOLRedirectionReply, msgSOLKeepAlivePing, msgSOLKeepAlivePong, msgSOLHeartbeat:
		hdr = heartbeatLen
	case msgSOLControlsFromHost:
		hdr = controlsLen
	case msgSOLDataFromHost:
		hdr = dataHeaderLen
	default:
		return nil, fmt.Errorf("unknown AMT redirection message 0x%02x", typ)
	}
	f := make([]byte, hdr)
	f[0] = typ
	if _, err := io.ReadFull(s.br, f[1:]); err != nil {
		return nil, err
	}
	var extra int
	switch typ {
	case msgStartRedirectionSessionReply:
		extra = int(f[12]) // OEM-defined trailer
	case msgAuthenticateSessionReply:
		n := binary.LittleEndian.Uint32(f[5:9])
		if n > maxAuthData {
			return nil, fmt.Errorf("auth reply data length %d exceeds %d", n, maxAuthData)
		}
		extra = int(n)
	case msgSOLDataFromHost:
		extra = int(binary.LittleEndian.Uint16(f[8:10]))
	}
	if extra == 0 {
		return f, nil
	}
	full := make([]byte, hdr+extra)
	copy(full, f)
	if _, err := io.ReadFull(s.br, full[hdr:]); err != nil {
		return nil, err
	}
	return full, nil
}

func (s *Session) write(b []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.conn.Write(b)
	return err
}

// writeSeq stamps the next sequence number into b[4:8] and writes it.
func (s *Session) writeSeq(b []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.seq++
	binary.LittleEndian.PutUint32(b[4:8], s.seq)
	_, err := s.conn.Write(b)
	return err
}

// readLoop demultiplexes AMT->console frames: host serial bytes go to the
// pipe Read drains; keepalive pings are echoed (amtterm); heartbeats,
// pongs, and control-line reports are consumed; EndSOLRedirectionReply or
// any read error ends the stream.
func (s *Session) readLoop() {
	defer s.wg.Done()
	err := s.demux()
	select {
	case <-s.done:
		// We closed the session: surface a clean end, not net.ErrClosed.
		_ = s.pw.Close()
	default:
		if errors.Is(err, io.EOF) {
			_ = s.pw.Close()
		} else {
			_ = s.pw.CloseWithError(fmt.Errorf("amt redirection: %w", err))
		}
	}
}

func (s *Session) demux() error {
	for {
		f, err := s.readFrame()
		if err != nil {
			return err
		}
		switch f[0] {
		case msgSOLDataFromHost:
			if _, err := s.pw.Write(f[dataHeaderLen:]); err != nil {
				return err
			}
		case msgSOLKeepAlivePing:
			if err := s.write(f); err != nil {
				return err
			}
		case msgEndSOLRedirectionReply:
			return io.EOF
		case msgSOLHeartbeat, msgSOLKeepAlivePong, msgSOLControlsFromHost:
		default:
			return fmt.Errorf("unexpected AMT redirection message 0x%02x mid-session", f[0])
		}
	}
}

func (s *Session) keepAliveLoop(every time.Duration) {
	defer s.wg.Done()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if err := s.writeSeq(seqFrame(msgSOLHeartbeat)); err != nil {
				return
			}
		}
	}
}

// Read returns host serial output.
func (s *Session) Read(p []byte) (int, error) { return s.pr.Read(p) }

// Write sends console input to the host as SOL data frames.
func (s *Session) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxTxChunk)
		f := make([]byte, dataHeaderLen+n)
		f[0] = msgSOLDataToHost
		binary.LittleEndian.PutUint16(f[8:10], uint16(n)) //nolint:gosec // n <= maxTxChunk
		copy(f[dataHeaderLen:], p[:n])
		if err := s.writeSeq(f); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// Close ends the SOL and redirection sessions (best effort, bounded by a
// short write deadline so a dead peer can't hang it) and closes the
// connection. Freeing the session explicitly matters: AMT allows one SOL
// session at a time, and an abandoned one holds the slot until AMT's own
// receive timeout fires.
func (s *Session) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = s.writeSeq(seqFrame(msgEndSOLRedirection))
		_ = s.write([]byte{msgEndRedirectionSession, 0, 0, 0})
		err = s.conn.Close()
		_ = s.pr.Close()
		s.wg.Wait()
	})
	return err
}

// seqFrame returns an 8-byte type+reserved+sequence frame (heartbeat,
// EndSOLRedirection); writeSeq fills the sequence number.
func seqFrame(typ byte) []byte {
	f := make([]byte, heartbeatLen)
	f[0] = typ
	return f
}

// DigestResponse computes the HTTP-digest (qop=auth) response AMT expects
// for the redirection service: MD5(HA1:nonce:nc:cnonce:qop:HA2) with
// HA1 = MD5(user:realm:pass) and HA2 = MD5("POST:/RedirectionService").
// An empty qop selects the RFC 2069 form MD5(HA1:nonce:HA2).
func DigestResponse(user, pass, realm, nonce, cnonce, qop string) string {
	ha1 := md5hex(user + ":" + realm + ":" + pass)
	ha2 := md5hex("POST:" + authURI)
	if qop == "" {
		return md5hex(ha1 + ":" + nonce + ":" + ha2)
	}
	return md5hex(ha1 + ":" + nonce + ":" + digestNC + ":" + cnonce + ":" + qop + ":" + ha2)
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // protocol-mandated
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("amt redirection: cnonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// authFrame builds an AuthenticateSession message: type, reserved, auth
// type, uint32 data length, data.
func authFrame(authType byte, data []byte) []byte {
	f := make([]byte, 9+len(data))
	f[0] = msgAuthenticateSession
	f[4] = authType
	binary.LittleEndian.PutUint32(f[5:9], uint32(len(data))) //nolint:gosec // bounded by 8 length-prefixed <=255-byte fields
	copy(f[9:], data)
	return f
}

// lpFields encodes strings as consecutive 1-byte-length-prefixed fields.
func lpFields(fields ...string) []byte {
	var out []byte
	for _, f := range fields {
		out = append(out, byte(len(f)))
		out = append(out, f...)
	}
	return out
}

// splitLP decodes n consecutive 1-byte-length-prefixed fields.
func splitLP(b []byte, n int) ([]string, error) {
	out := make([]string, 0, n)
	for range n {
		if len(b) < 1 || len(b) < 1+int(b[0]) {
			return nil, errors.New("truncated length-prefixed field")
		}
		l := int(b[0])
		out = append(out, string(b[1:1+l]))
		b = b[1+l:]
	}
	return out, nil
}

func containsByte(b []byte, v byte) bool {
	for _, x := range b {
		if x == v {
			return true
		}
	}
	return false
}
