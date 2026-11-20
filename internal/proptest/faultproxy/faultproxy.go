// Package faultproxy is a test-only TCP proxy that sits between an HTTP or
// WebSocket client and server and injects the faults a reverse proxy and a
// load balancer produce in production: idle timeouts (both directions or one),
// connection drops with FIN or RST, half-closed connections, blackholed
// connections that stay open but deliver nothing in one or both directions,
// frozen connections that stop reading, upgrade refusals (429/502/503),
// proxy restarts, and arbitrary splitting of the byte stream.
//
// It parses only the first request head on each connection (the WebSocket
// upgrade), records it, and from then on copies bytes. It is not a general
// HTTP proxy: keep-alive requests after the first are copied unparsed.
package faultproxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Request is the first request head the proxy saw on a connection.
type Request struct {
	Method     string
	RequestURI string // as sent on the request line, path and query
	Header     http.Header
	Host       string
}

// Proxy is a fault-injecting TCP proxy. The zero value is not usable; use New.
type Proxy struct {
	target string

	mu      sync.Mutex
	ln      net.Listener
	addr    string
	pairs   map[*pair]struct{}
	pending map[net.Conn]struct{} // accepted, upgrade request not yet forwarded
	refuse  []int                 // status codes for the next upgrades, consumed in order
	reqs    []Request
	silence int // the next silence accepted connections go silent after the upgrade
	closed  bool
	acceptW sync.WaitGroup

	// Settings read by pumps on every chunk.
	chunkMax   atomic.Int64 // >0: split forwarded data into writes of 1..chunkMax bytes
	chunkDelay atomic.Int64 // nanoseconds slept between split writes
	idleBoth   atomic.Int64 // nanoseconds; >0: drop a pair idle in both directions this long
	idleDown   atomic.Int64 // nanoseconds; >0: drop a pair the server has not written to this long
	maxLife    atomic.Int64 // nanoseconds; >0: drop a pair this long after it opened
	seed       atomic.Uint64

	accepted atomic.Int64
	refused  atomic.Int64
	dropped  atomic.Int64 // pairs dropped by an injected fault (not by an endpoint)
	down     atomic.Int64 // server-to-client bytes forwarded, all pairs
	up       atomic.Int64 // client-to-server bytes forwarded, all pairs
}

type pair struct {
	p        *Proxy
	client   net.Conn
	upstream net.Conn
	opened   time.Time

	lastUp   atomic.Int64 // unix nanos of last client->server byte
	lastDown atomic.Int64 // unix nanos of last server->client byte

	blackUp   atomic.Bool  // read and discard client->server
	blackDown atomic.Bool  // read and discard server->client
	downShut  atomic.Bool  // client got a FIN: discard server->client, keep client->server
	afterHead atomic.Bool  // go silent both ways once the upgrade response has passed
	freeze    atomic.Bool  // stop reading in both directions
	eofs      atomic.Int32 // bit 1: client->server EOF, bit 2: server->client EOF

	once sync.Once
	done chan struct{}
}

// New starts a proxy on 127.0.0.1 forwarding to target ("host:port").
func New(target string) (*Proxy, error) {
	p := &Proxy{target: target, pairs: map[*pair]struct{}{}, pending: map[net.Conn]struct{}{}}
	p.seed.Store(1)
	if err := p.listen("127.0.0.1:0"); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Proxy) listen(addr string) error {
	var ln net.Listener
	var err error
	// A restart re-binds the same port; retry briefly in case the old
	// listener's socket has not been released yet.
	for i := 0; i < 50; i++ {
		ln, err = net.Listen("tcp", addr)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.ln = ln
	p.addr = ln.Addr().String()
	p.mu.Unlock()
	p.acceptW.Add(1)
	go p.acceptLoop(ln)
	return nil
}

// Addr is the proxy's listen address.
func (p *Proxy) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addr
}

// URL is the proxy's address as an http:// base URL.
func (p *Proxy) URL() string { return "http://" + p.Addr() }

// SetSeed seeds the chunk-size randomness so a run is reproducible.
func (p *Proxy) SetSeed(s uint64) { p.seed.Store(s) }

// SetChunking splits forwarded data into writes of 1..max bytes with delay
// between them. max <= 0 disables splitting.
func (p *Proxy) SetChunking(max int, delay time.Duration) {
	p.chunkMax.Store(int64(max))
	p.chunkDelay.Store(int64(delay))
}

// SetIdleTimeout drops a connection after d with no bytes in either
// direction, the way an AWS ALB applies its idle timeout. 0 disables.
func (p *Proxy) SetIdleTimeout(d time.Duration) { p.idleBoth.Store(int64(d)) }

// SetDownstreamIdleTimeout drops a connection after d with no bytes from the
// server to the client, the way nginx applies proxy_read_timeout to a
// WebSocket. 0 disables.
func (p *Proxy) SetDownstreamIdleTimeout(d time.Duration) { p.idleDown.Store(int64(d)) }

// SetMaxLifetime drops every connection d after it opened (a connection
// reuse or lifetime limit). 0 disables.
func (p *Proxy) SetMaxLifetime(d time.Duration) { p.maxLife.Store(int64(d)) }

// SilenceNextAfterUpgrade lets the next n connections complete the HTTP
// upgrade and then deliver nothing in either direction, staying open: a
// path that dies between the upgrade and the first WebSocket message.
func (p *Proxy) SilenceNextAfterUpgrade(n int) {
	p.mu.Lock()
	p.silence += n
	p.mu.Unlock()
}

// RefuseNext answers the next len(codes) connections with those HTTP status
// codes (429, 502, 503, ...) without contacting the server.
func (p *Proxy) RefuseNext(codes ...int) {
	p.mu.Lock()
	p.refuse = append(p.refuse, codes...)
	p.mu.Unlock()
}

// Requests returns every first request head seen so far.
func (p *Proxy) Requests() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Request(nil), p.reqs...)
}

// Stats are cumulative counters.
type Stats struct {
	Accepted, Refused, Dropped, Open int64
	UpBytes, DownBytes               int64
}

func (p *Proxy) Stats() Stats {
	p.mu.Lock()
	open := int64(len(p.pairs))
	p.mu.Unlock()
	return Stats{
		Accepted: p.accepted.Load(), Refused: p.refused.Load(), Dropped: p.dropped.Load(), Open: open,
		UpBytes: p.up.Load(), DownBytes: p.down.Load(),
	}
}

func (p *Proxy) snapshot() []*pair {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*pair, 0, len(p.pairs))
	for pr := range p.pairs {
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].opened.Before(out[j].opened) })
	return out
}

// Open reports how many proxied connections are currently open.
func (p *Proxy) Open() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pairs)
}

// DropAll closes every open connection on both sides with a FIN.
func (p *Proxy) DropAll() int { return p.drop(false) }

// ResetAll closes every open connection on both sides with an RST.
func (p *Proxy) ResetAll() int { return p.drop(true) }

func (p *Proxy) drop(rst bool) int {
	prs := p.snapshot()
	for _, pr := range prs {
		pr.kill(rst)
	}
	// A connection still sending its upgrade request is open too: closing
	// it ends its handler, which otherwise waits out the request deadline.
	p.mu.Lock()
	for c := range p.pending {
		_ = c.Close()
	}
	p.mu.Unlock()
	return len(prs)
}

// HalfCloseDownAll sends a FIN to every client (the server-to-client
// direction ends) while keeping the client-to-server direction open and
// the server side connected, the way a proxy that half-closes behaves.
func (p *Proxy) HalfCloseDownAll() int {
	prs := p.snapshot()
	for _, pr := range prs {
		if tc, ok := pr.client.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		pr.downShut.Store(true) // nothing more reaches the client
	}
	return len(prs)
}

// BlackholeAll makes every open connection deliver nothing in either
// direction while staying open and acknowledging what it reads: a path
// that silently stopped working with no FIN or RST.
func (p *Proxy) BlackholeAll() int { return p.blackhole(true, true) }

// BlackholeUpAll is BlackholeAll for the client-to-server direction only:
// the server hears nothing more, the client still hears the server.
func (p *Proxy) BlackholeUpAll() int { return p.blackhole(true, false) }

// BlackholeDownAll is BlackholeAll for the server-to-client direction only.
func (p *Proxy) BlackholeDownAll() int { return p.blackhole(false, true) }

func (p *Proxy) blackhole(up, down bool) int {
	prs := p.snapshot()
	for _, pr := range prs {
		if up {
			pr.blackUp.Store(true)
		}
		if down {
			pr.blackDown.Store(true)
		}
	}
	return len(prs)
}

// FreezeAll makes every open connection stop reading in both directions
// (a wedged intermediary); senders block once socket buffers fill.
func (p *Proxy) FreezeAll() int {
	prs := p.snapshot()
	for _, pr := range prs {
		pr.freeze.Store(true)
	}
	return len(prs)
}

// Restart drops every connection with an RST, stops listening for down,
// then listens on the same address again: a proxy rollout.
func (p *Proxy) Restart(down time.Duration) error {
	p.mu.Lock()
	ln := p.ln
	addr := p.addr
	p.mu.Unlock()
	_ = ln.Close()
	p.acceptW.Wait()
	p.ResetAll()
	if down > 0 {
		time.Sleep(down)
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return errors.New("proxy closed")
	}
	return p.listen(addr)
}

// Close stops the proxy and closes every connection.
func (p *Proxy) Close() {
	p.mu.Lock()
	p.closed = true
	ln := p.ln
	p.mu.Unlock()
	_ = ln.Close()
	p.acceptW.Wait()
	p.ResetAll()
}

func (p *Proxy) acceptLoop(ln net.Listener) {
	defer p.acceptW.Done()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		p.accepted.Add(1)
		go p.handle(c)
	}
}

func (p *Proxy) handle(client net.Conn) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = client.Close()
		return
	}
	p.pending[client] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, client)
		p.mu.Unlock()
	}()
	br := bufio.NewReader(client)
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	req, err := http.ReadRequest(br)
	_ = client.SetReadDeadline(time.Time{})
	if err != nil {
		_ = client.Close()
		return
	}
	hdr := req.Header.Clone()
	host := req.Host
	p.mu.Lock()
	p.reqs = append(p.reqs, Request{Method: req.Method, RequestURI: req.RequestURI, Header: hdr.Clone(), Host: host})
	var code int
	silent := false
	if len(p.refuse) > 0 {
		code = p.refuse[0]
		p.refuse = p.refuse[1:]
	} else if p.silence > 0 {
		p.silence--
		silent = true
	}
	p.mu.Unlock()
	if code != 0 {
		p.refused.Add(1)
		body := http.StatusText(code)
		fmt.Fprintf(client, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nRetry-After: 1\r\nConnection: close\r\n\r\n%s",
			code, http.StatusText(code), len(body), body)
		_ = client.Close()
		return
	}

	upstream, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		fmt.Fprintf(client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		_ = client.Close()
		return
	}
	var head strings.Builder
	fmt.Fprintf(&head, "%s %s HTTP/1.1\r\nHost: %s\r\n", req.Method, req.RequestURI, host)
	_ = hdr.Write(&head)
	head.WriteString("\r\n")

	now := time.Now()
	pr := &pair{p: p, client: client, upstream: upstream, opened: now, done: make(chan struct{})}
	pr.lastUp.Store(now.UnixNano())
	pr.lastDown.Store(now.UnixNano())
	pr.afterHead.Store(silent)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	p.pairs[pr] = struct{}{}
	p.mu.Unlock()

	if _, err := io.WriteString(upstream, head.String()); err != nil {
		pr.kill(false)
		return
	}
	// Anything already buffered after the head (none for an upgrade, but
	// keep the stream intact) goes first.
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		if _, err := upstream.Write(b); err != nil {
			pr.kill(false)
			return
		}
		_, _ = br.Discard(n)
	}
	go pr.pump(client, upstream, false)
	go pr.pump(upstream, client, true)
	go pr.watch()
}

func (pr *pair) rng() *rand.Rand {
	s := pr.p.seed.Add(0x9e3779b97f4a7c15)
	return rand.New(rand.NewPCG(s, s^0xdeadbeef)) //nolint:gosec // reproducible test chunking, not security
}

// silenced reports whether the given direction currently delivers nothing.
func (pr *pair) silenced(down bool) bool {
	if down {
		return pr.blackDown.Load() || pr.downShut.Load()
	}
	return pr.blackUp.Load()
}

// pump copies src to dst. down is true for server-to-client.
func (pr *pair) pump(src, dst net.Conn, down bool) {
	r := pr.rng()
	buf := make([]byte, 32<<10)
	var head []byte // server-to-client bytes so far, until the upgrade response has ended
	for {
		for pr.freeze.Load() {
			select {
			case <-pr.done:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
		n, err := src.Read(buf)
		if n > 0 {
			nowN := time.Now().UnixNano()
			if pr.silenced(down) {
				// Read and acknowledge, deliver nothing.
				continue
			}
			if werr := pr.write(dst, buf[:n], r); werr != nil {
				pr.kill(false)
				return
			}
			if down && pr.afterHead.Load() {
				// The server writes nothing after its upgrade response
				// until the client's first message arrives, so the
				// response ends the chunk that completes it.
				head = append(head, buf[:n]...)
				if strings.Contains(string(head), "\r\n\r\n") {
					pr.afterHead.Store(false)
					pr.blackUp.Store(true)
					pr.blackDown.Store(true)
				}
			}
			if down {
				pr.lastDown.Store(nowN)
				pr.p.down.Add(int64(n))
			} else {
				pr.lastUp.Store(nowN)
				pr.p.up.Add(int64(n))
			}
		}
		if err != nil {
			if pr.silenced(down) {
				// A silent direction does not propagate a close either
				// (and after a half-close the client already has its
				// FIN). The pair ends once both endpoints have gone.
				pr.halfDone(down)
				return
			}
			// Propagate the half-close the endpoint sent, then wait for the
			// other direction to end the pair.
			if tc, ok := dst.(*net.TCPConn); ok && errors.Is(err, io.EOF) {
				_ = tc.CloseWrite()
				pr.halfDone(down)
				return
			}
			pr.kill(false)
			return
		}
	}
}

func (pr *pair) write(dst net.Conn, b []byte, r *rand.Rand) error {
	max := int(pr.p.chunkMax.Load())
	if max <= 0 {
		_, err := dst.Write(b)
		return err
	}
	delay := time.Duration(pr.p.chunkDelay.Load())
	for len(b) > 0 {
		k := 1 + r.IntN(max)
		if k > len(b) {
			k = len(b)
		}
		if _, err := dst.Write(b[:k]); err != nil {
			return err
		}
		b = b[k:]
		if delay > 0 && len(b) > 0 {
			time.Sleep(delay)
		}
	}
	return nil
}

// halfDone records that one direction ended with an EOF; when both have,
// the pair is finished.
func (pr *pair) halfDone(down bool) {
	bit := int32(1)
	if down {
		bit = 2
	}
	for {
		old := pr.eofs.Load()
		if pr.eofs.CompareAndSwap(old, old|bit) {
			if old|bit == 3 {
				pr.kill(false)
			}
			return
		}
	}
}

func (pr *pair) watch() {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-pr.done:
			return
		case <-t.C:
		}
		now := time.Now()
		if d := time.Duration(pr.p.maxLife.Load()); d > 0 && now.Sub(pr.opened) >= d {
			pr.p.dropped.Add(1)
			pr.kill(false)
			return
		}
		up := time.Unix(0, pr.lastUp.Load())
		dn := time.Unix(0, pr.lastDown.Load())
		if d := time.Duration(pr.p.idleBoth.Load()); d > 0 {
			last := up
			if dn.After(last) {
				last = dn
			}
			if now.Sub(last) >= d {
				pr.p.dropped.Add(1)
				pr.kill(false)
				return
			}
		}
		if d := time.Duration(pr.p.idleDown.Load()); d > 0 && now.Sub(dn) >= d {
			pr.p.dropped.Add(1)
			pr.kill(false)
			return
		}
	}
}

func (pr *pair) kill(rst bool) {
	pr.once.Do(func() {
		if rst {
			for _, c := range []net.Conn{pr.client, pr.upstream} {
				if tc, ok := c.(*net.TCPConn); ok {
					_ = tc.SetLinger(0)
				}
			}
		}
		_ = pr.client.Close()
		_ = pr.upstream.Close()
		close(pr.done)
		pr.p.mu.Lock()
		delete(pr.p.pairs, pr)
		pr.p.mu.Unlock()
	})
}
