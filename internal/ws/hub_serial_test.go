package ws

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

// helloAndRead dials the hub, sends the given hello, and returns the reply
// envelope type. Fails the test on dial/write/read errors.
func helloAndRead(t *testing.T, srv *httptest.Server, token string, hello HelloMsg) Envelope {
	t.Helper()
	conn := dialHub(t, srv, token)
	if conn == nil {
		t.Fatal("dial failed")
	}
	t.Cleanup(func() { conn.CloseNow() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.WriteTyped(ctx, TypeHello, "", hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	env, err := conn.ReadMessage(ctx)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return env
}

// TestHub_HelloMatchingSerialAccepted: a non-empty serial that the resolver
// accepts proceeds to a normal snapshot reply, records the serial on the touch
// call, and consults the resolver exactly once.
func TestHub_HelloMatchingSerialAccepted(t *testing.T) {
	sr := newFakeSerialResolver() // empty byERR + nil fallErr => accept all
	h, auth := newTestHubWithSerial(t, sr)
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	env := helloAndRead(t, srv, "tok-1", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 0, Proto: OpStreamProto, Serial: "SN-ABC"})
	if env.Type != TypeSnapshot {
		t.Fatalf("expected snapshot, got %q", env.Type)
	}
	if sr.callCount() != 1 {
		t.Fatalf("expected resolver consulted once, got %d", sr.callCount())
	}
	auth.mu.Lock()
	defer auth.mu.Unlock()
	if len(auth.touches) == 0 {
		t.Fatal("expected a TouchSiteWorker call")
	}
	last := auth.touches[len(auth.touches)-1]
	if last.serial == nil || *last.serial != "SN-ABC" {
		t.Fatalf("expected touch to record serial SN-ABC, got %v", last.serial)
	}
}

// TestHub_HelloSerialZeroMatchRefused: a non-empty serial that resolves to no
// NetBox device refuses the connection with a serial_mismatch error.
func TestHub_HelloSerialZeroMatchRefused(t *testing.T) {
	sr := newFakeSerialResolver()
	sr.byERR["SN-GHOST"] = ErrSerialUnknown
	h, auth := newTestHubWithSerial(t, sr)
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	env := helloAndRead(t, srv, "tok-1", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 0, Proto: OpStreamProto, Serial: "SN-GHOST"})
	if env.Type != TypeError {
		t.Fatalf("expected error, got %q", env.Type)
	}
	var e ErrorMsg
	if err := unmarshalData(env.Data, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != "serial_mismatch" {
		t.Fatalf("expected serial_mismatch, got %q", e.Code)
	}
}

// TestHub_HelloSerialMultipleMatchRefused: a serial that resolves to multiple
// NetBox devices (duplicate serials) refuses the connection.
func TestHub_HelloSerialMultipleMatchRefused(t *testing.T) {
	sr := newFakeSerialResolver()
	sr.byERR["SN-DUP"] = ErrSerialAmbiguous
	h, auth := newTestHubWithSerial(t, sr)
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	env := helloAndRead(t, srv, "tok-1", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 0, Proto: OpStreamProto, Serial: "SN-DUP"})
	if env.Type != TypeError {
		t.Fatalf("expected error, got %q", env.Type)
	}
}

// TestHub_HelloSerialOutOfScopeRefused: a serial resolving to a device outside
// the worker's site scope refuses the connection.
func TestHub_HelloSerialOutOfScopeRefused(t *testing.T) {
	sr := newFakeSerialResolver()
	sr.byERR["SN-OTHER"] = ErrSerialOutOfScope
	h, auth := newTestHubWithSerial(t, sr)
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	env := helloAndRead(t, srv, "tok-1", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 0, Proto: OpStreamProto, Serial: "SN-OTHER"})
	if env.Type != TypeError {
		t.Fatalf("expected error, got %q", env.Type)
	}
}

// TestHub_HelloEmptySerialRefused: an empty serial is REFUSED when serial
// enforcement is on (a real resolver wired). There is no tolerated-but-
// unverified state — an old binary or a bad DMI read must be fixed, not
// connected around (no transitional rollout hedge). The resolver is not
// consulted (nothing to resolve); the refusal is the empty serial itself.
func TestHub_HelloEmptySerialRefused(t *testing.T) {
	sr := newFakeSerialResolver()
	h, auth := newTestHubWithSerial(t, sr)
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	env := helloAndRead(t, srv, "tok-1", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 0, Proto: OpStreamProto, Serial: ""})
	if env.Type != TypeError {
		t.Fatalf("expected error (empty serial refused under enforcement), got %q", env.Type)
	}
	var e ErrorMsg
	if err := unmarshalData(env.Data, &e); err != nil {
		t.Fatal(err)
	}
	if e.Code != "serial_missing" {
		t.Fatalf("expected serial_missing, got %q", e.Code)
	}
	if sr.callCount() != 0 {
		t.Fatalf("resolver must NOT be consulted for an empty serial, got %d calls", sr.callCount())
	}
}

// TestHub_HelloEmptySerialNilResolverAccepted: with NO resolver wired
// (dev/test — the nil-config default, same shape as a nil bmcSecrets), serial
// validation is skipped entirely and an empty serial connects. This is the
// ONLY unverified mode; production wires a real resolver and enforces.
func TestHub_HelloEmptySerialNilResolverAccepted(t *testing.T) {
	h, auth, _ := newTestHub(t) // no SerialResolver in config
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	env := helloAndRead(t, srv, "tok-1", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 0, Proto: OpStreamProto, Serial: ""})
	if env.Type != TypeSnapshot {
		t.Fatalf("expected snapshot (nil resolver skips serial validation), got %q", env.Type)
	}
}

// TestHub_HelloSerialLookupFailureRefused: a transient resolver error (NetBox
// down) is fail-closed — the connection is refused, never accepted.
func TestHub_HelloSerialLookupFailureRefused(t *testing.T) {
	sr := newFakeSerialResolver()
	sr.fallErr = context.DeadlineExceeded // not one of the three sentinels
	h, auth := newTestHubWithSerial(t, sr)
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	env := helloAndRead(t, srv, "tok-1", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 0, Proto: OpStreamProto, Serial: "SN-XYZ"})
	if env.Type != TypeError {
		t.Fatalf("expected error (fail-closed on lookup failure), got %q", env.Type)
	}
}

// TestHub_HelloNoResolverConfigured: with no SerialResolver wired (the
// nil-config default), a non-empty serial is accepted as a no-op — preserving
// the pre-serial-verification behavior for a global built without NetBox serial verification.
func TestHub_HelloNoResolverConfigured(t *testing.T) {
	h, auth, _ := newTestHub(t) // no SerialResolver -> noopSerialResolver
	auth.register("tok-1", "pop3", "worker-1")
	h.Push("pop3", model.SiteSnapshot{SeqNum: 1, Site: model.SiteInfo{Slug: "pop3"}})

	srv := httptest.NewServer(h)
	defer srv.Close()

	env := helloAndRead(t, srv, "tok-1", HelloMsg{
		WorkerID: "worker-1", LastSeqNum: 0, Proto: OpStreamProto, Serial: "SN-ANYTHING"})
	if env.Type != TypeSnapshot {
		t.Fatalf("expected snapshot with no resolver, got %q", env.Type)
	}
}
