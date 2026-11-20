package clientauth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"golang.org/x/oauth2"

	"github.com/bluesky-social/slagworks/internal/cli/userpath"
)

// Token cache layout: a single JSON file holding the full oauth2.Token
// (access + refresh + expiry), 0600 in a 0700 dir. A sibling .lock file
// serializes every change to the cache across concurrent invocations of the
// CLI: a refresh (read, rotate, write back), a login's write and a logout's
// delete (see lockCache). The path itself comes from userpath
// (~/.config/<AppName>/token.json on all platforms); read/write of the 0600
// file go through userpath too.

// readCache loads the cached token. A token whose response carried no
// expires_in (RFC 6749 section 5.1 makes it optional) has a zero Expiry,
// which oauth2.Token.Valid reads as "never expires": the access token would
// be handed out after the API stopped accepting it and the refresh token
// never used. When the access token is a JWT, its exp claim (which the API
// enforces) fills that gap. Every check of a cached token's expiry reads it
// here, so a token is stored as the provider sent it; an opaque access token
// keeps the zero Expiry.
func readCache(path string) (*oauth2.Token, error) {
	b, err := userpath.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, fmt.Errorf("parsing token cache %s: %w", path, err)
	}
	if tok.Expiry.IsZero() {
		// exp is a NumericDate, which RFC 7519 lets be fractional; any value
		// present, even 0, is an expiry the API enforces.
		var claims struct {
			Exp *float64 `json:"exp"`
		}
		if decodeClaims(tok.AccessToken, &claims) == nil && claims.Exp != nil {
			tok.Expiry = time.Unix(int64(*claims.Exp), 0)
		}
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

// deleteCache removes the cached token. The lock file stays: unlinking it
// while another invocation holds or waits on it would let the next invocation
// create and lock a fresh file alongside it, ending mutual exclusion.
func deleteCache(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// lockCache takes the exclusive cross-process lock on the token cache at
// path and returns its release. A refresh holds it across the token-endpoint
// round trip and then writes the rotated token back, so anything else that
// changes the cache must hold it too: a login or logout that skipped it could
// land inside that window and then be overwritten by the refresh of the old
// session. Waiting aborts when ctx is done; the 100ms poll is the retry
// cadence flock uses while waiting.
func lockCache(ctx context.Context, path string) (unlock func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating token cache dir: %w", err)
	}
	lock := flock.New(path + ".lock")
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquiring token lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("acquiring token lock: %w", ctx.Err())
	}
	return func() { _ = lock.Unlock() }, nil
}

// storeToken replaces the cached token with a freshly obtained one (a login)
// under the cache lock, so it lands after any refresh already in flight.
func storeToken(ctx context.Context, path string, tok *oauth2.Token) error {
	unlock, err := lockCache(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	return writeCache(path, tok)
}

// removeToken deletes the cached token (a logout) under the cache lock, so a
// refresh already in flight cannot write the session back afterwards.
func removeToken(ctx context.Context, path string) error {
	unlock, err := lockCache(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	return deleteCache(path)
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
	// Acquire the lock under the context: if the operator Ctrl-C's (or a
	// timeout fires) while a concurrent invocation is holding the lock through
	// a slow/hung refresh, we abort instead of blocking indefinitely.
	unlock, err := lockCache(p.ctx, p.cachePath)
	if err != nil {
		return nil, err
	}
	defer unlock()

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
