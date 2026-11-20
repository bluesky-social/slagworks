// Package skewtest runs upgrade, rollback and restart scenarios across two
// builds of slagworks: the tree under test and an older release.
//
// The driver is internal/skewtest/skewtool, a small program that runs either
// a global or a site worker and uses only the internal API both releases
// share. TestMain builds it twice (once in this tree, once inside a checkout
// of the old release) and the properties run the two builds against the same
// Postgres database and the same worker.db, the way an operator's upgrade
// does.
//
// The cross-release tests are opt-in, because they need a second source
// tree and several minutes. The old release comes from, in order:
//
//	SKEW_OLD_TREE  a directory holding a checkout of the old release
//	SKEW_OLD_REF   a git ref to extract with git archive (for example v0.5.0)
//
// With neither set the cross-release tests skip and the same-release ones
// still run. The driver has to compile inside the old tree, so the oldest
// usable release is the oldest one whose internal API it still fits.
//
// Other knobs: SKEW_SHARDS=<n> runs each generated property as n parallel
// rapid checks; SKEW_TRACE=1 logs every case's history; SKEW_STDERR_DIR=<dir>
// keeps every driver process's log; SKEW_PORT_BASE=<n> moves the loopback
// port range. The generated properties run a fraction of -rapid.checks (a
// case starts four or more processes), so raise that flag for a deep run.
package skewtest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/bluesky-social/slagworks/internal/testpg"
)

var (
	toolNew    string // skewtool built from this tree
	toolOld    string // skewtool built from the old release; "" when unavailable
	oldMissing string // why toolOld is ""
	oldRef     string
)

func TestMain(m *testing.M) {
	flag.Parse()
	code := func() int {
		dir, err := os.MkdirTemp("", "skewtest-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "skewtest: temp dir:", err)
			return 1
		}
		defer func() { _ = os.RemoveAll(dir) }()
		root, err := moduleRoot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "skewtest:", err)
			return 1
		}
		toolNew = filepath.Join(dir, "skewtool-new")
		if out, err := run(root, "go", "build", "-o", toolNew, "./internal/skewtest/skewtool"); err != nil {
			fmt.Fprintf(os.Stderr, "skewtest: building skewtool from this tree failed: %v\n%s\n", err, out)
			return 1
		}
		oldRef = os.Getenv("SKEW_OLD_REF")
		if oldRef == "" {
			oldRef = "old"
		}
		if os.Getenv("SKEW_OLD_TREE") == "" && os.Getenv("SKEW_OLD_REF") == "" {
			oldMissing = "set SKEW_OLD_REF to a release tag (or SKEW_OLD_TREE to a checkout of one) to run it"
		} else {
			oldTree, err := resolveOldTree(root, dir, oldRef)
			if err != nil {
				// Asked for and not available: fail, do not skip.
				fmt.Fprintln(os.Stderr, "skewtest:", err)
				return 1
			}
			dst := filepath.Join(oldTree, "internal", "skewtest", "skewtool")
			if err := os.MkdirAll(dst, 0o755); err != nil {
				fmt.Fprintln(os.Stderr, "skewtest:", err)
				return 1
			}
			src, err := os.ReadFile(filepath.Join(root, "internal", "skewtest", "skewtool", "main.go"))
			if err != nil {
				fmt.Fprintln(os.Stderr, "skewtest:", err)
				return 1
			}
			// With SKEW_OLD_TREE the tree is the caller's: leave it as it
			// was found.
			driver := filepath.Join(dst, "main.go")
			previous, readErr := os.ReadFile(driver)
			switch {
			case readErr == nil:
				defer func() { _ = os.WriteFile(driver, previous, 0o644) }()
			case errors.Is(readErr, os.ErrNotExist):
				defer func() { _ = os.Remove(driver) }()
			default:
				fmt.Fprintln(os.Stderr, "skewtest:", readErr)
				return 1
			}
			if err := os.WriteFile(driver, src, 0o644); err != nil {
				fmt.Fprintln(os.Stderr, "skewtest:", err)
				return 1
			}
			toolOld = filepath.Join(dir, "skewtool-old")
			if out, err := run(oldTree, "go", "build", "-mod=vendor", "-o", toolOld, "./internal/skewtest/skewtool"); err != nil {
				// A release the shared API subset no longer compiles against
				// is a harness problem to fix, not a reason to pass.
				fmt.Fprintf(os.Stderr, "skewtest: building skewtool inside the %s tree failed: %v\n%s\n", oldRef, err, out)
				return 1
			}
		}
		c := m.Run()
		testpg.Shutdown()
		return c
	}()
	os.Exit(code)
}

func run(dir string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

func resolveOldTree(root, tmp, ref string) (string, error) {
	if d := os.Getenv("SKEW_OLD_TREE"); d != "" {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err != nil {
			return "", fmt.Errorf("SKEW_OLD_TREE=%s holds no go.mod: %w", d, err)
		}
		return d, nil
	}
	dst := filepath.Join(tmp, "old")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", err
	}
	tarball := filepath.Join(tmp, "old.tar")
	if out, err := run(root, "git", "archive", "--format=tar", "-o", tarball, ref); err != nil {
		return "", fmt.Errorf("no old release to test against: git archive %s failed (%v: %s); fetch the tag or set SKEW_OLD_TREE to a checkout of it", ref, err, strings.TrimSpace(out))
	}
	if out, err := run(dst, "tar", "-xf", tarball); err != nil {
		return "", fmt.Errorf("extracting %s: %v: %s", ref, err, out)
	}
	_ = os.Remove(tarball)
	return dst, nil
}

func needOld(t *testing.T) {
	t.Helper()
	if toolOld == "" {
		t.Skipf("cross-release test skipped: %s", oldMissing)
	}
}

// fataler is the part of testing.T and rapid.T the harness needs.
type fataler interface {
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// ---------------------------------------------------------------- tool process

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *lockedBuffer) tail(n int) string {
	s := l.String()
	if len(s) > n {
		return "..." + s[len(s)-n:]
	}
	return s
}

// proc is one running skewtool.
type proc struct {
	name   string
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *lockedBuffer
	exited chan struct{}
}

func startProc(t fataler, bin, name string) *proc {
	cmd := exec.Command(bin)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("harness: %s stdin: %v", name, err)
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("harness: %s stdout: %v", name, err)
	}
	p := &proc{name: name, cmd: cmd, in: in, out: bufio.NewReaderSize(outPipe, 1<<20), stderr: &lockedBuffer{}, exited: make(chan struct{})}
	cmd.Stderr = p.stderr
	// SKEW_STDERR_DIR keeps every tool's full log, for reading a scenario
	// back afterwards.
	if d := os.Getenv("SKEW_STDERR_DIR"); d != "" {
		if f, err := os.Create(filepath.Join(d, fmt.Sprintf("%04d-%s.log", procSerial.Add(1), name))); err == nil {
			cmd.Stderr = io.MultiWriter(p.stderr, f)
			go func() {
				<-p.exited
				_ = f.Close()
			}()
		}
	}
	// GOTRACEBACK=all: a SIGQUIT sent to a hung tool prints every goroutine.
	cmd.Env = append(os.Environ(), "GOTRACEBACK=all")
	if err := cmd.Start(); err != nil {
		t.Fatalf("harness: start %s: %v", name, err)
	}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p
}

var procSerial atomic.Int64

type toolResponse struct {
	OK   bool            `json:"ok"`
	Err  string          `json:"err"`
	Data json.RawMessage `json:"data"`
}

// errToolDied marks a call whose process exited or hung instead of
// answering: a crash or deadlock in the release under test, never an
// ordinary refusal.
var errToolDied = errors.New("tool process died")

// call sends one command and decodes the reply's data into out. An error
// the tool reported comes back as a plain error; a tool that exits or stops
// answering comes back wrapping errToolDied with its stderr.
func (p *proc) call(cmd string, args any, out any) error {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("harness: marshal %s args: %w", cmd, err)
	}
	line, err := json.Marshal(map[string]any{"cmd": cmd, "args": json.RawMessage(raw)})
	if err != nil {
		return err
	}
	if _, err := p.in.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("%w: %s: write %s: %v\nstderr:\n%s", errToolDied, p.name, cmd, err, p.stderr.tail(6000))
	}
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		l, err := p.out.ReadBytes('\n')
		ch <- result{l, err}
	}()
	var res result
	select {
	case res = <-ch:
	case <-time.After(90 * time.Second):
		_ = p.cmd.Process.Signal(syscall.SIGQUIT)
		<-p.exited
		return fmt.Errorf("%w: %s did not answer %s within 90s (deadlock?)\nstderr:\n%s", errToolDied, p.name, cmd, p.stderr.tail(30000))
	}
	if res.err != nil {
		<-p.exited
		return fmt.Errorf("%w: %s exited during %s: %v\nstderr:\n%s", errToolDied, p.name, cmd, res.err, p.stderr.tail(12000))
	}
	var resp toolResponse
	if err := json.Unmarshal(res.line, &resp); err != nil {
		return fmt.Errorf("harness: %s: bad reply to %s: %v: %q", p.name, cmd, err, res.line)
	}
	if out != nil && len(resp.Data) > 0 && string(resp.Data) != "null" {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return fmt.Errorf("harness: %s: decode %s data: %v: %s", p.name, cmd, err, resp.Data)
		}
	}
	if !resp.OK {
		return errors.New(resp.Err)
	}
	return nil
}

// kill ends the process the way a crash or an OOM kill does.
func (p *proc) kill() {
	_ = p.cmd.Process.Kill()
	<-p.exited
}

// quit closes stdin, which ends the tool's command loop.
func (p *proc) quit() {
	_ = p.in.Close()
	select {
	case <-p.exited:
	case <-time.After(10 * time.Second):
		p.kill()
	}
}

// ---------------------------------------------------------------- postgres

var (
	pgOnce   sync.Once
	pgAdmin  *gorm.DB
	pgDSN    string
	pgErr    error
	pgSerial int
	pgMu     sync.Mutex
)

// freshDatabase creates an empty database on the shared container and
// returns its DSN. template, when non-empty, names a database to copy.
func freshDatabase(t *testing.T, f fataler, template string) (name, dsn string) {
	pgMu.Lock()
	pgOnce.Do(func() {
		_ = testpg.DB(t, func(*gorm.DB) error { return nil })
		pgDSN = testpg.DSN()
		pgAdmin, pgErr = gorm.Open(postgres.Open(pgDSN), &gorm.Config{Logger: logger.Discard})
	})
	if pgErr != nil || pgAdmin == nil {
		pgMu.Unlock()
		f.Fatalf("harness: no Postgres to run against (the test database helper needs Docker): %v", pgErr)
	}
	pgSerial++
	name = fmt.Sprintf("skew_%d_%d", os.Getpid(), pgSerial)
	pgMu.Unlock()
	stmt := "CREATE DATABASE " + name
	if template != "" {
		stmt += " TEMPLATE " + template
	}
	if err := pgAdmin.Exec(stmt).Error; err != nil {
		f.Fatalf("harness: %s: %v", stmt, err)
	}
	return name, strings.Replace(pgDSN, "dbname=slagworks_test", "dbname="+name, 1)
}

func dropDatabase(name string) {
	if pgAdmin != nil {
		_ = pgAdmin.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)").Error
	}
}

// ---------------------------------------------------------------- small helpers

// ports hands out loopback addresses from a range below the kernel's
// ephemeral ports, so an address a case holds across a global restart
// cannot be taken in between by another process's connect or listen(:0).
// SKEW_PORT_BASE moves the range, for several test processes on one host
// (each takes 1000 ports from its base).
var ports = newPortAlloc()

type portAlloc struct {
	mu         sync.Mutex
	base, next int
	held       map[int]bool
}

const portRange = 1000

func newPortAlloc() *portAlloc {
	base := 20000 + (os.Getpid()%10)*portRange
	if v := os.Getenv("SKEW_PORT_BASE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 1024 && n < 65000 {
			base = n
		}
	}
	return &portAlloc{base: base, held: map[int]bool{}}
}

// get returns a free address. With hold, no later get returns the same
// port until release.
func (a *portAlloc) get(f fataler, hold bool) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for tries := 0; tries < portRange; tries++ {
		port := a.base + a.next
		a.next = (a.next + 1) % portRange
		if a.held[port] {
			continue
		}
		addr := "127.0.0.1:" + strconv.Itoa(port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		_ = ln.Close()
		if hold {
			a.held[port] = true
		}
		return addr
	}
	f.Fatalf("harness: no free port in %d-%d", a.base, a.base+portRange-1)
	return ""
}

func (a *portAlloc) release(addr string) {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	n, _ := strconv.Atoi(p)
	a.mu.Lock()
	delete(a.held, n)
	a.mu.Unlock()
}

func freePort(f fataler) string { return ports.get(f, false) }

var httpClient = &http.Client{Timeout: 10 * time.Second}

// post sends a JSON body and returns the status code and body text; status 0
// means nothing answered (the worker is down).
func post(url string, body any) (int, string) {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func get(url string) (int, string) {
	resp, err := httpClient.Get(url) //nolint:noctx // test helper
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}

// outboxRows counts the rows of a worker.db's outbox table: the
// observations the worker has queued and global has not acknowledged. It
// reads the file directly, so the count is the durable one whatever build
// of the worker wrote it and whether or not a worker is running. -1 means
// the file has no outbox table yet.
func outboxRows(f fataler, path string) int {
	db, err := gorm.Open(sqlite.Open(path+"?_pragma=busy_timeout(5000)"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		f.Fatalf("harness: open %s: %v", path, err)
	}
	defer func() {
		if s, err := db.DB(); err == nil {
			_ = s.Close()
		}
	}()
	var n int
	if err := db.Raw(`SELECT count(*) FROM outbox`).Scan(&n).Error; err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return -1
		}
		f.Fatalf("harness: count outbox rows of %s: %v", path, err)
	}
	return n
}

// eventually polls cond until it holds or d passes.
func eventually(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(15 * time.Millisecond)
	}
}
