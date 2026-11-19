package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
)

func TestScrubUTF8(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"ascii", []byte("hello\r\n"), "hello\r\n"},
		{"utf8", []byte("héllo"), "héllo"},
		{"invalid_byte_replaced", []byte{'a', 0xff, 'b'}, "a�b"},
		{"empty", []byte{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scrubUTF8(c.in)
			if got != c.want {
				t.Errorf("scrubUTF8(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestEncodeCastEventLine_Shape(t *testing.T) {
	row := model.ConsoleCastEvent{TUs: 1_500_000, Data: []byte("hi\r\n")}
	line, err := encodeCastEventLine(row, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var parsed [3]any
	if err := json.Unmarshal(line, &parsed); err != nil {
		t.Fatalf("not a JSON array: %v / %q", err, line)
	}
	if t0, ok := parsed[0].(float64); !ok || t0 != 1.5 {
		t.Errorf("t = %v, want 1.5", parsed[0])
	}
	if parsed[1] != "o" {
		t.Errorf("kind = %v, want o", parsed[1])
	}
	if parsed[2] != "hi\r\n" {
		t.Errorf("data = %q, want hi\\r\\n", parsed[2])
	}
}

func TestEncodeCastEventLine_NonUTF8Scrubbed(t *testing.T) {
	row := model.ConsoleCastEvent{TUs: 0, Data: []byte{'a', 0xff, 'b'}}
	line, err := encodeCastEventLine(row, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var parsed [3]any
	if err := json.Unmarshal(line, &parsed); err != nil {
		t.Fatal(err)
	}
	got, ok := parsed[2].(string)
	if !ok {
		t.Fatalf("data not a string: %T", parsed[2])
	}
	if got != "a�b" {
		t.Errorf("got %q, want a\\uFFFDb", got)
	}
}

// fakeConsoleStore is a minimal in-memory implementation of
// ConsoleStreamStore for handler-level tests.
type fakeConsoleStore struct {
	cast         *model.ConsoleCast
	events       []model.ConsoleCastEvent
	sinceFloor   int64
	liveEdge     int64
	ensureCalled bool
	// unknownMachine makes MachineExists report false.
	unknownMachine bool
}

func (s *fakeConsoleStore) MachineExists(_ uuid.UUID) (bool, error) {
	return !s.unknownMachine, nil
}

func (s *fakeConsoleStore) EnsureConsoleCast(_ uuid.UUID) (*model.ConsoleCast, error) {
	s.ensureCalled = true
	return s.cast, nil
}

func (s *fakeConsoleStore) CastEventsSince(_ context.Context, _ int64, floorTUs int64, _ int) ([]model.ConsoleCastEvent, error) {
	out := []model.ConsoleCastEvent{}
	for _, e := range s.events {
		if e.TUs >= floorTUs {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *fakeConsoleStore) SinceFloorByDuration(_ context.Context, _ int64, _ time.Duration) (int64, error) {
	return s.sinceFloor, nil
}

func (s *fakeConsoleStore) LiveEdgeCastFloor(_ context.Context, _ int64) (int64, error) {
	return s.liveEdge, nil
}

func (s *fakeConsoleStore) ScanCastEventsForNonce(_ context.Context, _ uuid.UUID, _ string, _ time.Duration) (bool, error) {
	return false, nil
}

// TestHandleConsoleStream_BackfillThenClose: with follow=false the
// server streams the asciicast header + all events and closes.
func TestHandleConsoleStream_BackfillThenClose(t *testing.T) {
	mid := uuid.New()
	now := time.Now().UTC()
	cs := &fakeConsoleStore{
		cast: &model.ConsoleCast{
			ID: 42, MachineID: mid, CastStartedAt: now,
			Width: 80, Height: 24,
		},
		events: []model.ConsoleCastEvent{
			{TUs: 1_000_000, Data: []byte("first\n")},
			{TUs: 2_500_000, Data: []byte("second\n")},
		},
	}

	srv := newServerWithConsole(cs)
	httpSrv := httptest.NewServer(srv.echo)
	defer httpSrv.Close()

	wsURL := strings.Replace(httpSrv.URL, "http", "ws", 1) + "/api/v1/console/stream?machine=" + mid.String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, dialResp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	defer conn.CloseNow() //nolint:errcheck

	// Read header.
	_, hdr, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	if err := json.Unmarshal(hdr, &header); err != nil {
		t.Fatalf("header decode: %v / %q", err, hdr)
	}
	if v, _ := header["version"].(float64); v != 2 {
		t.Errorf("version = %v, want 2", header["version"])
	}
	if v, _ := header["idle_time_limit"].(float64); v != 2.0 {
		t.Errorf("idle_time_limit = %v, want 2.0", header["idle_time_limit"])
	}
	if v, _ := header["width"].(float64); v != 80 {
		t.Errorf("width = %v, want 80", header["width"])
	}

	// Read events until the server closes (follow=false default).
	var lines [][]byte
	for {
		_, line, err := conn.Read(ctx)
		if err != nil {
			break
		}
		lines = append(lines, line)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d event lines, want 2: %s", len(lines), lines)
	}
	var first, second [3]any
	_ = json.Unmarshal(lines[0], &first)
	_ = json.Unmarshal(lines[1], &second)
	if first[2] != "first\n" || second[2] != "second\n" {
		t.Errorf("data sequence = %v, %v; want first\\n, second\\n", first[2], second[2])
	}
	if !cs.ensureCalled {
		t.Error("EnsureConsoleCast not called")
	}
}

// TestHandleConsoleStream_UnknownMachine404: a machine id that names no
// machine is refused with 404 before the upgrade, and the cast row is
// never ensured (no write for an unknown id).
func TestHandleConsoleStream_UnknownMachine404(t *testing.T) {
	cs := &fakeConsoleStore{unknownMachine: true}
	srv := newServerWithConsole(cs)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/console/stream?machine="+uuid.New().String(), nil)
	w := httptest.NewRecorder()
	srv.echo.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "slagctl machine list") {
		t.Errorf("404 body should tell the operator where to find a machine UUID: %s", w.Body.String())
	}
	if cs.ensureCalled {
		t.Error("EnsureConsoleCast called for an unknown machine")
	}
}

// TestHandleConsoleStream_SnapshotMode: with snapshot=true the server replays
// the whole cast into its own emulator and emits exactly ONE event line — the
// reconstructed screen — instead of one line per cast event. Feeding
// that single frame into a fresh emulator must reproduce the live screen,
// including a cursor-addressed in-place overwrite that a partial replay would
// corrupt.
func TestHandleConsoleStream_SnapshotMode(t *testing.T) {
	mid := uuid.New()
	now := time.Now().UTC()
	cs := &fakeConsoleStore{
		cast: &model.ConsoleCast{
			ID: 7, MachineID: mid, CastStartedAt: now, Width: 80, Height: 24,
		},
		// Many events, with an in-place overwrite of the top line: the live
		// screen shows "iPXE", not "BIOS".
		events: []model.ConsoleCastEvent{
			{TUs: 1, Data: []byte("\x1b[2J\x1b[H")},
			{TUs: 2, Data: []byte("\x1b[1;1HBIOS POST")},
			{TUs: 3, Data: []byte("\x1b[3;1Hloading...")},
			{TUs: 4, Data: []byte("\x1b[1;1HiPXE 1.21.1\x1b[K")},
		},
	}

	srv := newServerWithConsole(cs)
	httpSrv := httptest.NewServer(srv.echo)
	defer httpSrv.Close()

	wsURL := strings.Replace(httpSrv.URL, "http", "ws", 1) +
		"/api/v1/console/stream?machine=" + mid.String() + "&snapshot=true"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, dialResp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	defer conn.CloseNow() //nolint:errcheck

	// Header.
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("read header: %v", err)
	}
	// Exactly one event line (the snapshot), then close (follow=false).
	var lines [][]byte
	for {
		_, line, err := conn.Read(ctx)
		if err != nil {
			break
		}
		lines = append(lines, line)
	}
	if len(lines) != 1 {
		t.Fatalf("snapshot mode should emit exactly 1 frame, got %d: %s", len(lines), lines)
	}
	var ev [3]any
	if err := json.Unmarshal(lines[0], &ev); err != nil {
		t.Fatalf("snapshot frame not a JSON event: %v / %q", err, lines[0])
	}
	data, _ := ev[2].(string)
	// Reproduce the screen from the snapshot frame and check the overwrite won.
	got := feedANSI([]byte(data))
	if !strings.Contains(got, "iPXE 1.21.1") {
		t.Errorf("snapshot screen should show the overwriting iPXE line:\n%q", got)
	}
	if strings.Contains(got, "BIOS POST") {
		t.Errorf("overwritten BIOS text must not survive in the snapshot:\n%q", got)
	}
	if !strings.Contains(got, "loading...") {
		t.Errorf("snapshot should retain the un-overwritten line:\n%q", got)
	}
}

// generatedCastStore serves a long cast one page at a time and allocates
// each row's bytes fresh on every call, as the database driver does, so the
// store itself retains nothing between pages. Each call first forces a GC
// and records how far the live heap has grown above baseline: whatever is
// still live at that point is what the handler kept from earlier pages.
type generatedCastStore struct {
	rows    int64
	rowSize int

	mu       sync.Mutex
	baseline uint64
	peak     uint64
}

func liveHeapBytes() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// generatedCastRow is row t of the cast, rowSize bytes: NUL filler, which the
// emulator consumes without drawing (keeping a long replay fast), then a
// newline and one full 80-column line naming the row. Every row scrolls the
// screen by one full line, which is what fills an emulator's scrollback, and
// the final screen's bottom line names the last row replayed.
func generatedCastRow(t int64, rowSize int) []byte {
	line := fmt.Sprintf("\r\nrow %08d %s", t, strings.Repeat("k", 67))
	data := make([]byte, rowSize)
	copy(data[rowSize-len(line):], line)
	return data
}

func (s *generatedCastStore) MachineExists(uuid.UUID) (bool, error) { return true, nil }

func (s *generatedCastStore) EnsureConsoleCast(uuid.UUID) (*model.ConsoleCast, error) {
	return &model.ConsoleCast{ID: 1, Width: 80, Height: 24, CastStartedAt: time.Now()}, nil
}

func (s *generatedCastStore) CastEventsSince(_ context.Context, _ int64, floorTUs int64, limit int) ([]model.ConsoleCastEvent, error) {
	grown := liveHeapBytes()
	s.mu.Lock()
	if grown > s.baseline {
		s.peak = max(s.peak, grown-s.baseline)
	}
	s.mu.Unlock()
	var out []model.ConsoleCastEvent
	for t := max(floorTUs, 0); t < s.rows && (limit <= 0 || len(out) < limit); t++ {
		out = append(out, model.ConsoleCastEvent{ID: t + 1, TUs: t, Data: generatedCastRow(t, s.rowSize)})
	}
	return out, nil
}

func (s *generatedCastStore) SinceFloorByDuration(context.Context, int64, time.Duration) (int64, error) {
	return 0, nil
}

func (s *generatedCastStore) LiveEdgeCastFloor(context.Context, int64) (int64, error) {
	return s.rows, nil
}

func (s *generatedCastStore) ScanCastEventsForNonce(context.Context, uuid.UUID, string, time.Duration) (bool, error) {
	return false, nil
}

// TestHandleConsoleStream_SnapshotMemoryBoundedByPage: snapshot=true is the
// watch TUI's attach path, and cast events are kept forever, so the cast a
// snapshot replays grows for the life of the machine. The snapshot needs
// only the final screen, so global's memory while building it must stay
// bounded by one page of rows plus the emulator's screen. Two things broke
// that: collecting every row's bytes before replaying any of them, and the
// emulator's 10,000-line scrollback, which the snapshot never reads. A
// 64 MiB cast here must not grow the live heap past 16 MiB at any page
// boundary, and the snapshot must still end on the cast's last row.
func TestHandleConsoleStream_SnapshotMemoryBoundedByPage(t *testing.T) {
	const rows, rowSize = 16384, 4096 // 64 MiB of SOL bytes
	cs := &generatedCastStore{rows: rows, rowSize: rowSize}
	srv := newServerWithConsole(cs)
	httpSrv := httptest.NewServer(srv.echo)
	defer httpSrv.Close()

	wsURL := strings.Replace(httpSrv.URL, "http", "ws", 1) +
		"/api/v1/console/stream?machine=" + uuid.NewString() + "&snapshot=true"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cs.mu.Lock()
	cs.baseline = liveHeapBytes()
	cs.mu.Unlock()
	conn, dialResp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	defer conn.CloseNow() //nolint:errcheck

	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("read header: %v", err)
	}
	var lines [][]byte
	for {
		_, line, err := conn.Read(ctx)
		if err != nil {
			break // server closes after the snapshot (follow=false)
		}
		lines = append(lines, line)
	}
	if len(lines) != 1 {
		t.Fatalf("snapshot mode should emit exactly 1 frame, got %d", len(lines))
	}
	var ev [3]any
	if err := json.Unmarshal(lines[0], &ev); err != nil {
		t.Fatalf("snapshot frame not a JSON event: %v / %q", err, lines[0])
	}
	data, _ := ev[2].(string)
	screen := feedANSI([]byte(data))
	if want := fmt.Sprintf("row %08d", rows-1); !strings.Contains(screen, want) {
		t.Errorf("snapshot should end on the cast's last row %q:\n%q", want, screen)
	}

	cs.mu.Lock()
	peak := cs.peak
	cs.mu.Unlock()
	t.Logf("peak live-heap growth %d KiB", peak>>10)
	const limit = 16 << 20
	if peak > limit {
		t.Fatalf("live heap grew by %d MiB while building one snapshot of a %d MiB cast; want <= %d MiB (one page of rows plus one screen)",
			peak>>20, rows*rowSize>>20, limit>>20)
	}
}

// TestResolveStartFloor_TailValidation pins the server-side contract the
// client's --tail normalization relies on: a unit-bearing tail
// (e.g. "80s") parses and resolves to the store's since-floor, while a
// unitless tail ("80") is rejected as a 400-worthy error. The client now
// coerces bare numbers to seconds before they reach here, so a well-formed
// request never trips the unitless path; this test documents that the
// server is the validation backstop, not the place the coercion happens.
func TestResolveStartFloor_TailValidation(t *testing.T) {
	cs := &fakeConsoleStore{sinceFloor: 4242, liveEdge: 9999}
	srv := &Server{consoleStore: cs}
	e := echo.New()

	newCtx := func(rawQuery string) *echo.Context {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/console/stream?"+rawQuery, nil)
		return e.NewContext(req, httptest.NewRecorder())
	}

	cases := []struct {
		name      string
		query     string
		follow    bool
		wantFloor int64
		wantErr   bool
	}{
		{name: "tail_with_unit_ok", query: "tail=80s", wantFloor: 4242},
		{name: "tail_minutes_ok", query: "tail=10m", wantFloor: 4242},
		{name: "tail_unitless_rejected", query: "tail=80", wantErr: true},
		{name: "since_us_zero_ok", query: "since_us=0", wantFloor: 0},
		{name: "since_us_garbage_rejected", query: "since_us=abc", wantErr: true},
		{name: "none_nonfollow_floor_zero", query: "", wantFloor: 0},
		{name: "none_follow_live_edge", query: "", follow: true, wantFloor: 9999},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			floor, err := srv.resolveStartFloor(context.Background(), 1, newCtx(c.query), c.follow)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got floor=%d", floor)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if floor != c.wantFloor {
				t.Errorf("floor = %d, want %d", floor, c.wantFloor)
			}
		})
	}
}

// newServerWithConsole builds a Server with only the console stream
// handler mounted, without the oidcAuth middleware: these are
// handler-level tests. stream_auth_test.go drives the production router.
func newServerWithConsole(cs ConsoleStreamStore) *Server {
	s := &Server{
		echo:         echo.New(),
		consoleStore: cs,
	}
	api := s.echo.Group("/api/v1")
	api.GET("/console/stream", s.handleConsoleStream)
	return s
}

// noopBody is a stand-in for tests that don't care about response bodies.
var _ = http.StatusOK
