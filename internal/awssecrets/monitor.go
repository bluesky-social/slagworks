package awssecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// ErrMonitorSecretMissing is returned by UpsertMonitorEntry when the per-pop
// monitor secret (pop/<pop>/bmc-creds-monitor) does not exist — the pop wasn't
// bootstrapped. It is an operator-attention condition that RETRYING CANNOT FIX
// (slagworksd lacks the IRSA grant to CreateSecret), so callers driving a
// retry loop (the deploy_complete observation handler) must treat it as
// terminal rather than replaying forever. errors.Is against this sentinel.
var ErrMonitorSecretMissing = errors.New("awssecrets: monitor secret does not exist; pop must be bootstrapped")

const (
	defaultConsistencyWait    = 10 * time.Second
	defaultConsistencyBackoff = 50 * time.Millisecond
	maxConsistencyBackoff     = time.Second
)

// monitorVersion identifies one version of a pop's monitor secret: its
// VersionId and AWS's CreatedDate for it. Client.monitorWritten holds the
// last one this process wrote per pop.
type monitorVersion struct {
	id      string
	created time.Time
}

// BMCMonitorEntry is one host's entry in the per-pop monitor secret,
// keyed by NetBox device serial (the same key as bmc-creds-admin;
// hostnames drift, serials don't). LastKnownHostname and LastKnownIP
// (the BMC's NetBox oob_ip) are informational: they map a serial-keyed
// entry back to a host for humans and external tooling, and are
// refreshed on every write.
//
// Unlike BMCAdminHost, password is exposed here — slagworksd writes it,
// and reads it back (GetMonitorEntry) for a bmc_reconcile op to re-assert,
// so it has to round-trip through process memory. The secret contents
// stay in memory only for the duration of the read-modify-write
// transaction, which is bounded by the per-pop advisory lock window, or of
// the op-creation request that reads one entry.
//
// UnconfirmedPasswords holds generated passwords a reconcile may have
// written to the account without learning the outcome, oldest first. While
// it is non-empty the BMC holds either Password or one of them; the next
// bmc_reconcile op finds out which and the list is dropped. Consumers of
// the entry keep using Password.
type BMCMonitorEntry struct {
	Kind                 string         `json:"kind"`
	Username             string         `json:"username"`
	Password             string         `json:"password"`
	UnconfirmedPasswords []string       `json:"unconfirmed_passwords,omitempty"`
	ExtraConfig          map[string]any `json:"extra_config"`
	LastKnownHostname    string         `json:"last_known_hostname"`
	LastKnownIP          string         `json:"last_known_ip"`
}

// MonitorWriter is the slagworksd-facing interface for the per-pop
// monitor secret. Production talks to AWS Secrets Manager via the
// IRSA role (newSlagworksBMCSecretsIRSARole gained PutSecretValue on
// `pop/*/bmc-creds-monitor-*` in 2026-05-20); tests use the
// FakeMonitorWriter below.
//
// UpsertMonitorEntry is the read-modify-write primitive: caller passes
// the device's NetBox serial and the new entry, the implementation
// reads the secret, replaces or inserts that serial's entry, and writes
// the secret back. The secret must already exist (EnsureMonitorSecret /
// an operator's bootstrap); a missing secret is ErrMonitorSecretMissing.
type MonitorWriter interface {
	UpsertMonitorEntry(ctx context.Context, pop, serial string, entry BMCMonitorEntry) error
	// EnsureMonitorSecret creates the pop's monitor secret with a _meta-only
	// stub if absent (no-op if present). Called at site-config bootstrap so the
	// secret exists before the first deploy_complete upsert. Idempotent.
	EnsureMonitorSecret(ctx context.Context, pop string) error
}

// UpsertMonitorEntry implements MonitorWriter for the production Client:
// read AWSCURRENT, replace or insert the serial's entry, PutSecretValue.
//
// Secrets Manager has no compare-and-swap, so the caller must serialize
// writers per pop (slagworksd holds store.WithPopAdvisoryLock); unserialized
// concurrent writers lose entries. Serializing is not sufficient on its own:
// Secrets Manager reads are eventually consistent, so the next lock holder's
// read can return a version older than the previous holder's write, and a
// write built on it silently drops every entry added since (pop2,
// 2026-10-28: a bmc_reconcile write built on a read several versions stale
// and dropped three hosts' entries). So every write is read back by
// VersionId to learn AWS's CreatedDate for it, which becomes the pop's
// watermark (Client.monitorWritten), and the read step retries a version
// older than the watermark until Secrets Manager catches up, refusing with
// an error if it never does (see readMonitorSecret). The watermark only
// covers this process's own writes: a stale read can still drop an
// out-of-band edit this process never saw.
//
// The secret resource (pop/<pop>/bmc-creds-monitor) MUST exist before
// the first call. It is
// pre-created by the bootstrap procedure with a `_meta`-only stub.
// We do not attempt CreateSecret here — that would need a broader
// IRSA grant than the Get/Describe/Put we have today, and the
// "secret doesn't exist" path is genuinely an operator-attention
// situation (the pop wasn't bootstrapped) rather than something we
// should silently paper over.
func (c *Client) UpsertMonitorEntry(ctx context.Context, pop, serial string, entry BMCMonitorEntry) error {
	if pop == "" {
		return errors.New("awssecrets: empty pop")
	}
	if serial == "" {
		return errors.New("awssecrets: empty serial; monitor entries are keyed by NetBox device serial")
	}
	if serial == "_meta" {
		return errors.New("awssecrets: serial _meta is reserved")
	}

	name := "pop/" + pop + "/bmc-creds-monitor"

	// 1) Read the current secret, no older than this process's last
	// write to it.
	existing, err := c.readMonitorSecret(ctx, pop, name)
	if err != nil {
		return err
	}

	// 2) Modify in-memory. The serial key replaces any existing entry
	// for that serial; the "_meta" reservation is enforced at the
	// top of this function before we hit AWS.
	//
	// gosec flags this as a password-pattern marshal; the whole
	// purpose of this method is to serialize the cred for an AWS
	// PutSecretValue call, so the lint is correctly identifying the
	// behavior but the behavior is intended.
	encoded, err := json.Marshal(entry) //nolint:gosec // intentional: serializing for AWS PutSecretValue
	if err != nil {
		return fmt.Errorf("marshal entry for %s: %w", serial, err)
	}
	existing[serial] = encoded

	// 3) Write back. Marshal with sorted keys so PutSecretValue is
	// deterministic — useful for diffs in CloudTrail and for tests
	// that want to assert byte-equal payloads.
	body, err := marshalSorted(existing)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", name, err)
	}
	bodyStr := string(body)

	put, err := c.api.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId:     &name,
		SecretString: &bodyStr,
	})
	if err != nil {
		return fmt.Errorf("PutSecretValue %s: %w", name, err)
	}
	if put.VersionId == nil {
		return fmt.Errorf("PutSecretValue %s returned no VersionId, so the write cannot be confirmed; retry the upsert", name)
	}

	// 4) Confirm the write and advance the pop's watermark to it.
	return c.confirmMonitorWrite(ctx, pop, name, *put.VersionId, serial, encoded)
}

// GetMonitorEntry implements Reader: it reads the pop's monitor secret, no
// older than this process's last write to it (see readMonitorSecret), and
// returns the serial's entry, or nil when the secret has none. The
// no-older-than rule matters for this read too: right after a rotation is
// stored, a stale read would hand a bmc_reconcile op the pre-rotation
// password to re-assert, leaving the BMC and the store disagreeing.
func (c *Client) GetMonitorEntry(ctx context.Context, pop, serial string) (*BMCMonitorEntry, error) {
	if pop == "" {
		return nil, errors.New("awssecrets: empty pop")
	}
	if serial == "" {
		return nil, errors.New("awssecrets: empty serial; monitor entries are keyed by NetBox device serial")
	}
	if serial == "_meta" {
		return nil, errors.New("awssecrets: serial _meta is reserved")
	}
	name := "pop/" + pop + "/bmc-creds-monitor"
	entries, err := c.readMonitorSecret(ctx, pop, name)
	if err != nil {
		return nil, err
	}
	raw, ok := entries[serial]
	if !ok {
		return nil, nil
	}
	var e BMCMonitorEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("parse %s entry for %s: %w", name, serial, err)
	}
	return &e, nil
}

// readMonitorSecret reads AWSCURRENT of the pop's monitor secret and returns
// its entries. A version created before the pop's watermark (this process's
// last confirmed write) is a stale read: it is retried with backoff for up to
// consistencyWait, then refused, because it lacks what this process wrote
// since: a write built on it would drop those entries, and a lookup in it
// would return superseded passwords. A version at or after the watermark is
// accepted, including one written outside slagworks (an operator edit).
func (c *Client) readMonitorSecret(ctx context.Context, pop, name string) (map[string]json.RawMessage, error) {
	last := c.monitorWatermark(pop)
	var out *secretsmanager.GetSecretValueOutput
	err := c.awaitConsistent(ctx, func() (bool, error) {
		var err error
		out, err = c.api.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &name})
		if err != nil {
			if isResourceNotFoundError(err) {
				// The bootstrap pre-creates the secret with a `_meta`-only
				// stub, so not-found means the pop wasn't bootstrapped. Wrap
				// the sentinel so callers treat it as terminal: no amount of
				// retry creates the secret.
				return false, fmt.Errorf("%s: %w", name, ErrMonitorSecretMissing)
			}
			return false, fmt.Errorf("GetSecretValue %s: %w", name, err)
		}
		if last.created.IsZero() {
			return true, nil
		}
		if out.CreatedDate == nil {
			return false, fmt.Errorf("GetSecretValue %s returned no CreatedDate, so the read cannot be checked against this process's last write (version %s); refusing to use it, retry",
				name, last.id)
		}
		return !out.CreatedDate.Before(last.created), nil
	})
	if errors.Is(err, errNotConsistent) {
		return nil, fmt.Errorf("%s: after %s Secrets Manager still serves version %s (created %s), older than this process's last write, version %s (created %s); "+
			"refusing it because it lacks what was written since (an upsert built on it would drop those entries, a lookup would return superseded passwords). Retry once Secrets Manager catches up; "+
			"if AWSCURRENT was deliberately moved to an older version, restart slagworksd global to reset the watermark",
			name, c.consistencyWait, derefString(out.VersionId), out.CreatedDate.Format(time.RFC3339Nano),
			last.id, last.created.Format(time.RFC3339Nano))
	}
	if err != nil {
		return nil, err
	}
	return parseMonitorSecret(name, out)
}

// confirmMonitorWrite reads the version PutSecretValue created back by
// VersionId, retrying while it is not yet readable, advances the pop's
// watermark to AWS's CreatedDate for it, and checks it carries the serial's
// entry. AWS's timestamp, not the local clock, is what later reads are
// compared against. If the version cannot be read back, the watermark stays
// at the previous confirmed write; once it has been read back the watermark
// advances even if the entry check then fails, because that version is still
// the newest this process wrote and carries every other host's entries. Every
// failure asks for a replay; both callers treat it as transient and replay
// the upsert, which restores this entry even if a later writer's read
// predated this version.
func (c *Client) confirmMonitorWrite(ctx context.Context, pop, name, versionID, serial string, encoded json.RawMessage) error {
	var out *secretsmanager.GetSecretValueOutput
	err := c.awaitConsistent(ctx, func() (bool, error) {
		var err error
		out, err = c.api.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
			SecretId:  &name,
			VersionId: &versionID,
		})
		switch {
		case err == nil:
			return true, nil
		case isResourceNotFoundError(err):
			return false, nil // the new version is not readable yet
		default:
			return false, fmt.Errorf("read back %s version %s: %w", name, versionID, err)
		}
	})
	if errors.Is(err, errNotConsistent) {
		return fmt.Errorf("%s: version %s written for %s was still not readable after %s, so the write is unconfirmed; retry the upsert",
			name, versionID, serial, c.consistencyWait)
	}
	if err != nil {
		return err
	}
	if out.CreatedDate == nil {
		return fmt.Errorf("read back %s version %s returned no CreatedDate, so the write cannot be recorded as this process's last; retry the upsert",
			name, versionID)
	}
	c.setMonitorWatermark(pop, monitorVersion{id: versionID, created: *out.CreatedDate})

	written, err := parseMonitorSecret(name, out)
	if err != nil {
		return err
	}
	if !bytes.Equal(written[serial], encoded) {
		return fmt.Errorf("%s version %s does not carry the entry just written for %s; retry the upsert",
			name, versionID, serial)
	}
	return nil
}

// parseMonitorSecret decodes one version of a monitor secret into its
// entries. Bootstrap creates the secret as SecretString (the expected
// form), but the AWS API allows either SecretString or SecretBinary. A
// response with neither is refused rather than treated as empty: upserting
// into an empty map would overwrite every other host's monitor cred.
func parseMonitorSecret(name string, out *secretsmanager.GetSecretValueOutput) (map[string]json.RawMessage, error) {
	var raw []byte
	switch {
	case out.SecretString != nil:
		raw = []byte(*out.SecretString)
	case out.SecretBinary != nil:
		raw = out.SecretBinary
	default:
		return nil, fmt.Errorf("secret %s has neither SecretString nor SecretBinary set; "+
			"refusing to upsert (would overwrite siblings)", name)
	}
	entries := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return entries, nil
}

// errNotConsistent is awaitConsistent's report that Secrets Manager did not
// catch up within consistencyWait. Callers replace it with an error naming
// what they were waiting for.
var errNotConsistent = errors.New("awssecrets: Secrets Manager did not catch up")

// awaitConsistent calls attempt until it reports done or fails, sleeping
// between attempts with doubling backoff. It returns errNotConsistent once
// the next sleep would run past consistencyWait.
func (c *Client) awaitConsistent(ctx context.Context, attempt func() (done bool, err error)) error {
	deadline := time.Now().Add(c.consistencyWait)
	delay := c.consistencyBackoff
	for {
		done, err := attempt()
		if err != nil || done {
			return err
		}
		if time.Now().Add(delay).After(deadline) {
			return errNotConsistent
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("awssecrets: waiting for Secrets Manager to catch up: %w", ctx.Err())
		case <-t.C:
		}
		delay = min(2*delay, maxConsistencyBackoff)
	}
}

func (c *Client) monitorWatermark(pop string) monitorVersion {
	c.monitorMu.Lock()
	defer c.monitorMu.Unlock()
	return c.monitorWritten[pop]
}

func (c *Client) setMonitorWatermark(pop string, v monitorVersion) {
	c.monitorMu.Lock()
	defer c.monitorMu.Unlock()
	c.monitorWritten[pop] = v
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// EnsureMonitorSecret creates pop/<pop>/bmc-creds-monitor with a `_meta`-only
// stub if it does not already exist, and is a no-op when it does. This is the
// slagworks-side of the per-pop bootstrap (called when a site-config is
// created): the secret must exist before the first deploy_complete can upsert a
// host's monitor cred into it, otherwise that upsert fail-replays.
//
// The stub has a `_meta`-only shape: no
// host entries, `status` marking it empty-by-design, schema_version for future
// migrations. The bmc-creds-sync.py tooling and UpsertMonitorEntry both tolerate
// (and preserve) `_meta`, so this composes with the existing operator workflow.
//
// Requires secretsmanager:CreateSecret on pop/*/bmc-creds-monitor-* (the IRSA
// grant was widened for this). Idempotent: a concurrent create losing the race
// surfaces as ResourceExists, which we treat as success.
func (c *Client) EnsureMonitorSecret(ctx context.Context, pop string) error {
	if pop == "" {
		return errors.New("awssecrets: empty pop")
	}
	name := "pop/" + pop + "/bmc-creds-monitor"

	// Fast path: already exists -> nothing to do. DescribeSecret is cheap and
	// the common case (re-creating a site-config, or a pop bootstrapped out of
	// band) hits it.
	exists, err := c.secretExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	stub := monitorSecretStub(pop)
	desc := "slagworks per-pop BMC monitor (USER-tier) credentials"
	if _, err := c.api.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name:         &name,
		Description:  &desc,
		SecretString: &stub,
	}); err != nil {
		// Lost a create race (another writer / a just-landed Pulumi apply):
		// the secret now exists, which is exactly the desired post-state.
		if strings.Contains(err.Error(), "ResourceExistsException") {
			return nil
		}
		return fmt.Errorf("awssecrets: CreateSecret %s: %w", name, err)
	}
	return nil
}

// monitorSecretStub returns the `_meta`-only initial payload for a freshly
// created monitor secret, with the stub fields
// (schema_version 1, tier "monitor", status marking it empty-by-design). No
// host entries — those are added by UpsertMonitorEntry on deploy_complete.
func monitorSecretStub(pop string) string {
	meta := map[string]any{
		"_meta": map[string]any{
			"aws_path":       "pop/" + pop + "/bmc-creds-monitor",
			"aws_region":     "us-east-2",
			"tier":           "monitor",
			"schema_version": 1,
			"status":         "STUB - empty by design until monitor accounts are bootstrapped",
			"created_by":     "slagworksd (site-config bootstrap)",
		},
	}
	b, err := json.Marshal(meta)
	if err != nil {
		// The map is a fixed literal of JSON-safe values; marshal cannot fail.
		// Fall back to a minimal valid stub rather than panic.
		return `{"_meta":{"tier":"monitor","status":"STUB"}}`
	}
	return string(b)
}

// isResourceNotFoundError reports whether the AWS SDK error indicates
// the secret doesn't exist. SDK v2 wraps API errors in an OperationError
// whose Unwrap chain includes *types.ResourceNotFoundException; we
// check the error-string to avoid pulling the typed-error import for
// a single-call sentinel.
func isResourceNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "ResourceNotFoundException")
}

// marshalSorted JSON-encodes a map with keys in sorted order. The
// stdlib encodes Go maps in sorted key order already (since 1.12),
// but RawMessage values bypass that ordering — they're already
// pre-encoded. We re-encode each value to a fresh interface{} to
// force the same canonicalization.
func marshalSorted(m map[string]json.RawMessage) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make([]struct {
		k string
		v json.RawMessage
	}, len(keys))
	for i, k := range keys {
		ordered[i].k = k
		ordered[i].v = m[k]
	}
	// Build the object manually to guarantee key ordering.
	buf := []byte{'{'}
	for i, kv := range ordered {
		if i > 0 {
			buf = append(buf, ',')
		}
		kEncoded, err := json.Marshal(kv.k)
		if err != nil {
			return nil, err
		}
		buf = append(buf, kEncoded...)
		buf = append(buf, ':')
		buf = append(buf, kv.v...)
	}
	buf = append(buf, '}')
	// Validate the result is parseable JSON before returning.
	if !json.Valid(buf) {
		return nil, errors.New("marshalSorted produced invalid JSON")
	}
	return buf, nil
}

// FakeMonitorWriter is an in-memory MonitorWriter for tests. The
// Secrets map holds one map[serial]BMCMonitorEntry per pop; the
// upsert mutates it in place.
type FakeMonitorWriter struct {
	Secrets map[string]map[string]BMCMonitorEntry
	// LastPop/LastSerial/LastEntry capture the most recent upsert for
	// tests that want to assert what was written.
	LastPop    string
	LastSerial string
	LastEntry  BMCMonitorEntry
	// Ensured records pops for which EnsureMonitorSecret was called, so tests
	// can assert the bootstrap fired. EnsureErr, if set, is returned (models a
	// CreateSecret failure).
	Ensured   map[string]bool
	EnsureErr error
}

// EnsureMonitorSecret implements MonitorWriter. Records the pop (and creates the
// per-pop secret map so a later upsert sees an "existing" secret), or returns
// EnsureErr when set.
func (f *FakeMonitorWriter) EnsureMonitorSecret(_ context.Context, pop string) error {
	if f.EnsureErr != nil {
		return f.EnsureErr
	}
	if f.Ensured == nil {
		f.Ensured = map[string]bool{}
	}
	f.Ensured[pop] = true
	if f.Secrets == nil {
		f.Secrets = map[string]map[string]BMCMonitorEntry{}
	}
	if _, ok := f.Secrets[pop]; !ok {
		f.Secrets[pop] = map[string]BMCMonitorEntry{}
	}
	return nil
}

// GetMonitorEntry returns the serial's entry, or nil when there is none.
func (f *FakeMonitorWriter) GetMonitorEntry(_ context.Context, pop, serial string) (*BMCMonitorEntry, error) {
	if e, ok := f.Secrets[pop][serial]; ok {
		return &e, nil
	}
	return nil, nil
}

// UpsertMonitorEntry implements MonitorWriter.
func (f *FakeMonitorWriter) UpsertMonitorEntry(_ context.Context, pop, serial string, entry BMCMonitorEntry) error {
	if f.Secrets == nil {
		f.Secrets = map[string]map[string]BMCMonitorEntry{}
	}
	if _, ok := f.Secrets[pop]; !ok {
		f.Secrets[pop] = map[string]BMCMonitorEntry{}
	}
	f.Secrets[pop][serial] = entry
	f.LastPop = pop
	f.LastSerial = serial
	f.LastEntry = entry
	return nil
}
