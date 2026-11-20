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

// Token cache layout: a single JSON file holding one session, the full
// oauth2.Token (access + refresh + expiry) and the server, issuer and client
// it was issued for, 0600 in a 0700 dir. A sibling .lock file serializes
// every change to the cache across concurrent invocations of the CLI: a
// refresh (read, rotate, write back), a login's write and a logout's delete
// (see lockCache). The path itself comes from userpath
// (~/.config/<AppName>/token.json on all platforms); read/write of the 0600
// file go through userpath too.

// binding names what a session was issued for: the API it is the bearer for,
// and the identity provider and client that minted it and refresh it.
type binding struct {
	API      string `json:"api"`
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
}

// session is the content of the token cache. The token and its binding are
// one file replaced by one rename, so no interleaving of invocations can
// leave one server's token recorded as another's: whoever wrote the token
// wrote its binding in the same step.
type session struct {
	binding
	Token *oauth2.Token `json:"token"`
}

// readCache loads the cached session. A token whose response carried no
// expires_in (RFC 6749 section 5.1 makes it optional) has a zero Expiry,
// which oauth2.Token.Valid reads as "never expires": the access token would
// be handed out after the API stopped accepting it and the refresh token
// never used. When the access token is a JWT, its exp claim (which the API
// enforces) fills that gap. Every check of a cached token's expiry reads it
// here, so a token is stored as the provider sent it; an opaque access token
// keeps the zero Expiry.
func readCache(path string) (*session, error) {
	b, err := userpath.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parsing token cache %s: %w", path, err)
	}
	if s.Token == nil {
		return nil, fmt.Errorf("token cache %s holds no token", path)
	}
	if s.Token.Expiry.IsZero() {
		// exp is a NumericDate, which RFC 7519 lets be fractional; any value
		// present, even 0, is an expiry the API enforces.
		var claims struct {
			Exp *float64 `json:"exp"`
		}
		if decodeClaims(s.Token.AccessToken, &claims) == nil && claims.Exp != nil {
			s.Token.Expiry = time.Unix(int64(*claims.Exp), 0)
		}
	}
	return &s, nil
}

// writeCache atomically writes the session (temp file + rename), 0600,
// creating the parent dir 0700 if needed.
func writeCache(path string, s session) error {
	// Persisting the OAuth token (incl. refresh token) to the local cache is
	// the whole point — userpath.WriteFile lands it 0600 in a 0700 dir via a
	// temp+rename. gosec flags the token fields as "secret pattern" but caching
	// them locally is intended.
	b, err := json.Marshal(s) // #nosec G117 -- intentional local token cache, 0600
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

// storeToken replaces the cached session with a freshly obtained one (a
// login) under the cache lock, so it lands after any refresh already in
// flight.
func storeToken(ctx context.Context, path string, s session) error {
	unlock, err := lockCache(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	return writeCache(path, s)
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
//
// What it reads under the lock may be a session another invocation's login
// just wrote for a different server. It hands out, and refreshes, only a
// session bound to want, so that login's token never reaches this server and
// its refresh token never reaches this server's identity provider.
type persistingTokenSource struct {
	ctx       context.Context
	oc        *oauth2.Config
	want      binding
	cachePath string
}

func newPersistingTokenSource(ctx context.Context, oc *oauth2.Config, want binding, cachePath string) *persistingTokenSource {
	return &persistingTokenSource{ctx: ctx, oc: oc, want: want, cachePath: cachePath}
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	// Acquire the lock under the context, so Ctrl-C (or a caller's deadline)
	// aborts the wait. A concurrent invocation holding the lock through a
	// slow refresh lets go within providerTimeout, when its refresh gives up.
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
	if cur.binding != p.want {
		return nil, notSignedIn(p.want)
	}
	if cur.Token.Valid() {
		return cur.Token, nil // someone else already refreshed (or it never expired)
	}
	if cur.Token.RefreshToken == "" {
		return nil, fmt.Errorf("token expired and no refresh token; re-login required")
	}

	// Refresh from the on-disk token (NOT a pre-captured one).
	refreshed, err := askProvider(p.ctx, p.want.Issuer, func(ctx context.Context) (*oauth2.Token, error) {
		return p.oc.TokenSource(ctx, cur.Token).Token()
	})
	if err != nil {
		// Do NOT touch the cache on failure — a revoked/expired/unreachable
		// refresh must not overwrite the cache with a bad token.
		if isNoAnswer(err) {
			return nil, err
		}
		return nil, fmt.Errorf("refreshing token (re-login may be required): %w", err)
	}
	if err := writeCache(p.cachePath, session{binding: p.want, Token: refreshed}); err != nil {
		return nil, fmt.Errorf("persisting refreshed token: %w", err)
	}
	return refreshed, nil
}
