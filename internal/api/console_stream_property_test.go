package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Property tests for /api/v1/console/stream against an in-memory store
// that honours the production query contract (t_us >= floor, ORDER BY
// t_us, LIMIT), unlike fakeConsoleStore which ignores limit and so never
// exercises pagination. The model is "every stored event, in t_us
// order, exactly once": the client must receive precisely that, across
// page boundaries (pageSize=500), in follow mode while new events land,
// and the handler goroutine must exit once the client goes away.

type pagedConsoleStore struct {
	cast *model.ConsoleCast

	mu     sync.Mutex
	events []model.ConsoleCastEvent // id-ordered (insertion order)
	nextID int64
}

func (s *pagedConsoleStore) add(tus int64) model.ConsoleCastEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	e := model.ConsoleCastEvent{ID: s.nextID, CastID: s.cast.ID, TUs: tus, Data: []byte(fmt.Sprintf("e%d;", s.nextID))}
	s.events = append(s.events, e)
	return e
}

func (s *pagedConsoleStore) MachineExists(uuid.UUID) (bool, error) { return true, nil }
func (s *pagedConsoleStore) EnsureConsoleCast(uuid.UUID) (*model.ConsoleCast, error) {
	return s.cast, nil
}

func (s *pagedConsoleStore) CastEventsSince(_ context.Context, _ int64, floor int64, limit int) ([]model.ConsoleCastEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.ConsoleCastEvent
	for _, e := range s.events {
		if e.TUs >= floor {
			out = append(out, e)
		}
	}
	// ORDER BY t_us ASC; ties broken by id (one legal pg order).
	sort.SliceStable(out, func(i, j int) bool { return out[i].TUs < out[j].TUs })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *pagedConsoleStore) SinceFloorByDuration(context.Context, int64, time.Duration) (int64, error) {
	return 0, nil
}

func (s *pagedConsoleStore) LiveEdgeCastFloor(context.Context, int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := int64(-1)
	for _, e := range s.events {
		m = max(m, e.TUs)
	}
	return m + 1, nil
}

func (s *pagedConsoleStore) ScanCastEventsForNonce(context.Context, uuid.UUID, string, time.Duration) (bool, error) {
	return false, nil
}

// consoleStreamServer wires handleConsoleStream and reports when each
// handler invocation returns, so tests can assert resource release. The
// handler polls the store synchronously, so its return also means the
// store is no longer being polled.
func consoleStreamServer(cs ConsoleStreamStore) (*httptest.Server, *sync.WaitGroup) {
	s := &Server{echo: echo.New(), consoleStore: cs}
	var inflight sync.WaitGroup
	s.echo.GET("/api/v1/console/stream", func(c *echo.Context) error {
		inflight.Add(1)
		defer inflight.Done()
		return s.handleConsoleStream(c)
	})
	return httptest.NewServer(s.echo), &inflight
}

func dialConsole(t interface{ Fatalf(string, ...any) }, srv *httptest.Server, q string) *websocket.Conn {
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/console/stream?machine=" + uuid.NewString() + q
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, dialResp, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	conn.SetReadLimit(1 << 24)
	return conn
}

// readEventIDs reads asciicast event lines and returns the event ids
// (decoded from the data payload "e<id>;") in arrival order, stopping
// after want lines or on close/timeout.
func readEventIDs(conn *websocket.Conn, want int, timeout time.Duration) ([]int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var ids []int64
	for len(ids) < want {
		_, buf, err := conn.Read(ctx)
		if err != nil {
			return ids, err
		}
		var arr []any
		if err := json.Unmarshal(buf, &arr); err != nil {
			continue // header object
		}
		data, _ := arr[2].(string)
		for _, part := range strings.Split(strings.TrimSuffix(data, ";"), ";") {
			var id int64
			if _, err := fmt.Sscanf(part, "e%d", &id); err == nil {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func expectedOrder(events []model.ConsoleCastEvent, floor int64) []int64 {
	cp := append([]model.ConsoleCastEvent(nil), events...)
	sort.SliceStable(cp, func(i, j int) bool { return cp[i].TUs < cp[j].TUs })
	var ids []int64
	for _, e := range cp {
		if e.TUs >= floor {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// genTUs produces a strictly increasing t_us sequence after start: the
// per-cast invariant the cast pump guarantees and the stream's
// t_us+1 cursor relies on.
func genTUs(t *rapid.T, n int, start int64) []int64 {
	out := make([]int64, n)
	cur := start
	for i := range out {
		cur += rapid.Int64Range(1, 200_000).Draw(t, "dt")
		out[i] = cur
	}
	return out
}

func TestConsoleStream_Property_BackfillDeliversEveryEventOnce(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cs := &pagedConsoleStore{cast: &model.ConsoleCast{ID: 7, Width: 80, Height: 24, CastStartedAt: time.Now()}}
		// Sizes straddle the 500-row page boundary.
		n := rapid.SampledFrom([]int{0, 1, 499, 500, 501, 999, 1000, 1001, 1337}).Draw(t, "n")
		for _, tus := range genTUs(t, n, 0) {
			cs.add(tus)
		}
		var floor int64
		q := ""
		if rapid.Bool().Draw(t, "useSince") && n > 0 {
			floor = cs.events[rapid.IntRange(0, n-1).Draw(t, "floorIdx")].TUs
			q = fmt.Sprintf("&since_us=%d", floor)
		}
		srv, inflight := consoleStreamServer(cs)
		defer srv.Close()
		conn := dialConsole(t, srv, q)
		defer conn.CloseNow() //nolint:errcheck
		want := expectedOrder(cs.events, floor)
		got, err := readEventIDs(conn, len(want)+1, 5*time.Second)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("backfill mismatch: got %d ids, want %d\n got  %v\n want %v", len(got), len(want), head(got), head(want))
		}
		// The read must end because the server closed the stream, not
		// because the client gave up: the client's timeout closes the
		// connection, which would let a handler that kept polling return.
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("non-follow stream still open after its backfill")
		}
		waitGroupTimeout(t, inflight, 3*time.Second, "non-follow handler did not return after backfill")
	})
}

func TestConsoleStream_Property_FollowDeliversLiveEventsAndReleasesOnDisconnect(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		cs := &pagedConsoleStore{cast: &model.ConsoleCast{ID: 7, Width: 80, Height: 24, CastStartedAt: time.Now()}}
		// Backlogs on and around one and two 500-row pages.
		n0 := rapid.SampledFrom([]int{0, 1, 2, 499, 500, 501, 1000, 1001}).Draw(t, "n0")
		last := int64(0)
		for _, tus := range genTUs(t, n0, 0) {
			cs.add(tus)
			last = tus
		}
		srv, inflight := consoleStreamServer(cs)
		defer srv.Close()
		conn := dialConsole(t, srv, "&follow=true&since_us=0")
		defer conn.CloseNow() //nolint:errcheck

		// Live events land in rounds either while the backlog is still
		// streaming or once the client has read all of it, so a round can
		// arrive mid-page, between pages or during the poll sleep. They
		// are read back once at the end, which keeps a case to at most
		// about one poll interval of waiting.
		var got []int64
		if !rapid.Bool().Draw(t, "liveDuringBackfill") {
			ids, err := readEventIDs(conn, n0, 5*time.Second)
			if err != nil {
				t.Fatalf("initial read: %v (got %d/%d)", err, len(ids), n0)
			}
			got = ids
		}
		rounds := rapid.IntRange(1, 3).Draw(t, "rounds")
		live := 0
		for r := 0; r < rounds; r++ {
			k := rapid.SampledFrom([]int{1, 3, 499, 500, 501}).Draw(t, "k")
			for _, tus := range genTUs(t, k, last) {
				cs.add(tus)
				last = tus
			}
			live += k
			time.Sleep(time.Duration(rapid.IntRange(0, 3).Draw(t, "gapMs")) * time.Millisecond)
		}
		rest := n0 + live - len(got)
		ids, err := readEventIDs(conn, rest, 10*time.Second)
		if err != nil {
			t.Fatalf("live read: %v (got %d/%d)", err, len(ids), rest)
		}
		got = append(got, ids...)
		want := expectedOrder(cs.events, 0)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("follow mismatch\n got  %v\n want %v", head(got), head(want))
		}

		// Disconnect: either a clean close frame or an abrupt drop.
		if rapid.Bool().Draw(t, "cleanClose") {
			_ = conn.Close(websocket.StatusNormalClosure, "bye")
		} else {
			_ = conn.CloseNow()
		}
		waitGroupTimeout(t, inflight, 3*time.Second, "follow handler still running after client disconnect")
	})
}

func head(ids []int64) []int64 {
	if len(ids) > 12 {
		return ids[:12]
	}
	return ids
}

func waitGroupTimeout(t interface{ Fatalf(string, ...any) }, wg *sync.WaitGroup, d time.Duration, msg string) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s", msg)
	}
}
