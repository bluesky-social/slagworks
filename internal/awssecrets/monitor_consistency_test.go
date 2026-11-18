package awssecrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// fakeSecretsAPI is an in-memory Secrets Manager holding one secret whose
// reads lag its writes, the way pop/pop2/bmc-creds-monitor's did on
// 2026-10-28. For currentLag after a PutSecretValue, AWSCURRENT reads return
// the newest version older than that window, so a burst of writes can leave a
// read several versions behind. For idLag, a by-VersionId read of the new
// version reports ResourceNotFoundException. CreatedDate comes from a logical
// clock advancing 1ms per version, so version order is strict regardless of
// wall-clock resolution; visibility uses the wall clock.
type fakeSecretsAPI struct {
	mu         sync.Mutex
	versions   []fakeVersion // creation order; the last is AWSCURRENT
	currentLag time.Duration
	idLag      time.Duration
	puts       int
	staleReads int // AWSCURRENT reads answered with an older version than the newest
}

type fakeVersion struct {
	id      string
	created time.Time // AWS-side CreatedDate
	putAt   time.Time // wall clock; zero for the seed, visible from the start
	body    string
}

var fakeEpoch = time.Date(2026, 10, 28, 23, 29, 37, 0, time.UTC)

func newFakeSecretsAPI(seed string) *fakeSecretsAPI {
	return &fakeSecretsAPI{versions: []fakeVersion{{id: "v0", created: fakeEpoch, body: seed}}}
}

func (v fakeVersion) visible(lag time.Duration) bool {
	return v.putAt.IsZero() || time.Since(v.putAt) >= lag
}

func (v fakeVersion) output() *secretsmanager.GetSecretValueOutput {
	id, body, created := v.id, v.body, v.created
	return &secretsmanager.GetSecretValueOutput{VersionId: &id, SecretString: &body, CreatedDate: &created}
}

func (f *fakeSecretsAPI) setLag(current, byID time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.currentLag, f.idLag = current, byID
}

func (f *fakeSecretsAPI) addVersionLocked(body string) string {
	n := len(f.versions)
	id := fmt.Sprintf("v%d", n)
	f.versions = append(f.versions, fakeVersion{
		id:      id,
		created: fakeEpoch.Add(time.Duration(n) * time.Millisecond),
		putAt:   time.Now(),
		body:    body,
	})
	return id
}

func (f *fakeSecretsAPI) GetSecretValue(_ context.Context, in *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.VersionId != nil {
		for _, v := range f.versions {
			if v.id == *in.VersionId && v.visible(f.idLag) {
				return v.output(), nil
			}
		}
		msg := "Secrets Manager can't find the specified secret value for VersionId: " + *in.VersionId
		return nil, &types.ResourceNotFoundException{Message: &msg}
	}
	for i := len(f.versions) - 1; i >= 0; i-- {
		if f.versions[i].visible(f.currentLag) {
			if i != len(f.versions)-1 {
				f.staleReads++
			}
			return f.versions[i].output(), nil
		}
	}
	return nil, errors.New("fakeSecretsAPI: no visible version (the seed is always visible)")
}

func (f *fakeSecretsAPI) PutSecretValue(_ context.Context, in *secretsmanager.PutSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	id := f.addVersionLocked(*in.SecretString)
	return &secretsmanager.PutSecretValueOutput{VersionId: &id}, nil
}

func (f *fakeSecretsAPI) DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	return &secretsmanager.DescribeSecretOutput{}, nil
}

func (f *fakeSecretsAPI) CreateSecret(context.Context, *secretsmanager.CreateSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	return nil, errors.New("fakeSecretsAPI: CreateSecret is not modeled")
}

// externalPut writes a version the way an operator's
// `aws secretsmanager put-secret-value` would: not through the Client.
func (f *fakeSecretsAPI) externalPut(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addVersionLocked(body)
}

// latest returns the newest version's entries, bypassing read lag: the
// ground truth of what the last write left in the secret.
func (f *fakeSecretsAPI) latest(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	f.mu.Lock()
	body := f.versions[len(f.versions)-1].body
	f.mu.Unlock()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("latest version is not JSON: %v (%s)", err, body)
	}
	return m
}

func (f *fakeSecretsAPI) counts() (puts, staleReads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts, f.staleReads
}

const seedMonitor = `{"_meta":{"tier":"monitor"}}`

// newTestMonitorClient builds a Client over api with short retry delays and
// a consistency wait long enough that only a read that never catches up
// exhausts it.
func newTestMonitorClient(api secretsAPI) *Client {
	c := newClient(api, time.Minute)
	c.consistencyWait = 5 * time.Second
	c.consistencyBackoff = 2 * time.Millisecond
	return c
}

func testMonitorEntry(password string) BMCMonitorEntry {
	return BMCMonitorEntry{
		Kind: KindBMC, Username: "monitor", Password: password,
		ExtraConfig: map[string]any{"ipmi_user_slot": 4},
	}
}

func requireKeys(t *testing.T, got map[string]json.RawMessage, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := got[k]; !ok {
			t.Errorf("secret is missing %q; has %d keys", k, len(got))
		}
	}
	if len(got) != len(keys) {
		t.Errorf("secret has %d keys, want %d (%v)", len(got), len(keys), keys)
	}
}

// TestUpsertMonitorEntry_RetriesStaleRead reproduces the pop2 loss: the write
// after SN-A's reads a version from before SN-A's write. The Client must wait
// for a read at least as new as its own last write, so the final version
// carries both entries instead of silently dropping SN-A.
func TestUpsertMonitorEntry_RetriesStaleRead(t *testing.T) {
	api := newFakeSecretsAPI(seedMonitor)
	// AWSCURRENT lags each write; the by-VersionId read-back does not.
	api.setLag(250*time.Millisecond, 0)
	c := newTestMonitorClient(api)

	if err := c.UpsertMonitorEntry(t.Context(), "pop2", "SN-A", testMonitorEntry("AAAA")); err != nil {
		t.Fatalf("upsert SN-A: %v", err)
	}
	if err := c.UpsertMonitorEntry(t.Context(), "pop2", "SN-B", testMonitorEntry("BBBB")); err != nil {
		t.Fatalf("upsert SN-B: %v", err)
	}

	requireKeys(t, api.latest(t), "_meta", "SN-A", "SN-B")
	if puts, stale := api.counts(); stale == 0 || puts != 2 {
		t.Errorf("puts=%d staleReads=%d; want 2 puts and SN-B's first read to have been stale", puts, stale)
	}
}

// TestUpsertMonitorEntry_StaleReadNeverCatchesUp: when AWSCURRENT never
// catches up with the last write within the wait, the upsert fails with a
// retryable (not ErrMonitorSecretMissing) error and does NOT Put, because any
// write built on the stale read would drop SN-A.
func TestUpsertMonitorEntry_StaleReadNeverCatchesUp(t *testing.T) {
	api := newFakeSecretsAPI(seedMonitor)
	c := newTestMonitorClient(api)
	if err := c.UpsertMonitorEntry(t.Context(), "pop2", "SN-A", testMonitorEntry("AAAA")); err != nil {
		t.Fatalf("upsert SN-A: %v", err)
	}

	api.setLag(time.Hour, 0)
	c.consistencyWait = 100 * time.Millisecond
	err := c.UpsertMonitorEntry(t.Context(), "pop2", "SN-B", testMonitorEntry("BBBB"))
	if err == nil {
		t.Fatal("expected an error for a read that never caught up")
	}
	if !strings.Contains(err.Error(), "older than this process's last write") {
		t.Errorf("error should say the read is older than the last write: %v", err)
	}
	if errors.Is(err, ErrMonitorSecretMissing) {
		t.Errorf("a stale read is retryable and must not be ErrMonitorSecretMissing (terminal): %v", err)
	}
	if puts, _ := api.counts(); puts != 1 {
		t.Errorf("puts = %d, want 1: SN-B must not write on a stale read", puts)
	}
	requireKeys(t, api.latest(t), "_meta", "SN-A")
}

// TestUpsertMonitorEntry_AcceptsNewerExternalVersion: a version created after
// this process's last write (an operator's out-of-band edit) is not stale and
// is built on directly, keeping the operator's entry.
func TestUpsertMonitorEntry_AcceptsNewerExternalVersion(t *testing.T) {
	api := newFakeSecretsAPI(seedMonitor)
	c := newTestMonitorClient(api)
	if err := c.UpsertMonitorEntry(t.Context(), "pop2", "SN-A", testMonitorEntry("AAAA")); err != nil {
		t.Fatalf("upsert SN-A: %v", err)
	}

	edited := api.latest(t)
	edited["SN-OPERATOR"] = json.RawMessage(`{"kind":"bmc","username":"monitor","password":"OPER"}`)
	body, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	api.externalPut(string(body))

	// Short wait: mistaking the newer version for a stale one fails fast.
	c.consistencyWait = 100 * time.Millisecond
	if err := c.UpsertMonitorEntry(t.Context(), "pop2", "SN-B", testMonitorEntry("BBBB")); err != nil {
		t.Fatalf("upsert SN-B over a newer external version: %v", err)
	}
	requireKeys(t, api.latest(t), "_meta", "SN-A", "SN-OPERATOR", "SN-B")
	if _, stale := api.counts(); stale != 0 {
		t.Errorf("staleReads = %d, want 0", stale)
	}
}

// TestUpsertMonitorEntry_UnconfirmedWriteErrors: if the version Put created
// never becomes readable by VersionId, the upsert reports an error (so the
// caller replays it) rather than claiming a write it could not confirm.
func TestUpsertMonitorEntry_UnconfirmedWriteErrors(t *testing.T) {
	api := newFakeSecretsAPI(seedMonitor)
	api.setLag(0, time.Hour)
	c := newTestMonitorClient(api)
	c.consistencyWait = 100 * time.Millisecond

	err := c.UpsertMonitorEntry(t.Context(), "pop2", "SN-A", testMonitorEntry("AAAA"))
	if err == nil {
		t.Fatal("expected an error when the written version is never readable")
	}
	if !strings.Contains(err.Error(), "not readable") {
		t.Errorf("error should say the written version was not readable: %v", err)
	}
	if errors.Is(err, ErrMonitorSecretMissing) {
		t.Errorf("an unconfirmed write is retryable and must not be ErrMonitorSecretMissing (terminal): %v", err)
	}
}

// TestUpsertMonitorEntry_ConcurrentSamePopLosesNothing drives many upserts
// for one pop through the real Client, serialized the way
// store.WithPopAdvisoryLock serializes them (which orders the writers but
// gives no read-after-write consistency), against reads that lag every
// write. Every entry must survive.
func TestUpsertMonitorEntry_ConcurrentSamePopLosesNothing(t *testing.T) {
	api := newFakeSecretsAPI(seedMonitor)
	api.setLag(40*time.Millisecond, 10*time.Millisecond)
	c := newTestMonitorClient(api)

	const n = 16
	var popLock sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, n)
	want := []string{"_meta"}
	for i := range n {
		serial := fmt.Sprintf("SN-%02d", i)
		want = append(want, serial)
		wg.Go(func() {
			popLock.Lock()
			defer popLock.Unlock()
			errs <- c.UpsertMonitorEntry(t.Context(), "pop2", serial, testMonitorEntry("PW"+serial))
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("upsert: %v", err)
		}
	}

	requireKeys(t, api.latest(t), want...)
	if puts, stale := api.counts(); puts != n || stale == 0 {
		t.Errorf("puts=%d staleReads=%d; want %d puts and at least one stale read (else the race was not exercised)", puts, stale, n)
	}
}

// TestGetMonitorEntry_ReturnsSerialEntry: the read returns the serial's
// entry from the pop's monitor secret, and nil (no error) for a serial the
// secret has no entry for: a new BMC.
func TestGetMonitorEntry_ReturnsSerialEntry(t *testing.T) {
	api := newFakeSecretsAPI(`{"_meta":{"tier":"monitor"},"SN-A":{"kind":"bmc","username":"monitor","password":"AAAA","extra_config":{"ipmi_user_slot":4}}}`)
	c := newTestMonitorClient(api)

	got, err := c.GetMonitorEntry(t.Context(), "pop2", "SN-A")
	if err != nil {
		t.Fatalf("GetMonitorEntry: %v", err)
	}
	if got == nil || got.Username != "monitor" || got.Password != "AAAA" || got.Kind != KindBMC {
		t.Errorf("entry = %+v", got)
	}
	none, err := c.GetMonitorEntry(t.Context(), "pop2", "SN-NEW")
	if err != nil || none != nil {
		t.Errorf("absent serial: entry=%+v err=%v, want nil, nil", none, err)
	}
	if _, err := c.GetMonitorEntry(t.Context(), "pop2", "_meta"); err == nil {
		t.Error("_meta is not a host entry and must be refused")
	}
}

// TestGetMonitorEntry_WaitsForOwnLastWrite: right after this process
// rotates a host's monitor password, a read that Secrets Manager answers
// with the pre-rotation version must not be used: a bmc_reconcile would
// re-assert the stale password on the BMC while the store holds the new one.
// The read waits for a version at least as new as the last write.
func TestGetMonitorEntry_WaitsForOwnLastWrite(t *testing.T) {
	api := newFakeSecretsAPI(seedMonitor)
	c := newTestMonitorClient(api)
	if err := c.UpsertMonitorEntry(t.Context(), "pop2", "SN-A", testMonitorEntry("OLD")); err != nil {
		t.Fatalf("upsert OLD: %v", err)
	}
	api.setLag(250*time.Millisecond, 0)
	if err := c.UpsertMonitorEntry(t.Context(), "pop2", "SN-A", testMonitorEntry("NEW")); err != nil {
		t.Fatalf("upsert NEW: %v", err)
	}

	got, err := c.GetMonitorEntry(t.Context(), "pop2", "SN-A")
	if err != nil {
		t.Fatalf("GetMonitorEntry: %v", err)
	}
	if got == nil || got.Password != "NEW" {
		t.Errorf("entry = %+v, want the password this process last wrote", got)
	}
	if _, stale := api.counts(); stale == 0 {
		t.Error("expected at least one stale AWSCURRENT read (else the lag was not exercised)")
	}
}

// notFoundSecretsAPI answers every read with ResourceNotFoundException: the
// pop's monitor secret was never bootstrapped.
type notFoundSecretsAPI struct{ fakeSecretsAPI }

func (*notFoundSecretsAPI) GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	msg := "Secrets Manager can't find the specified secret."
	return nil, &types.ResourceNotFoundException{Message: &msg}
}

// TestGetMonitorEntry_MissingSecret: a pop with no monitor secret is
// ErrMonitorSecretMissing, the same terminal sentinel the upsert returns.
func TestGetMonitorEntry_MissingSecret(t *testing.T) {
	c := newTestMonitorClient(&notFoundSecretsAPI{})
	_, err := c.GetMonitorEntry(t.Context(), "pop9", "SN-A")
	if !errors.Is(err, ErrMonitorSecretMissing) {
		t.Fatalf("err = %v, want ErrMonitorSecretMissing", err)
	}
}

// TestFakeReader_GetMonitorEntry: the fake serves Monitor entries by pop and
// serial, nil for an unknown serial, and ErrMonitorSecretMissing for a pop
// MonitorPresent marks absent.
func TestFakeReader_GetMonitorEntry(t *testing.T) {
	fake := &FakeReader{Monitor: map[string]map[string]BMCMonitorEntry{
		"pop1": {"SN-A": testMonitorEntry("AAAA")},
	}}
	got, err := fake.GetMonitorEntry(t.Context(), "pop1", "SN-A")
	if err != nil || got == nil || got.Password != "AAAA" {
		t.Errorf("entry = %+v err=%v", got, err)
	}
	if got, err := fake.GetMonitorEntry(t.Context(), "pop1", "SN-B"); err != nil || got != nil {
		t.Errorf("unknown serial: entry=%+v err=%v, want nil, nil", got, err)
	}
	fake.MonitorPresent = map[string]bool{"pop1": false}
	if _, err := fake.GetMonitorEntry(t.Context(), "pop1", "SN-A"); !errors.Is(err, ErrMonitorSecretMissing) {
		t.Errorf("absent secret: err = %v, want ErrMonitorSecretMissing", err)
	}
}
