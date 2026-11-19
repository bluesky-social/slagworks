package store

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestRegisterSiteWorker_MintsToken(t *testing.T) {
	st := testStore(t)

	w, token, err := st.RegisterSiteWorker("pop3", "worker-1", "worker-1.pop3")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("expected non-empty plaintext token")
	}
	if w.TokenHash == "" {
		t.Fatal("expected non-empty token hash")
	}
	if w.TokenHash == token {
		t.Fatal("hash must not equal plaintext")
	}
	if got := HashWorkerToken(token); got != w.TokenHash {
		t.Fatalf("hash mismatch: got %q, want %q", got, w.TokenHash)
	}
}

func TestRegisterSiteWorker_RotatesOnReregister(t *testing.T) {
	st := testStore(t)

	_, token1, err := st.RegisterSiteWorker("pop3", "worker-1", "worker-1.pop3")
	if err != nil {
		t.Fatal(err)
	}
	second, token2, err := st.RegisterSiteWorker("pop3", "worker-1", "worker-1.pop3-renamed")
	if err != nil {
		t.Fatal(err)
	}

	if token1 == token2 {
		t.Fatal("re-registration should mint a new token")
	}
	// Row identity (first.ID vs second.ID) is GORM-decided; the
	// (slug, worker_id) upsert handles dedupe. We verify the rotation
	// by confirming the new FQDN landed and the old token no longer
	// authenticates.
	if second.FQDN != "worker-1.pop3-renamed" {
		t.Fatalf("fqdn not refreshed: %q", second.FQDN)
	}
	if _, err := st.GetSiteWorkerByTokenHash(HashWorkerToken(token1)); !errors.Is(err, ErrSiteWorkerNotFound) {
		t.Fatal("old token should no longer authenticate")
	}
}

func TestGetSiteWorkerByTokenHash(t *testing.T) {
	st := testStore(t)

	_, token, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3")
	if err != nil {
		t.Fatal(err)
	}

	got, err := st.GetSiteWorkerByTokenHash(HashWorkerToken(token))
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkerID != "worker-a" || got.SiteSlug != "pop3" {
		t.Fatalf("got slug=%q worker=%q", got.SiteSlug, got.WorkerID)
	}

	if _, err := st.GetSiteWorkerByTokenHash("deadbeef"); !errors.Is(err, ErrSiteWorkerNotFound) {
		t.Fatalf("expected ErrSiteWorkerNotFound, got %v", err)
	}
	if _, err := st.GetSiteWorkerByTokenHash(""); !errors.Is(err, ErrSiteWorkerNotFound) {
		t.Fatalf("empty hash should return ErrSiteWorkerNotFound, got %v", err)
	}
}

func TestTouchSiteWorker_DoesNotChangeToken(t *testing.T) {
	st := testStore(t)

	_, token, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	originalHash := HashWorkerToken(token)

	if _, err := st.TouchSiteWorker("pop3", "worker-a", "a.pop3-updated", nil); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSiteWorkerByTokenHash(originalHash)
	if err != nil {
		t.Fatalf("token should still authenticate after Touch: %v", err)
	}
	if got.FQDN != "a.pop3-updated" {
		t.Fatalf("fqdn not updated: %q", got.FQDN)
	}
}

func TestTwoWorkersDistinctTokens(t *testing.T) {
	st := testStore(t)

	_, t1, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	_, t2, err := st.RegisterSiteWorker("pop3", "worker-b", "b.pop3")
	if err != nil {
		t.Fatal(err)
	}
	if t1 == t2 {
		t.Fatal("distinct workers got same token")
	}
}

// TestTouchSiteWorker_UpdateOnly pins the B10 contract: Touch never
// creates a row. The old upsert could race RegisterSiteWorker and mint
// an unusable empty-token row for a worker that never registered.
func TestTouchSiteWorker_UpdateOnly(t *testing.T) {
	st := testStore(t)

	_, err := st.TouchSiteWorker("pop3", "never-registered", "ghost.pop3", nil)
	if !errors.Is(err, ErrSiteWorkerNotFound) {
		t.Fatalf("touch on unregistered worker: got %v want ErrSiteWorkerNotFound", err)
	}
	workers, err := st.ListSiteWorkers("pop3")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range workers {
		if w.WorkerID == "never-registered" {
			t.Fatal("touch created a row for an unregistered worker")
		}
	}
}

// TestTouchSiteWorker_HeartbeatKeepsFQDN: heartbeats touch with fqdn=""
// and must not blank the FQDN recorded at hello.
func TestTouchSiteWorker_HeartbeatKeepsFQDN(t *testing.T) {
	st := testStore(t)

	if _, _, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3"); err != nil {
		t.Fatal(err)
	}
	w, err := st.TouchSiteWorker("pop3", "worker-a", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.FQDN != "a.pop3" {
		t.Fatalf("heartbeat touch blanked fqdn: got %q want a.pop3", w.FQDN)
	}
}

// TestTouchSiteWorker_Serial pins the contract: a hello touch with a
// non-nil serial records it; a subsequent heartbeat (serial=nil) preserves it;
// and a hello with an empty serial (unverified worker) overwrites the stored
// value so the column reflects the currently-connected worker.
func TestTouchSiteWorker_Serial(t *testing.T) {
	st := testStore(t)

	if _, _, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3"); err != nil {
		t.Fatal(err)
	}

	// Hello with a verified serial.
	verified := "SN-VERIFIED"
	w, err := st.TouchSiteWorker("pop3", "worker-a", "a.pop3", &verified)
	if err != nil {
		t.Fatal(err)
	}
	if w.Serial != "SN-VERIFIED" {
		t.Fatalf("hello touch did not record serial: got %q", w.Serial)
	}

	// Heartbeat (serial=nil) must not blank the recorded serial.
	w, err = st.TouchSiteWorker("pop3", "worker-a", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.Serial != "SN-VERIFIED" {
		t.Fatalf("heartbeat blanked serial: got %q want SN-VERIFIED", w.Serial)
	}

	// Hello from an unverified worker (empty serial) overwrites it.
	empty := ""
	w, err = st.TouchSiteWorker("pop3", "worker-a", "a.pop3", &empty)
	if err != nil {
		t.Fatal(err)
	}
	if w.Serial != "" {
		t.Fatalf("empty-serial hello should clear stored serial: got %q", w.Serial)
	}
}

// TestListSiteWorkersBySerial_IgnoresCase: the column holds the worker's
// SMBIOS spelling and callers pass NetBox's spelling. The hello gate matched
// the two case-insensitively, so the lookup must too; an empty serial still
// matches nothing.
func TestListSiteWorkersBySerial_IgnoresCase(t *testing.T) {
	st := testStore(t)
	if _, _, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3"); err != nil {
		t.Fatal(err)
	}
	reported := "abc123"
	if _, err := st.TouchSiteWorker("pop3", "worker-a", "", &reported); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RegisterSiteWorker("pop3", "worker-b", "b.pop3"); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListSiteWorkersBySerial("ABC123")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].WorkerID != "worker-a" {
		t.Fatalf("ListSiteWorkersBySerial(%q) = %+v, want worker-a (serial %q)", "ABC123", got, reported)
	}
	if got, err := st.ListSiteWorkersBySerial(""); err != nil || len(got) != 0 {
		t.Fatalf("empty serial matched %+v (err %v), want nothing", got, err)
	}
}

func TestResolveSiteWorker_ByUUID(t *testing.T) {
	st := testStore(t)
	w, _, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.ResolveSiteWorker(w.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != w.ID {
		t.Fatalf("resolved id %s, want %s", got.ID, w.ID)
	}
}

func TestResolveSiteWorker_ByWorkerID(t *testing.T) {
	st := testStore(t)
	w, _, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.ResolveSiteWorker("worker-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != w.ID {
		t.Fatalf("resolved id %s, want %s", got.ID, w.ID)
	}
}

func TestResolveSiteWorker_ByFQDN(t *testing.T) {
	st := testStore(t)
	w, _, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.ResolveSiteWorker("a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != w.ID {
		t.Fatalf("resolved id %s, want %s", got.ID, w.ID)
	}
}

// TestResolveSiteWorker_ByUUIDShapedWorkerID: slagworksd generates a uuid
// for its worker_id unless SLAGWORKS_WORKER_ID overrides it, so a worker_id
// that parses as a uuid is the default, and it must still resolve even
// though no row has that id.
func TestResolveSiteWorker_ByUUIDShapedWorkerID(t *testing.T) {
	st := testStore(t)
	workerID := uuid.NewString()
	w, _, err := st.RegisterSiteWorker("pop3", workerID, "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.ResolveSiteWorker(workerID)
	if err != nil {
		t.Fatalf("resolve worker_id %s: %v", workerID, err)
	}
	if got.ID != w.ID {
		t.Fatalf("resolved id %s, want %s", got.ID, w.ID)
	}
}

// TestResolveSiteWorker_IDWinsOverWorkerID: when the identifier is one
// row's id and another row's worker_id, the id match wins. The uuid is
// what slagctl tells an operator to use to disambiguate, so it must keep
// naming exactly one row.
func TestResolveSiteWorker_IDWinsOverWorkerID(t *testing.T) {
	st := testStore(t)
	byID, _, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RegisterSiteWorker("pop4", byID.ID.String(), "b.pop4"); err != nil {
		t.Fatal(err)
	}
	got, err := st.ResolveSiteWorker(byID.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != byID.ID {
		t.Fatalf("resolved id %s, want %s", got.ID, byID.ID)
	}
}

func TestResolveSiteWorker_NotFound(t *testing.T) {
	st := testStore(t)
	if _, err := st.ResolveSiteWorker("ghost"); !errors.Is(err, ErrSiteWorkerNotFound) {
		t.Fatalf("got %v, want ErrSiteWorkerNotFound", err)
	}
	// A syntactically-valid but unregistered uuid is also not-found.
	if _, err := st.ResolveSiteWorker(uuid.NewString()); !errors.Is(err, ErrSiteWorkerNotFound) {
		t.Fatalf("unknown uuid: got %v, want ErrSiteWorkerNotFound", err)
	}
}

// TestResolveSiteWorker_Ambiguous registers the same worker_id under two
// site slugs, then resolves by that shared worker_id — expect an
// *AmbiguousSiteWorkerError listing both candidates.
func TestResolveSiteWorker_Ambiguous(t *testing.T) {
	st := testStore(t)
	if _, _, err := st.RegisterSiteWorker("pop3", "worker-shared", "a.pop3"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RegisterSiteWorker("pop4", "worker-shared", "b.pop4"); err != nil {
		t.Fatal(err)
	}
	_, err := st.ResolveSiteWorker("worker-shared")
	var amb *AmbiguousSiteWorkerError
	if !errors.As(err, &amb) {
		t.Fatalf("got %v, want *AmbiguousSiteWorkerError", err)
	}
	if len(amb.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(amb.Candidates))
	}
}

// TestDeleteSiteWorker_RemovesRow deletes by ID and confirms the row is
// gone from ListSiteWorkers and the old token no longer authenticates.
func TestDeleteSiteWorker_RemovesRow(t *testing.T) {
	st := testStore(t)
	w, token, err := st.RegisterSiteWorker("pop3", "worker-a", "a.pop3")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteSiteWorker(w.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	workers, err := st.ListSiteWorkers("")
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range workers {
		if got.ID == w.ID {
			t.Fatal("deleted worker still present in ListSiteWorkers")
		}
	}
	if _, err := st.GetSiteWorkerByTokenHash(HashWorkerToken(token)); !errors.Is(err, ErrSiteWorkerNotFound) {
		t.Fatalf("old token still authenticates: %v", err)
	}
}

func TestDeleteSiteWorker_NotFound(t *testing.T) {
	st := testStore(t)
	if err := st.DeleteSiteWorker(uuid.New()); !errors.Is(err, ErrSiteWorkerNotFound) {
		t.Fatalf("got %v, want ErrSiteWorkerNotFound", err)
	}
}
