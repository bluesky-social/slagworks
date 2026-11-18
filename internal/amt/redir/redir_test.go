package redir

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // verifying the protocol's MD5 digest
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAMT is the AMT side of the redirection protocol, driven by the
// message formats in amtterm's RedirectionConstants.h and MeshCommander's
// amt-redir-ws: it parses every console->AMT frame strictly (so a malformed
// client frame fails the test) and scripts the AMT->console replies.
type fakeAMT struct {
	t    *testing.T
	user string
	pass string

	startStatus   byte   // StartRedirectionSessionReply status
	authTypes     []byte // offered in the auth-query reply
	solStatus     byte   // StartSOLRedirectionReply status
	hostOutput    [][]byte
	endAfterOut   bool // send EndSOLRedirectionReply after hostOutput
	junkAfterOut  bool // send an unknown message after hostOutput
	silent        bool // never answer (handshake timeout test)
	sendPing      bool // send a SOL_KEEP_ALIVE_PING after hostOutput
	controlsFirst bool // send SOL_CONTROLS_FROM_HOST before the SOL reply

	mu         sync.Mutex
	toHost     bytes.Buffer
	dataFrames int
	heartbeats int
	pingEchoed bool
	endSOL     bool
	endSession bool
	settings   []uint16
	lastSeq    uint32
	seqOK      bool
	handshake  error
}

func newFake(t *testing.T) *fakeAMT {
	return &fakeAMT{t: t, user: "admin", pass: "P@ssw0rd!", authTypes: []byte{1, 3, 4}, seqOK: true}
}

func (f *fakeAMT) fail(format string, args ...any) {
	f.mu.Lock()
	if f.handshake == nil {
		f.handshake = fmt.Errorf(format, args...)
	}
	f.mu.Unlock()
}

func readN(r io.Reader, n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(r, b)
	return b, err
}

func lp(fields ...string) []byte { return lpFields(fields...) }

func authReply(status, typ byte, data []byte) []byte {
	f := make([]byte, 9+len(data))
	f[0], f[1], f[4] = msgAuthenticateSessionReply, status, typ
	binary.LittleEndian.PutUint32(f[5:9], uint32(len(data))) //nolint:gosec // test
	copy(f[9:], data)
	return f
}

func (f *fakeAMT) readAuth(c net.Conn) (byte, []string, error) {
	hdr, err := readN(c, 9)
	if err != nil {
		return 0, nil, err
	}
	if hdr[0] != msgAuthenticateSession {
		return 0, nil, fmt.Errorf("want AuthenticateSession, got 0x%02x", hdr[0])
	}
	data, err := readN(c, int(binary.LittleEndian.Uint32(hdr[5:9])))
	if err != nil {
		return 0, nil, err
	}
	if hdr[4] == authQuery {
		if len(data) != 0 {
			return 0, nil, fmt.Errorf("auth query carries %d data bytes", len(data))
		}
		return authQuery, nil, nil
	}
	fields, err := splitLP(data, 8)
	if err != nil {
		return 0, nil, err
	}
	return hdr[4], fields, nil
}

const (
	fakeRealm = "Digest:2A6EE5E1E8C3B1B0B5A84F2C1F9A0DB1"
	fakeNonce = "kWd1Ewn8AAAAAAAA6zU5vwYz2vVeRQsy"
)

func md5hexT(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec // test
	return hex.EncodeToString(sum[:])
}

func (f *fakeAMT) serve(c net.Conn) {
	defer c.Close()
	if f.silent {
		_, _ = io.Copy(io.Discard, c)
		return
	}
	start, err := readN(c, 8)
	if err != nil || start[0] != msgStartRedirectionSession || string(start[4:]) != "SOL " {
		f.fail("start: %v %x", err, start)
		return
	}
	reply := []byte{msgStartRedirectionSessionReply, f.startStatus, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 2, 0xAA, 0xBB} // 2-byte OEM trailer
	_, _ = c.Write(reply)
	if f.startStatus != statusSuccess {
		return
	}

	typ, _, err := f.readAuth(c)
	if err != nil || typ != authQuery {
		f.fail("auth query: %v type %d", err, typ)
		return
	}
	_, _ = c.Write(authReply(statusSuccess, authQuery, f.authTypes))

	typ, fields, err := f.readAuth(c)
	if err != nil {
		return // client gave up (e.g. digest not offered)
	}
	if typ != authDigest || fields[0] != f.user || fields[1] != "" || fields[2] != "" || fields[3] != "/RedirectionService" {
		f.fail("digest init: type %d fields %q", typ, fields)
		return
	}
	_, _ = c.Write(authReply(statusAuthContinue, authDigest, lp(fakeRealm, fakeNonce, "auth")))

	typ, fields, err = f.readAuth(c)
	if err != nil || typ != authDigest {
		f.fail("digest response: %v type %d", err, typ)
		return
	}
	user, realm, nonce, uri, cnonce, nc, resp, qop := fields[0], fields[1], fields[2], fields[3], fields[4], fields[5], fields[6], fields[7]
	ha1 := md5hexT(user + ":" + realm + ":" + f.pass)
	ha2 := md5hexT("POST:" + uri)
	want := md5hexT(ha1 + ":" + nonce + ":" + nc + ":" + cnonce + ":" + qop + ":" + ha2)
	if user != f.user || realm != fakeRealm || nonce != fakeNonce || qop != "auth" || cnonce == "" || resp != want {
		_, _ = c.Write(authReply(statusAuthContinue, authDigest, nil))
		return
	}
	_, _ = c.Write(authReply(statusSuccess, authDigest, nil))

	sol, err := readN(c, 24)
	if err != nil || sol[0] != msgStartSOLRedirection {
		f.fail("start sol: %v %x", err, sol)
		return
	}
	f.mu.Lock()
	for i := range 6 {
		f.settings = append(f.settings, binary.LittleEndian.Uint16(sol[8+2*i:]))
	}
	f.lastSeq = binary.LittleEndian.Uint32(sol[4:8])
	f.mu.Unlock()
	if f.controlsFirst {
		_, _ = c.Write([]byte{msgSOLControlsFromHost, 0, 0, 0, 1, 0, 0, 0, 0x03, 0x04})
	}
	solReply := make([]byte, startSOLReplyLen)
	solReply[0], solReply[1] = msgStartSOLRedirectionReply, f.solStatus
	_, _ = c.Write(solReply)
	if f.solStatus != statusSuccess {
		return
	}

	go f.produce(c)
	f.consume(c)
}

// produce writes the scripted host output. Each frame is written in two
// pieces to prove the client reassembles frames split across TLS records.
func (f *fakeAMT) produce(c net.Conn) {
	for i, out := range f.hostOutput {
		frame := make([]byte, dataHeaderLen+len(out))
		frame[0] = msgSOLDataFromHost
		binary.LittleEndian.PutUint32(frame[4:8], uint32(i+1)) //nolint:gosec // test
		binary.LittleEndian.PutUint16(frame[8:10], uint16(len(out)))
		copy(frame[dataHeaderLen:], out)
		cut := len(frame) / 2
		_, _ = c.Write(frame[:cut])
		time.Sleep(5 * time.Millisecond)
		_, _ = c.Write(frame[cut:])
		_, _ = c.Write([]byte{msgSOLHeartbeat, 0, 0, 0, 9, 0, 0, 0})
	}
	if f.sendPing {
		_, _ = c.Write([]byte{msgSOLKeepAlivePing, 0, 0, 0, 0x55, 0, 0, 0})
	}
	if f.junkAfterOut {
		_, _ = c.Write([]byte{0x77, 0, 0, 0})
	}
	if f.endAfterOut {
		_, _ = c.Write([]byte{msgEndSOLRedirectionReply, 0, 0, 0, 0, 0, 0, 0})
	}
}

func (f *fakeAMT) consume(c net.Conn) {
	for {
		typ, err := readN(c, 1)
		if err != nil {
			return
		}
		f.mu.Lock()
		switch typ[0] {
		case msgSOLDataToHost:
			f.mu.Unlock()
			hdr, err := readN(c, dataHeaderLen-1)
			if err != nil {
				return
			}
			data, err := readN(c, int(binary.LittleEndian.Uint16(hdr[7:9])))
			if err != nil {
				return
			}
			f.mu.Lock()
			f.checkSeq(hdr[3:7])
			f.toHost.Write(data)
			f.dataFrames++
		case msgSOLHeartbeat, msgEndSOLRedirection:
			f.mu.Unlock()
			rest, err := readN(c, heartbeatLen-1)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.checkSeq(rest[3:7])
			if typ[0] == msgSOLHeartbeat {
				f.heartbeats++
			} else {
				f.endSOL = true
			}
		case msgSOLKeepAlivePing:
			f.mu.Unlock()
			rest, err := readN(c, heartbeatLen-1)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.pingEchoed = rest[3] == 0x55
		case msgEndRedirectionSession:
			f.mu.Unlock()
			if _, err := readN(c, 3); err != nil {
				return
			}
			f.mu.Lock()
			f.endSession = true
			f.mu.Unlock()
			return
		default:
			f.handshake = fmt.Errorf("unexpected console message 0x%02x mid-session", typ[0])
			f.mu.Unlock()
			return
		}
		f.mu.Unlock()
	}
}

// checkSeq requires console sequence numbers to strictly increase. Caller
// holds f.mu.
func (f *fakeAMT) checkSeq(b []byte) {
	seq := binary.LittleEndian.Uint32(b)
	if seq <= f.lastSeq {
		f.seqOK = false
	}
	f.lastSeq = seq
}

// listen starts a TLS listener (the shape AMT serves on :16995) running
// fake on each accepted connection; returns its "host:port".
func (f *fakeAMT) listen(t *testing.T) string {
	t.Helper()
	cert := httptest.NewUnstartedServer(nil)
	cert.StartTLS()
	cfg := &tls.Config{Certificates: cert.TLS.Certificates, MinVersion: tls.VersionTLS12}
	cert.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { f.serve(c) })
		}
	})
	return ln.Addr().String()
}

func readAll(t *testing.T, s *Session, want int) string {
	t.Helper()
	var got bytes.Buffer
	buf := make([]byte, 7) // small buffer: exercises partial reads of a frame
	deadline := time.After(3 * time.Second)
	for got.Len() < want {
		ch := make(chan struct{})
		var n int
		var err error
		go func() { n, err = s.Read(buf); close(ch) }()
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("timed out after %q", got.String())
		}
		got.Write(buf[:n])
		if err != nil {
			t.Fatalf("Read: %v (after %q)", err, got.String())
		}
	}
	return got.String()
}

func TestDialSOL_HappyPath(t *testing.T) {
	f := newFake(t)
	f.hostOutput = [][]byte{[]byte("iPXE 1.21.1 -- Open Source Network Boot\r\n"), []byte("net0: 58:47:ca:00:00:01\r\n")}
	f.sendPing = true
	f.controlsFirst = true
	addr := f.listen(t)

	s, err := DialSOL(context.Background(), addr, f.user, f.pass, Options{KeepAlive: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("DialSOL: %v", err)
	}
	want := "iPXE 1.21.1 -- Open Source Network Boot\r\nnet0: 58:47:ca:00:00:01\r\n"
	if got := readAll(t, s, len(want)); got != want {
		t.Errorf("host output = %q, want %q", got, want)
	}

	big := strings.Repeat("x", maxTxChunk+17)
	if n, err := s.Write([]byte("help\r" + big)); err != nil || n != 5+len(big) {
		t.Fatalf("Write = (%d, %v)", n, err)
	}
	time.Sleep(100 * time.Millisecond) // a few keepalive ticks
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close must be a no-op: %v", err)
	}
	if _, err := s.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("Read after Close = %v, want EOF", err)
	}

	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.endSession })
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handshake != nil {
		t.Fatalf("fake: %v", f.handshake)
	}
	if got := f.toHost.String(); got != "help\r"+big {
		t.Errorf("console input reached host as %d bytes, want %d", len(got), 5+len(big))
	}
	if f.dataFrames != 2 {
		t.Errorf("data frames = %d, want 2 (input split at the %d-byte MAX_TRANSMIT_BUFFER)", f.dataFrames, maxTxChunk)
	}
	if f.heartbeats < 2 {
		t.Errorf("heartbeats = %d, want >= 2 at a 20ms keepalive", f.heartbeats)
	}
	if !f.pingEchoed {
		t.Error("SOL_KEEP_ALIVE_PING was not echoed")
	}
	if !f.endSOL || !f.endSession {
		t.Errorf("close sent EndSOLRedirection=%v EndRedirectionSession=%v, want both", f.endSOL, f.endSession)
	}
	if !f.seqOK {
		t.Error("console sequence numbers must strictly increase")
	}
	wantSettings := []uint16{1000, 100, 0, 10000, 0, 5000}
	if fmt.Sprint(f.settings) != fmt.Sprint(wantSettings) {
		t.Errorf("SOL settings = %v, want %v", f.settings, wantSettings)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 300 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached within 3s")
}

func TestDialSOL_WrongPassword(t *testing.T) {
	f := newFake(t)
	addr := f.listen(t)
	_, err := DialSOL(context.Background(), addr, f.user, "wrong", Options{})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestDialSOL_HandshakeFailures(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*fakeAMT)
		wantSub string
	}{
		{"start refused", func(f *fakeAMT) { f.startStatus = 1 }, "start refused"},
		{"no digest offered", func(f *fakeAMT) { f.authTypes = []byte{1} }, "only HTTP digest"},
		{"sol refused", func(f *fakeAMT) { f.solStatus = 2 }, "start sol refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			tc.mutate(f)
			addr := f.listen(t)
			s, err := DialSOL(context.Background(), addr, f.user, f.pass, Options{})
			if err == nil {
				_ = s.Close()
				t.Fatal("DialSOL succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantSub)
			}
		})
	}
}

// TestDialSOL_HandshakeHonorsContext: an endpoint that accepts TLS but never
// answers must not hang the dial past ctx.
func TestDialSOL_HandshakeHonorsContext(t *testing.T) {
	f := newFake(t)
	f.silent = true
	addr := f.listen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := DialSOL(ctx, addr, f.user, f.pass, Options{}); err == nil {
		t.Fatal("want error from a silent endpoint")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("handshake took %v, want bounded by the 200ms ctx", time.Since(start))
	}
}

// TestSession_AMTEndsSession: EndSOLRedirectionReply from AMT ends the
// stream with a clean EOF after the buffered output.
func TestSession_AMTEndsSession(t *testing.T) {
	f := newFake(t)
	f.hostOutput = [][]byte{[]byte("reboot: Restarting system\r\n")}
	f.endAfterOut = true
	addr := f.listen(t)
	s, err := DialSOL(context.Background(), addr, f.user, f.pass, Options{})
	if err != nil {
		t.Fatalf("DialSOL: %v", err)
	}
	defer s.Close()
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "reboot: Restarting system\r\n" {
		t.Errorf("got %q", got)
	}
}

// TestSession_UnknownMessageIsAnError: an unparseable message can't be
// skipped (its length is unknown), so the stream ends with an error.
func TestSession_UnknownMessageIsAnError(t *testing.T) {
	f := newFake(t)
	f.hostOutput = [][]byte{[]byte("ok\r\n")}
	f.junkAfterOut = true
	addr := f.listen(t)
	s, err := DialSOL(context.Background(), addr, f.user, f.pass, Options{})
	if err != nil {
		t.Fatalf("DialSOL: %v", err)
	}
	defer s.Close()
	_, err = io.ReadAll(s)
	if err == nil || !strings.Contains(err.Error(), "0x77") {
		t.Fatalf("err = %v, want unknown-message error", err)
	}
}

// TestDigestResponse pins the response against an independently computed
// RFC 2617 value (qop=auth) and the RFC 2069 form (empty qop).
func TestDigestResponse(t *testing.T) {
	ha1 := md5hexT("admin:" + fakeRealm + ":pw")
	ha2 := md5hexT("POST:/RedirectionService")
	want := md5hexT(ha1 + ":" + fakeNonce + ":00000002:abc:auth:" + ha2)
	if got := DigestResponse("admin", "pw", fakeRealm, fakeNonce, "abc", "auth"); got != want {
		t.Errorf("qop=auth: got %s, want %s", got, want)
	}
	want = md5hexT(ha1 + ":" + fakeNonce + ":" + ha2)
	if got := DigestResponse("admin", "pw", fakeRealm, fakeNonce, "abc", ""); got != want {
		t.Errorf("no qop: got %s, want %s", got, want)
	}
}
