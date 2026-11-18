package clientauth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/oauth2"

	"github.com/bluesky-social/slagworks/internal/cli/userpath"
)

// Token cache layout: a single JSON file holding the full oauth2.Token
// (access + refresh + expiry), 0600 in a 0700 dir. A sibling .lock file
// serializes refreshes across concurrent invocations of the CLI. The path
// itself comes from userpath (~/.config/<AppName>/token.json on all
// platforms); read/write of the 0600 file go through userpath too.

func readCache(path string) (*oauth2.Token, error) {
	b, err := userpath.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, fmt.Errorf("parsing token cache %s: %w", path, err)
	}
	return &tok, nil
}

// writeCache atomically writes the token (temp file + rename), 0600, creating
// the parent dir 0700 if needed.
func writeCache(path string, tok *oauth2.Token) error {
	// Persisting the OAuth token (incl. refresh token) to the local cache is
	// the whole point — userpath.WriteFile lands it 0600 in a 0700 dir via a
	// temp+rename. gosec flags the token fields as "secret pattern" but caching
	// them locally is intended.
	b, err := json.Marshal(tok) // #nosec G117 -- intentional local token cache, 0600
	if err != nil {
		return err
	}
	return userpath.WriteFile(path, b)
}

func deleteCache(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// best-effort: also drop the lock file
	_ = os.Remove(path + ".lock")
	return nil
}

// persistingTokenSource is an oauth2.TokenSource that refreshes under a file
// lock and writes the rotated token back to disk. This is the load-bearing
// detail: golang.org/x/oauth2's own TokenSource refreshes only in memory and
// never persists. Because providers such as Authentik ROTATE refresh tokens
// (each refresh consumes the old one and issues a new one), failing to (a)
// write the new token back, or (b) serialize concurrent refreshes,
// deterministically logs the operator out:
//
//   - No write-back: the next invocation refreshes the already-consumed
//     refresh token from disk → the provider rejects it → session dead.
//   - Concurrent refreshes: two processes both spend the same refresh token →
//     rotation invalidates one → session dead.
//
// So Token() takes an exclusive flock, READS THE TOKEN FROM DISK INSIDE THE
// LOCK (a pre-instantiated config.TokenSource would capture a stale token and
// defeat the lock), refreshes only if expired, ABORTS the write on refresh
// error (never corrupts the cache with a nil/empty token), and atomically
// writes a successfully-rotated token back before unlocking.
type persistingTokenSource struct {
	ctx       context.Context
	oc        *oauth2.Config
	cachePath string
}

func newPersistingTokenSource(ctx context.Context, oc *oauth2.Config, cachePath string) *persistingTokenSource {
	return &persistingTokenSource{ctx: ctx, oc: oc, cachePath: cachePath}
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	lock := flock.New(p.cachePath + ".lock")
	// Acquire the lock under the context: if the operator Ctrl-C's (or a
	// timeout fires) while a concurrent invocation is holding the lock through
	// a slow/hung refresh, we abort instead of blocking indefinitely. The
	// 100ms poll is the retry cadence flock uses while waiting.
	locked, err := lock.TryLockContext(p.ctx, 100*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquiring token lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("acquiring token lock: %w", p.ctx.Err())
	}
	defer func() { _ = lock.Unlock() }()

	// Read INSIDE the lock so a process that was waiting picks up whatever the
	// previous holder just wrote.
	cur, err := readCache(p.cachePath)
	if err != nil {
		return nil, fmt.Errorf("reading token cache under lock: %w", err)
	}
	if cur.Valid() {
		return cur, nil // someone else already refreshed (or it never expired)
	}
	if cur.RefreshToken == "" {
		return nil, fmt.Errorf("token expired and no refresh token; re-login required")
	}

	// Refresh from the on-disk token (NOT a pre-captured one).
	refreshed, err := p.oc.TokenSource(p.ctx, cur).Token()
	if err != nil {
		// Do NOT touch the cache on failure — a revoked/expired/unreachable
		// refresh must not overwrite the cache with a bad token.
		return nil, fmt.Errorf("refreshing token (re-login may be required): %w", err)
	}
	if err := writeCache(p.cachePath, refreshed); err != nil {
		return nil, fmt.Errorf("persisting refreshed token: %w", err)
	}
	return refreshed, nil
}
