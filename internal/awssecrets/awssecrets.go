// Package awssecrets is the slagworks-side reader for the per-pop OOB
// device credential secret. See docs/user/configuration.md ("Secrets in
// AWS Secrets Manager") for the on-the-wire layout. The secret is named
// pop/<pop>/bmc-creds-admin for historical reasons but holds creds for
// five credential kinds (kind="bmc", "amt", "pdu", "serial",
// "serial-server"); the name is kept to avoid an IAM rescope.
//
// Entries are keyed by DEVICE SERIAL, matching NetBox `device.serial`
// (BMC: FRU Product Serial / Dell service tag; PDU: NetBox serial).
// This reverses the original hostname-keying per the operator decision
// of 2026-05-28: hostnames drift, serials don't. Consumers resolve a
// device's serial from NetBox, then Lookup by that serial.
//
// Authentication uses the default AWS credential chain (in Kubernetes,
// the pod's IAM role); tests can inject a fake fetcher.
package awssecrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// Kind enumerates the credential kinds the secret carries.
const (
	KindBMC = "bmc"
	// KindAMT is an Intel AMT (vPro Management Engine) endpoint: the
	// device's oob_ip answers WS-Man on :16993 and redirection on :16995,
	// never IPMI/Redfish. See docs/plans/2026-10-19-amt-oob-transport.md.
	KindAMT          = "amt"
	KindPDU          = "pdu"
	KindSerial       = "serial"
	KindSerialServer = "serial-server"
)

// AdminSecret is the parsed shape of pop/<pop>/bmc-creds-admin. Entries
// are keyed by device serial. Holds all five credential kinds; callers
// filter by AdminHost.Kind when they care.
type AdminSecret struct {
	// BySerial maps device serial (== NetBox device.serial) to the
	// per-device entry. The map is the source of truth for "is this
	// device known to the cred store"; an unknown serial returns nil
	// from Lookup, which callers treat as "use empty defaults".
	BySerial map[string]AdminHost
}

// AdminHost is one entry. Kind distinguishes the five credential kinds
// (bmc, amt, pdu, serial, serial-server). The kind-agnostic device-credentials
// API passes every field through. Runtime BMC and PDU consumers separately
// require their respective kinds before using a credential. A kind=bmc entry
// is the only credential slagworks uses to drive a BMC (it is the BMC's
// slot-2 admin account); PDU entries are the sole credential source for PDU
// power operations. Serial credentials are not yet consumed by Slagworks.
type AdminHost struct {
	Kind        string         `json:"kind"`
	Username    string         `json:"username"`
	Password    string         `json:"password"`
	ExtraConfig map[string]any `json:"extra_config"`
}

// rawSecretEntry matches the secret's per-device JSON shape. We
// unmarshal into this and project into AdminHost; fields slagworks
// doesn't consume (chassis_serial, board_serial, verified*,
// last_known_*) are read by external credential-rotation tooling, not
// here, and are intentionally left out.
type rawSecretEntry struct {
	Kind        string         `json:"kind"`
	Username    string         `json:"username"`
	Password    string         `json:"password"`
	ExtraConfig map[string]any `json:"extra_config"`
}

// Reader is the consumer-facing interface. The production
// implementation talks to AWS Secrets Manager; tests use the
// in-memory FakeReader below.
type Reader interface {
	GetAdmin(ctx context.Context, pop string) (*AdminSecret, error)
	// GetMonitorEntry returns the device's entry in
	// pop/<pop>/bmc-creds-monitor (keyed by NetBox serial), or nil when the
	// secret holds none for it. A missing secret is ErrMonitorSecretMissing.
	GetMonitorEntry(ctx context.Context, pop, serial string) (*BMCMonitorEntry, error)
	// AdminSecretExists / MonitorSecretExists are presence probes for
	// preflight (DescribeSecret under the hood). (false, nil) means the
	// secret is genuinely absent; (false, err) means the check itself failed.
	AdminSecretExists(ctx context.Context, pop string) (bool, error)
	MonitorSecretExists(ctx context.Context, pop string) (bool, error)
}

// secretsAPI is the subset of the Secrets Manager API the Client calls.
// *secretsmanager.Client satisfies it; tests substitute an in-memory fake
// that models eventual consistency.
type secretsAPI interface {
	GetSecretValue(ctx context.Context, in *secretsmanager.GetSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	PutSecretValue(ctx context.Context, in *secretsmanager.PutSecretValueInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
	DescribeSecret(ctx context.Context, in *secretsmanager.DescribeSecretInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
	CreateSecret(ctx context.Context, in *secretsmanager.CreateSecretInput, optFns ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
}

// Client is the production Reader. Holds a Secrets Manager client and
// a per-pop in-memory cache with TTL. Refresh is on-demand: a call
// after the TTL expires triggers a fresh fetch synchronously, on the
// caller's goroutine. Slagworks's call rate is operator-driven (one
// per op), so adding a refresh goroutine would be overkill.
type Client struct {
	api      secretsAPI
	cacheTTL time.Duration
	mu       sync.Mutex
	cache    map[string]cacheEntry // key: pop slug

	// monitorWritten is the per-pop read-after-write watermark for
	// pop/<pop>/bmc-creds-monitor: the last version this process wrote,
	// with AWS's CreatedDate for it. UpsertMonitorEntry refuses to build on
	// an AWSCURRENT read older than it (see readMonitorSecret).
	//
	// It lives in process memory because slagworks global runs as a single
	// replica, so this process is the only slagworks writer of the secret.
	// A second replica would not see this replica's watermark and could
	// build on a stale read again; running more than one would need the
	// watermark shared between them (e.g. a Postgres row read and written
	// under the per-pop advisory lock). A restart starts with no watermark,
	// so the first write per pop after it is unguarded.
	monitorMu      sync.Mutex
	monitorWritten map[string]monitorVersion // key: pop slug

	// consistencyWait bounds each wait for Secrets Manager to serve a
	// version at least as new as this process's last write (the stale-read
	// retry, and the post-Put read-back). consistencyBackoff is the first
	// retry delay; it doubles up to maxConsistencyBackoff.
	consistencyWait    time.Duration
	consistencyBackoff time.Duration
}

type cacheEntry struct {
	secret *AdminSecret
	// readAt is when the GetSecretValue that returned secret was issued.
	// The TTL counts from it, so a slow read does not extend how long its
	// value is served, and a read issued earlier never replaces the entry.
	readAt time.Time
}

// Config bundles the optional knobs. Zero values pick sensible defaults.
type Config struct {
	// CacheTTL bounds how long a fetched secret stays in memory
	// before the next call refreshes it. Zero disables caching (every
	// call hits AWS); negative is invalid.
	CacheTTL time.Duration

	// Region defaults to "us-east-2" when empty (the region the
	// secrets were first deployed in).
	Region string
}

// NewClient builds a production Reader using the in-pod IRSA
// credential chain. Returns an error if the AWS SDK config can't be
// loaded — usually means the pod's ServiceAccount isn't IRSA-annotated.
func NewClient(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.CacheTTL < 0 {
		return nil, errors.New("awssecrets: negative CacheTTL")
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-2"
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("awssecrets: load aws config: %w", err)
	}
	return newClient(secretsmanager.NewFromConfig(awsCfg), cfg.CacheTTL), nil
}

// newClient wires a Client around a Secrets Manager API. NewClient passes
// the real SDK client; tests pass a fake.
func newClient(api secretsAPI, cacheTTL time.Duration) *Client {
	return &Client{
		api:                api,
		cacheTTL:           cacheTTL,
		cache:              map[string]cacheEntry{},
		monitorWritten:     map[string]monitorVersion{},
		consistencyWait:    defaultConsistencyWait,
		consistencyBackoff: defaultConsistencyBackoff,
	}
}

// GetAdmin fetches pop/<pop>/bmc-creds-admin and returns the parsed
// secret. Cache hits return the cached value without contacting AWS;
// cache misses (cold start, expired TTL) fetch synchronously.
//
// `pop` is a slug like "pop1" — the secret name is constructed by
// the caller convention `pop/<pop>/bmc-creds-admin`. Caller is
// responsible for validating that `pop` looks like a slug; passing a
// path-traversal-shaped string would change the secret name target.
// An IAM policy scoped to `pop/*/bmc-creds-admin-*` makes an out-of-scope
// read fail at AWS, but failing closed at the caller is cleaner.
func (c *Client) GetAdmin(ctx context.Context, pop string) (*AdminSecret, error) {
	if pop == "" {
		return nil, errors.New("awssecrets: empty pop")
	}

	c.mu.Lock()
	if entry, ok := c.cache[pop]; ok && time.Since(entry.readAt) < c.cacheTTL {
		c.mu.Unlock()
		return entry.secret, nil
	}
	c.mu.Unlock()

	// Cache miss: fetch outside the lock so concurrent misses on
	// different pops don't serialize. A burst of misses on the SAME
	// pop will issue redundant fetches; acceptable for slagworks's
	// op-rate. Their responses can arrive in any order, so the store
	// below keeps the latest-issued read's value: a read issued before
	// a rotation that returns after a later read was cached must not
	// put the pre-rotation password back.
	name := "pop/" + pop + "/bmc-creds-admin"
	readAt := time.Now()
	out, err := c.api.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: &name,
	})
	if err != nil {
		return nil, fmt.Errorf("awssecrets: GetSecretValue %s: %w", name, err)
	}
	if out.SecretString == nil {
		return nil, fmt.Errorf("awssecrets: %s has no SecretString", name)
	}

	parsed, err := parseAdminSecret([]byte(*out.SecretString))
	if err != nil {
		return nil, fmt.Errorf("awssecrets: parse %s: %w", name, err)
	}

	c.mu.Lock()
	if cur, ok := c.cache[pop]; !ok || cur.readAt.Before(readAt) {
		c.cache[pop] = cacheEntry{secret: parsed, readAt: readAt}
	}
	c.mu.Unlock()

	return parsed, nil
}

// AdminSecretExists reports whether pop/<pop>/bmc-creds-admin exists in
// Secrets Manager, using DescribeSecret (cheaper than GetSecretValue and the
// IRSA role grants it). Used by preflight to refuse a deploy/commission whose
// pop has no admin secret at all — without it slagworksd has no first-deploy
// BMC-actuation cred and PDU power ops have no creds. Distinguishes the
// genuinely-absent case (false, nil) from an access/transport error (false,
// err) so preflight can tell "pop not set up" from "AWS is down".
func (c *Client) AdminSecretExists(ctx context.Context, pop string) (bool, error) {
	if pop == "" {
		return false, errors.New("awssecrets: empty pop")
	}
	return c.secretExists(ctx, "pop/"+pop+"/bmc-creds-admin")
}

// MonitorSecretExists reports whether pop/<pop>/bmc-creds-monitor exists. Used
// by preflight: a deploy's deploy_complete handler upserts the per-host monitor
// cred into this secret, and a missing secret made that upsert fail-and-replay
// forever (the cred-drift storm). Refusing up front turns that into a clear
// pre-op error. Same (exists, err) contract as AdminSecretExists.
func (c *Client) MonitorSecretExists(ctx context.Context, pop string) (bool, error) {
	if pop == "" {
		return false, errors.New("awssecrets: empty pop")
	}
	return c.secretExists(ctx, "pop/"+pop+"/bmc-creds-monitor")
}

// secretExists is the shared DescribeSecret presence probe. Returns
// (false, nil) when the secret is genuinely absent (ResourceNotFound) and
// (false, err) for any other failure (auth, throttle, network) so callers
// don't conflate "not bootstrapped" with "couldn't check".
func (c *Client) secretExists(ctx context.Context, name string) (bool, error) {
	_, err := c.api.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{
		SecretId: &name,
	})
	if err != nil {
		if isResourceNotFoundError(err) {
			return false, nil
		}
		return false, fmt.Errorf("awssecrets: DescribeSecret %s: %w", name, err)
	}
	return true, nil
}

// Lookup returns the entry for a device serial, or nil if the secret
// has no entry for that serial. Callers treat nil as "use empty
// defaults" rather than failing the op — the cred design tolerates
// partial secret population during pop bringup.
func (s *AdminSecret) Lookup(serial string) *AdminHost {
	if s == nil || serial == "" {
		return nil
	}
	if h, ok := s.BySerial[serial]; ok {
		return &h
	}
	return nil
}

// parseAdminSecret normalizes the raw JSON into the trimmed
// AdminSecret shape. Tolerates a top-level `_meta` key (the spec
// allows it).
func parseAdminSecret(blob []byte) (*AdminSecret, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(blob, &raw); err != nil {
		return nil, err
	}
	out := &AdminSecret{BySerial: map[string]AdminHost{}}
	for key, val := range raw {
		if key == "_meta" {
			continue
		}
		var entry rawSecretEntry
		if err := json.Unmarshal(val, &entry); err != nil {
			return nil, fmt.Errorf("entry %q: %w", key, err)
		}
		out.BySerial[key] = AdminHost(entry)
	}
	return out, nil
}

// FakeReader is an in-memory Reader for tests. Construct with a
// pre-populated Secrets map; GetAdmin returns the entry for the
// requested pop, or an error if absent.
//
// Presence probes: by default a pop present in Secrets reports its admin
// secret as existing, and ALL pops report the monitor secret as existing
// (the common "fully bootstrapped" case keeps tests terse). Override either
// via the explicit *Present maps (pop -> bool) when a test needs to model a
// missing secret — e.g. MonitorPresent{"pop2": false} for the pop2 gap.
type FakeReader struct {
	Secrets        map[string]*AdminSecret
	Monitor        map[string]map[string]BMCMonitorEntry // pop -> serial -> entry
	AdminPresent   map[string]bool                       // nil => derive from Secrets
	MonitorPresent map[string]bool                       // nil => present for all pops
}

// GetAdmin satisfies Reader.
func (f *FakeReader) GetAdmin(_ context.Context, pop string) (*AdminSecret, error) {
	if s, ok := f.Secrets[pop]; ok {
		return s, nil
	}
	return nil, fmt.Errorf("awssecrets fake: no secret for pop %q", pop)
}

// AdminSecretExists satisfies Reader. Defaults to "present iff the pop has a
// Secrets entry"; AdminPresent overrides per-pop when set.
func (f *FakeReader) AdminSecretExists(_ context.Context, pop string) (bool, error) {
	if f.AdminPresent != nil {
		return f.AdminPresent[pop], nil
	}
	_, ok := f.Secrets[pop]
	return ok, nil
}

// MonitorSecretExists satisfies Reader. Defaults to present for every pop;
// MonitorPresent overrides per-pop when set (model the bootstrap gap).
func (f *FakeReader) MonitorSecretExists(_ context.Context, pop string) (bool, error) {
	if f.MonitorPresent != nil {
		return f.MonitorPresent[pop], nil
	}
	return true, nil
}

// GetMonitorEntry satisfies Reader: the Monitor entry for the serial, nil
// when there is none, or ErrMonitorSecretMissing for a pop MonitorPresent
// marks absent.
func (f *FakeReader) GetMonitorEntry(ctx context.Context, pop, serial string) (*BMCMonitorEntry, error) {
	if ok, _ := f.MonitorSecretExists(ctx, pop); !ok {
		return nil, fmt.Errorf("pop/%s/bmc-creds-monitor: %w", pop, ErrMonitorSecretMissing)
	}
	if e, ok := f.Monitor[pop][serial]; ok {
		return &e, nil
	}
	return nil, nil
}
