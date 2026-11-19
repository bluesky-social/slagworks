package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/amt/redir"
	"github.com/bluesky-social/slagworks/internal/model"
)

// CastFrame is one batch of bytes captured from a machine's serial console,
// destined for global's console_cast_events table.
//
// TUs is monotonic-per-cast microseconds, computed by the pump as
// `prev_max_us + monotonic_since_pump_start` and bumped to one past the
// pump's previous frame when that would not advance it. Strict
// monotonicity must be preserved across pump restarts and site-host
// clock changes: the console stream pages with cursor = t_us + 1.
type CastFrame struct {
	MachineID uuid.UUID
	TUs       int64
	Data      []byte
}

// CastSink consumes CastFrames produced by the pump. Production
// wires this to the relay forwarder (TypeCastBatch); tests substitute
// a slice-collecting fake.
type CastSink interface {
	PushCastFrame(CastFrame)
}

// CastResumeSource fetches the resume offset for a machine when a
// pump starts. In production this is an RPC to global; in tests a
// fake returns whatever value the test set.
type CastResumeSource interface {
	ResumeOffsetUs(ctx context.Context, machineID uuid.UUID) (int64, error)
}

// CastNoiseFilter strips a transport's own chrome (banners, teardown
// messages) out of the console byte stream before it reaches the cast.
// Streaming: Filter may hold back a partial match across calls; Flush
// returns whatever is held at end of session. Not safe for concurrent use.
type CastNoiseFilter interface {
	Filter(p []byte) []byte
	Flush() []byte
}

// CastSession is one live console session from a CastDialer.
type CastSession struct {
	// Stream carries host serial output (Read) and console input (Write).
	// Close ends the session and releases everything behind it (the
	// ipmitool process and its pty, or the AMT redirection connection);
	// it must be idempotent, safe concurrently with Read, and must make a
	// blocked Read return. The pump closes it when the session ends,
	// including on ctx cancel and the silence watchdog.
	Stream io.ReadWriteCloser
	// Noise is the transport's chrome filter, fresh per session; nil
	// passes bytes through untouched.
	Noise CastNoiseFilter
	// NoSilenceWatchdog exempts the session from the silence watchdog.
	// Set by transports that cannot go quiet while still "connected"
	// (a local serial tty): there a silent stream means a silent console
	// (a running switch prints nothing for hours), so a forced reconnect
	// would only churn. Such sessions end on read error/EOF alone.
	NoSilenceWatchdog bool
}

// CastDialer opens a console session for a machine. Each transport owns
// its noise filter: ipmitool prints activation/teardown chrome into its
// pty, AMT redirection carries only host bytes.
type CastDialer interface {
	Dial(ctx context.Context, machineID uuid.UUID) (CastSession, error)
}

// CastPumpConfig wires a per-machine pump.
type CastPumpConfig struct {
	MachineID uuid.UUID
	Dialer    CastDialer
	Resume    CastResumeSource
	Sink      CastSink
	Logger    *slog.Logger

	// FlushInterval bounds how long bytes can sit in the buffer
	// before being emitted as a frame. Default 100ms.
	FlushInterval time.Duration
	// FlushSize bounds how many bytes can accumulate before being
	// emitted. Default 4096.
	FlushSize int
	// ReadBufSize is the chunk size handed to pty.Read; bigger
	// chunks reduce syscalls, smaller chunks bound batching latency.
	// Default 4096.
	ReadBufSize int
	// ReconnectBackoff is the BASE sleep between an ipmitool exit and
	// the next dial. Default 1s. When sessions die faster than
	// FastFailThreshold (a busy BMC SOL slot, the AMI ~5-slot trap),
	// the actual sleep grows exponentially from this base toward
	// MaxReconnectBackoff; a healthy-length session resets it.
	ReconnectBackoff time.Duration
	// MaxReconnectBackoff caps the exponential growth applied on
	// repeated fast-fails. Default 60s.
	MaxReconnectBackoff time.Duration
	// FastFailThreshold classifies a session as a "fast fail" (slot
	// collision / immediate exit) when its wall-clock duration is below
	// this. Real SOL sessions run for minutes; collisions die in <100ms,
	// so the two are well separated. A fast fail increments the backoff
	// streak; any session at or above the threshold resets it. Default 2s.
	FastFailThreshold time.Duration

	// SilenceThreshold is how long a session may go without any bytes
	// from the BMC before the silence watchdog kills it for a
	// proactive reconnect. The IPMI session can stay up at the network
	// layer (UDP keepalives flowing) while the BMC's serial side has
	// hung; from the pump's view the pty just goes quiet forever.
	// Zero disables the watchdog. Default 5m (set by NewCastPump only
	// when StateFunc is non-nil — without a state gate we can't tell
	// "BMC hung" from "host legitimately powered off").
	SilenceThreshold time.Duration
	// SilenceCheckEvery is how often the watchdog re-checks the
	// last-byte time. Default 30s.
	SilenceCheckEvery time.Duration
	// StateFunc reports the machine's current lifecycle state, used to
	// gate the silence watchdog: we only force-reconnect when the
	// chassis SHOULD be producing console output (running/deploying/
	// commissioning/commissioned). Nil disables the watchdog — without
	// a state gate, a silent powered-off host would churn the BMC slot
	// forever. Status of a watchdog reconnect is logged via the pump's
	// Logger (a source=site envelope), never injected into the cast.
	StateFunc func() string
}

func (c *CastPumpConfig) defaults() {
	if c.FlushInterval == 0 {
		c.FlushInterval = 100 * time.Millisecond
	}
	if c.FlushSize == 0 {
		c.FlushSize = 4096
	}
	if c.ReadBufSize == 0 {
		c.ReadBufSize = 4096
	}
	if c.ReconnectBackoff == 0 {
		c.ReconnectBackoff = time.Second
	}
	if c.MaxReconnectBackoff == 0 {
		c.MaxReconnectBackoff = 60 * time.Second
	}
	if c.FastFailThreshold == 0 {
		c.FastFailThreshold = 2 * time.Second
	}
	// Watchdog defaults only apply when a state gate is present; see
	// StateFunc doc. Without StateFunc the watchdog stays off.
	if c.StateFunc != nil {
		if c.SilenceThreshold == 0 {
			c.SilenceThreshold = 5 * time.Minute
		}
		if c.SilenceCheckEvery == 0 {
			c.SilenceCheckEvery = 30 * time.Second
		}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// CastPump runs a per-machine console cast capture loop over a CastDialer
// (ipmitool SOL or AMT redirection). Run blocks until ctx is canceled; on
// each session end it sleeps ReconnectBackoff and re-dials. Bytes from each session are
// monotonically appended to the same logical cast (the prev_max_us
// pulled from Resume on each session start ensures this).
type CastPump struct {
	cfg CastPumpConfig

	// lastTUs is the t_us of the last frame this pump emitted, across all
	// of its sessions. Only flush touches it, always under the current
	// session's mu; sessions run one at a time and each joins its ticker
	// goroutine before returning, so no two sessions overlap on it.
	lastTUs int64
}

// NewCastPump constructs a pump but does not start it. Call Run.
func NewCastPump(cfg CastPumpConfig) *CastPump {
	cfg.defaults()
	return &CastPump{cfg: cfg}
}

// Run drives the dial → batch → sink loop until ctx is canceled.
// Errors during dialing or pty I/O are logged and the pump retries
// after a backoff that grows exponentially across consecutive
// "fast fails" (sessions that die faster than FastFailThreshold) and
// resets after any healthy-length session.
//
// The fast-fail backoff exists because AMI BMCs expose only ~5 SOL
// session slots; a flat retry into a not-yet-freed slot collides,
// ipmitool exits in tens of ms, and a tight 1s loop both floods the
// journal and prevents the slot from ever clearing. Growing the
// backoff (with jitter, so a fleet doesn't resync) lets the slot
// drain and turns a per-second log storm into a trickle.
func (p *CastPump) Run(ctx context.Context) {
	log := p.cfg.Logger.With("machine_id", p.cfg.MachineID, "component", "castpump")

	failStreak := 0   // consecutive fast-fails; drives exponential backoff
	unstable := false // whether we've WARNed about this run of fast-fails

	for {
		if ctx.Err() != nil {
			return
		}

		started := time.Now()
		err := p.session(ctx, log)
		sessionDur := time.Since(started)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Debug("castpump session ended with error", "err", err, "session_dur", sessionDur.Truncate(time.Millisecond))
		}
		if ctx.Err() != nil {
			return
		}

		if sessionDur < p.cfg.FastFailThreshold {
			failStreak++
			// One WARN when a pump crosses into "unstable" — surfaces a
			// stuck BMC SOL slot without the per-cycle INFO storm.
			if failStreak == 3 && !unstable {
				unstable = true
				// last_err surfaces the cause (a busy BMC SOL slot, a
				// missing or locked console tty) without raising the
				// per-session debug log.
				log.Warn("castpump: repeated fast session failures, backing off (BMC SOL slot busy or console device unavailable)",
					"fail_streak", failStreak, "last_session_dur", sessionDur.Truncate(time.Millisecond), "last_err", err)
			}
		} else {
			// A healthy-length session: clear the penalty and announce
			// recovery exactly once.
			if unstable {
				log.Info("castpump: session recovered, backoff reset", "after_fail_streak", failStreak)
			}
			failStreak = 0
			unstable = false
		}

		backoff := applyJitter(nextBackoff(p.cfg.ReconnectBackoff, p.cfg.MaxReconnectBackoff, failStreak), 0.2)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// nextBackoff returns base << streak, clamped to [base, max]. A streak
// of 0 yields base; each increment doubles the delay until it saturates
// at max. Negative streaks are treated as 0 (defensive). The shift is
// bounded so a large streak can't overflow before the clamp applies.
func nextBackoff(base, max time.Duration, streak int) time.Duration {
	if streak <= 0 {
		return base
	}
	d := base
	for range streak {
		d *= 2
		if d >= max || d <= 0 { // d<=0 guards against shift overflow
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

// applyJitter adds up to frac*d of positive jitter to d, using a
// best-effort random source. Jitter is always non-negative so a backoff
// never shrinks below its computed floor; the spread desynchronizes a
// fleet of pumps that would otherwise retry in lockstep.
func applyJitter(d time.Duration, frac float64) time.Duration {
	return applyJitterWith(d, frac, randFloat())
}

// applyJitterWith is the deterministic core of applyJitter, taking the
// random draw r ∈ [0,1) as a parameter so tests can sweep it.
func applyJitterWith(d time.Duration, frac float64, r float64) time.Duration {
	if frac <= 0 || d <= 0 {
		return d
	}
	return d + time.Duration(r*frac*float64(d))
}

// randFloat returns a pseudo-random float in [0,1) for backoff jitter.
// math/rand/v2 is auto-seeded and goroutine-safe; jitter doesn't need
// crypto-strength randomness.
func randFloat() float64 { return rand.Float64() } //nolint:gosec // G404: backoff jitter, not security-sensitive

// session runs one console session end-to-end: resolve resume offset,
// dial, batch bytes, flush on rules, close the stream. Returns when the
// stream ends (clean or error) or ctx is canceled.
func (p *CastPump) session(ctx context.Context, log *slog.Logger) error {
	prevMaxUs, err := p.cfg.Resume.ResumeOffsetUs(ctx, p.cfg.MachineID)
	if err != nil {
		return fmt.Errorf("resume offset: %w", err)
	}
	log.Debug("castpump session start", "prev_max_us", prevMaxUs)

	// Session-scoped context: cancelling it (pump shutdown, the silence
	// watchdog) closes the stream, which is what unblocks a Read stuck on
	// a hung transport (an ipmitool pty, a quiet AMT TLS connection).
	sessionCtx, cancelSessionProc := context.WithCancel(ctx)
	defer cancelSessionProc()

	sess, err := p.cfg.Dialer.Dial(sessionCtx, p.cfg.MachineID)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	stream := sess.Stream
	stopCloseOnCancel := context.AfterFunc(sessionCtx, func() { _ = stream.Close() })
	defer stopCloseOnCancel()
	// Close is idempotent; this releases the transport (reaps ipmitool,
	// ends the AMT session) on every return path.
	defer func() { _ = stream.Close() }()
	noise := sess.Noise
	if noise == nil {
		noise = passthroughNoise{}
	}

	pumpStart := time.Now()
	var (
		mu        sync.Mutex // guards buf
		buf       bytes.Buffer
		batchOpen time.Time
	)

	// lastByte tracks when we last read bytes off the pty, for the
	// silence watchdog. Updated by the read loop (lock-free), read by
	// the watchdog goroutine. Seed to now so a slow first byte doesn't
	// trip the watchdog immediately.
	lastByte := &atomic.Int64{}
	lastByte.Store(time.Now().UnixNano())

	// flush emits whatever's currently in buf as one CastFrame and
	// resets state. Caller must hold mu.
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		data := make([]byte, buf.Len())
		copy(data, buf.Bytes())
		buf.Reset()
		// Two flushes inside one microsecond (a backlog drained one full
		// read after another, or the ticker flush racing a full read)
		// read the same elapsed time, and a resume offset that lags this
		// pump's own frames (still in flight to global) can start a
		// session behind them. Either would repeat or rewind t_us, so
		// step one past the previous frame instead until the clock
		// passes it again.
		t := time.Since(pumpStart).Microseconds() + prevMaxUs
		if t <= p.lastTUs {
			t = p.lastTUs + 1
		}
		p.lastTUs = t
		p.cfg.Sink.PushCastFrame(CastFrame{
			MachineID: p.cfg.MachineID,
			TUs:       t,
			Data:      data,
		})
	}
	flushLocked := func() {
		mu.Lock()
		flush()
		mu.Unlock()
	}

	// Periodic ticker drives the FlushInterval rule; the read loop
	// drives the FlushSize rule. Both terminate on ctx.Done() or pty
	// EOF.
	ctxSession, cancelSession := context.WithCancel(ctx)
	defer cancelSession()

	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		t := time.NewTicker(p.cfg.FlushInterval)
		defer t.Stop()
		for {
			select {
			case <-ctxSession.Done():
				return
			case <-t.C:
				mu.Lock()
				if buf.Len() > 0 && time.Since(batchOpen) >= p.cfg.FlushInterval {
					flush()
				}
				mu.Unlock()
			}
		}
	}()

	// Silence watchdog: if no bytes arrive for SilenceThreshold AND
	// the machine state says output should be flowing, cancel the
	// session proc so Run re-dials. Guards the "UDP session alive but
	// BMC serial hung" failure mode. Status goes to the pump logger
	// (source=site envelope), never into the cast. Disabled when
	// StateFunc is nil (defaults() leaves the threshold at 0).
	if p.cfg.SilenceThreshold > 0 && p.cfg.SilenceCheckEvery > 0 && p.cfg.StateFunc != nil && !sess.NoSilenceWatchdog {
		go p.runSilenceWatchdog(ctxSession, log, lastByte, cancelSessionProc)
	}

	// Read loop. noise strips the transport's own chrome before it reaches
	// the buffer, so none of it ever enters the stored cast; it may hold a
	// sub-pattern-length tail across reads, flushed on exit.
	readBuf := make([]byte, p.cfg.ReadBufSize)
	var readErr error
	for {
		n, rerr := stream.Read(readBuf)
		if n > 0 {
			lastByte.Store(time.Now().UnixNano())
			clean := noise.Filter(readBuf[:n])
			if len(clean) > 0 {
				mu.Lock()
				if buf.Len() == 0 {
					batchOpen = time.Now()
				}
				buf.Write(clean)
				if buf.Len() >= p.cfg.FlushSize {
					flush()
				}
				mu.Unlock()
			}
		}
		if rerr != nil {
			if !isStreamEnd(rerr) {
				readErr = rerr
			}
			break
		}
	}

	cancelSession()
	<-tickerDone
	if tail := noise.Flush(); len(tail) > 0 {
		mu.Lock()
		buf.Write(tail)
		mu.Unlock()
	}
	flushLocked()
	// Close before returning (the deferred Close is then a no-op): for
	// ipmitool this SIGKILLs and reaps the child, for AMT it ends the
	// redirection session and frees AMT's single SOL slot.
	_ = stream.Close()
	if readErr != nil {
		return fmt.Errorf("console read: %w", readErr)
	}
	log.Debug("castpump session end")
	return nil
}

// isStreamEnd reports whether a stream Read error is an ordinary end of
// session rather than a fault: EOF, a closed stream (our own Close racing
// the Read, e.g. on ctx cancel or the silence watchdog), or the EIO a pty
// master returns once its child exits.
func isStreamEnd(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) || isPtyClosedErr(err)
}

// passthroughNoise is the CastNoiseFilter for transports that inject no
// chrome into the console stream (AMT redirection).
type passthroughNoise struct{}

func (passthroughNoise) Filter(p []byte) []byte { return p }
func (passthroughNoise) Flush() []byte          { return nil }

// runSilenceWatchdog cancels the session if the BMC has been silent
// longer than SilenceThreshold while StateFunc reports a state in
// which the chassis should be producing serial output. Cancelling
// cancelSessionProc closes the session stream (SIGKILLs ipmitool, ends
// an AMT session); Run then re-dials after
// ReconnectBackoff. A reconnect is logged (source=site) so the gap is
// visible, but no synthetic bytes go into the cast.
func (p *CastPump) runSilenceWatchdog(ctx context.Context, log *slog.Logger, lastByte *atomic.Int64, cancel context.CancelFunc) {
	t := time.NewTicker(p.cfg.SilenceCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		silentFor := time.Since(time.Unix(0, lastByte.Load()))
		if silentFor < p.cfg.SilenceThreshold {
			continue
		}
		state := p.cfg.StateFunc()
		if !shouldReconnectOnSilence(state) {
			continue
		}
		log.Warn("castpump: BMC silent past threshold, forcing reconnect",
			"silent_for", silentFor.Truncate(time.Second), "machine_state", state)
		cancel()
		return
	}
}

// shouldReconnectOnSilence gates the silence watchdog on machine
// state: only force a reconnect when the chassis is supposed to be
// powered on AND producing console output. Everything else (new,
// failed, "", anything unrecognized) suppresses — we'd rather miss a
// watchdog reconnect than hammer a BMC slot on a host that's meant to
// be quiet.
func shouldReconnectOnSilence(state string) bool {
	switch state {
	case string(model.MachineStateRunning),
		string(model.MachineStateDeploying),
		string(model.MachineStateCommissioning),
		string(model.MachineStateCommissioned):
		return true
	default:
		return false
	}
}

// isPtyClosedErr matches the EIO and "file already closed" cases
// reading from a pty master after its slave's child has exited.
func isPtyClosedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "input/output error") ||
		strings.Contains(msg, "file already closed")
}

// ---- production dialers ----

// TransportCastDialer picks each machine's console transport from the
// snapshot (castTransportFor): AMT redirection for an Intel AMT machine
// (snapshot AMT, i.e. its bmc-creds-admin entry is kind=amt), ipmitool SOL
// for any other machine with a BMC, and the local console-server tty for a
// BMC-less machine cabled to this worker. Selection keys on the snapshot
// rather than an op envelope because the pump runs continuously, not only
// while an op is in flight; the same AMT flag drives the power-path gates,
// so the two cannot disagree about a machine.
//
// A nil transport dialer fails the dial for machines that need it. The
// site worker runs two CastManagers over this type: the VIP-scoped one
// (IPMI + AMT, listing BMC machines) and a process-lifetime one (Serial
// only, listing SerialCastLister's machines), so the sets never overlap.
type TransportCastDialer struct {
	Cache *Cache
	// IPMI dials ipmitool SOL. nil when ipmitool is not on PATH: BMC
	// machines then fail each dial (and back off) while AMT machines still
	// stream.
	IPMI   CastDialer
	AMT    CastDialer
	Serial CastDialer
}

func (d *TransportCastDialer) Dial(ctx context.Context, machineID uuid.UUID) (CastSession, error) {
	m := d.Cache.MachineByID(machineID)
	if m == nil {
		return CastSession{}, fmt.Errorf("castpump dial: machine %s not in cache", machineID)
	}
	switch castTransportFor(m, d.Cache.SelfSerial()) {
	case castTransportAMT:
		if d.AMT == nil {
			return CastSession{}, fmt.Errorf("castpump dial: machine %s needs AMT redirection but this dialer has no AMT transport", machineID)
		}
		return d.AMT.Dial(ctx, machineID)
	case castTransportIPMI:
		if d.IPMI == nil {
			return CastSession{}, fmt.Errorf("castpump dial: machine %s needs ipmitool SOL but ipmitool is not on PATH", machineID)
		}
		return d.IPMI.Dial(ctx, machineID)
	case castTransportSerial:
		if d.Serial == nil {
			return CastSession{}, fmt.Errorf("castpump dial: machine %s needs its console-server tty but this dialer has no serial transport", machineID)
		}
		return d.Serial.Dial(ctx, machineID)
	default:
		return CastSession{}, fmt.Errorf("castpump dial: machine %s has no console transport on this worker (no BMC, and no console port cabled to this worker in NetBox)", machineID)
	}
}

// AMTOnlyCastLister narrows the cast pump set to AMT machines. A worker
// without ipmitool uses it so BMC machines get no pump (each would only
// fail its dial and back off, with a resume-offset RPC to global per
// attempt) while AMT machines, which need no ipmitool, still stream.
type AMTOnlyCastLister struct{ *Cache }

// ListActiveMachineIDs is Cache.ListActiveMachineIDs restricted to AMT
// machines.
func (l AMTOnlyCastLister) ListActiveMachineIDs() []uuid.UUID {
	all := l.Cache.ListActiveMachineIDs()
	out := all[:0]
	for _, id := range all {
		if m := l.MachineByID(id); m != nil && m.AMT {
			out = append(out, id)
		}
	}
	return out
}

// AMTSOLDialer opens an Intel AMT serial-over-LAN redirection session
// (TLS :16995) to the machine's oob_ip (snapshot BMCIP). The digest
// credential is the snapshot's BMCUser/BMCPass, which global stamps from
// the kind=amt bmc-creds-admin entry for an AMT machine.
type AMTSOLDialer struct {
	Cache  *Cache
	Logger *slog.Logger
	// HandshakeTimeout bounds TLS dial + redirection handshake. Default 15s.
	HandshakeTimeout time.Duration
	// dial is a test seam; nil uses redir.DialSOL.
	dial func(ctx context.Context, host, user, pass string) (io.ReadWriteCloser, error)
}

func (d *AMTSOLDialer) Dial(ctx context.Context, machineID uuid.UUID) (CastSession, error) {
	m := d.Cache.MachineByID(machineID)
	if m == nil {
		return CastSession{}, fmt.Errorf("castpump dial: machine %s not in cache", machineID)
	}
	if !m.AMT {
		return CastSession{}, fmt.Errorf("castpump dial: machine %s is not an AMT machine", machineID)
	}
	if m.BMCIP == "" {
		return CastSession{}, fmt.Errorf("castpump dial: AMT machine %s has no oob_ip", machineID)
	}
	if m.BMCUser == "" || m.BMCPass == "" {
		return CastSession{}, fmt.Errorf("castpump dial: AMT machine %s has no credential (populate its kind=amt bmc-creds-admin entry)", machineID)
	}
	timeout := d.HandshakeTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dial := d.dial
	if dial == nil {
		dial = func(ctx context.Context, host, user, pass string) (io.ReadWriteCloser, error) {
			return redir.DialSOL(ctx, host, user, pass, redir.Options{})
		}
	}
	stream, err := dial(hctx, m.BMCIP, m.BMCUser, m.BMCPass)
	if err != nil {
		return CastSession{}, fmt.Errorf("castpump dial: amt sol %s: %w", m.BMCIP, err)
	}
	logger := d.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Debug("castpump dialer: amt sol session started", "machine_id", machineID, "hostname", m.Hostname, "amt_ip", m.BMCIP)
	// AMT redirection carries only host serial bytes: no chrome to filter.
	return CastSession{Stream: stream}, nil
}

// IPMIToolPTYDialer dials a real BMC by spawning ipmitool sol activate
// under a pty.
//
// Pty wrapping (instead of plain stdout pipes) is required for ipmitool:
// it checks isatty(stdin), refuses to flow with "tcgetattr: Inappropriate
// ioctl for device" if stdin isn't a tty, and line-buffers output on a pipe
// (4KB block boundaries) vs. flushing immediately on a pty. Pty solves both.
type IPMIToolPTYDialer struct {
	Cache     *Cache
	Path      string // override for tests; "" → "ipmitool"
	CipherTag string // override; "" → "3" with op-side override
	Width     uint16
	Height    uint16
	Logger    *slog.Logger
}

func (d *IPMIToolPTYDialer) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

func (d *IPMIToolPTYDialer) Dial(ctx context.Context, machineID uuid.UUID) (CastSession, error) {
	log := d.logger().With("machine_id", machineID)

	m := d.Cache.MachineByID(machineID)
	if m == nil {
		return CastSession{}, fmt.Errorf("castpump dial: machine %s not in cache", machineID)
	}
	if m.AMT {
		// Plan D2: an AMT endpoint is never driven as IPMI.
		return CastSession{}, fmt.Errorf("castpump dial: machine %s's OOB endpoint is Intel AMT; refusing ipmitool SOL", machineID)
	}
	if m.BMCIP == "" {
		return CastSession{}, fmt.Errorf("castpump dial: machine %s has no BMC IP", machineID)
	}
	user, pass, credSrc := resolveSOLCredsWithSource(d.Cache, m)
	if user == "" || pass == "" {
		return CastSession{}, fmt.Errorf("castpump dial: no BMC creds for machine %s (cred_src=%s)", machineID, credSrc)
	}

	bin := d.Path
	if bin == "" {
		bin = "ipmitool"
	}
	cipher := d.CipherTag
	if cipher == "" {
		cipher = "3"
	}
	if op := d.Cache.ActiveDeployOpForMachine(m.ID); op != nil {
		if rt, err := extractBMCRuntime(op.Params); err == nil {
			if c := rt.IpmiCipher(); c > 0 {
				cipher = strconv.Itoa(c)
			}
		}
	}

	args := []string{"-I", "lanplus", "-U", user, "-E", "-C", cipher}
	if host, portStr, ok := splitHostPort(m.BMCIP); ok {
		args = append(args, "-H", host, "-p", portStr)
	} else {
		args = append(args, "-H", m.BMCIP)
	}
	args = append(args, "sol", "activate")

	procCtx, kill := context.WithCancel(ctx)
	cmd := exec.CommandContext(procCtx, bin, args...)
	cmd.Env = append(os.Environ(), "IPMI_PASSWORD="+pass)

	width := d.Width
	if width == 0 {
		width = 80
	}
	height := d.Height
	if height == 0 {
		height = 24
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: width, Rows: height})
	if err != nil {
		kill()
		return CastSession{}, fmt.Errorf("pty.StartWithSize: %w", err)
	}
	log.Debug("castpump dialer: started ipmitool",
		"hostname", m.Hostname,
		"bmc_ip", m.BMCIP,
		"cred_src", credSrc,
		"cipher", cipher,
		"pid", cmd.Process.Pid,
	)
	return CastSession{
		Stream: newPTYStream(cmd, ptmx, kill),
		Noise:  &solNoiseFilter{},
	}, nil
}

// ptyStream adapts a started child process and its pty master to the
// CastSession stream contract: Read/Write go to the pty, Close SIGKILLs
// the child (via its CommandContext), reaps it, and closes the pty.
// Killing before Wait matters: a child still running on a hung UDP IPMI
// session would otherwise block Wait forever.
type ptyStream struct {
	cmd  *exec.Cmd
	ptmx *os.File
	kill context.CancelFunc
	once sync.Once
}

func newPTYStream(cmd *exec.Cmd, ptmx *os.File, kill context.CancelFunc) *ptyStream {
	return &ptyStream{cmd: cmd, ptmx: ptmx, kill: kill}
}

func (s *ptyStream) Read(p []byte) (int, error)  { return s.ptmx.Read(p) }
func (s *ptyStream) Write(p []byte) (int, error) { return s.ptmx.Write(p) }

func (s *ptyStream) Close() error {
	s.once.Do(func() {
		s.kill()
		_ = s.ptmx.Close()
		_ = s.cmd.Wait()
	})
	return nil
}

// splitHostPort returns (host, port, true) if s is "host:port" where
// port parses as a valid number. Returns ("", "", false) otherwise —
// including for plain IPv4/hostnames and bracketed IPv6 forms (which
// ipmitool can take as -H directly). Used to transparently support
// test BMC URLs of the form "127.0.0.1:<n>" without disturbing the
// production "10.254.241.9" case. (Moved here from the deleted
// solmanager.go — shared by the cast dialer.)
func splitHostPort(s string) (string, string, bool) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", "", false
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", false
	}
	return host, port, true
}

// resolveSOLCredsWithSource picks the BMC creds in priority order:
//
//  1. snapshot BMCUser/BMCPass — the device's bmc-creds-admin entry,
//     stamped by global at snapshot build.
//  2. active deploy op's bmc_runtime envelope — the same entry, resolved
//     by global at op creation (covers a snapshot built while the
//     secret fetch failed).
//
// Half-creds (one set, one empty) refuses both tiers; the caller
// surfaces "no credentials" and the pump backs off.
//
// Returns (user, pass, source) where source is a string label
// describing which path produced the cred — used by the dialer for
// diagnostic logging. (Moved here from the deleted solmanager.go —
// shared by the cast dialer.)
func resolveSOLCredsWithSource(cache *Cache, m *model.MachineSnapshot) (string, string, string) {
	if m.BMCUser != "" && m.BMCPass != "" {
		return m.BMCUser, m.BMCPass, "snapshot"
	}
	if m.BMCUser != "" || m.BMCPass != "" {
		return "", "", "snapshot_partial"
	}
	op := cache.ActiveDeployOpForMachine(m.ID)
	if op == nil {
		return "", "", "no_active_deploy_op"
	}
	rt, err := extractBMCRuntime(op.Params)
	if err != nil {
		return "", "", "op_bmc_runtime_decode_err"
	}
	if rt.Username == "" || rt.Password == "" {
		return "", "", "op_bmc_runtime_empty"
	}
	return rt.Username, rt.Password, "op_bmc_runtime"
}
