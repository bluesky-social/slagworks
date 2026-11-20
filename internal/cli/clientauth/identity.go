package clientauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"golang.org/x/oauth2"
)

// Identity is the human-readable subset of a token's claims, for `auth whoami`
// and `doctor`.
type Identity struct {
	Subject           string   `json:"sub"`
	Email             string   `json:"email"`
	PreferredUsername string   `json:"preferred_username"`
	Groups            []string `json:"groups"`
	Expiry            int64    `json:"exp"`
	// Refreshable is not a claim: CachedIdentity sets it when the cache also
	// holds a refresh token, so an expired access token is renewed on next
	// use instead of needing a new login.
	Refreshable bool `json:"-"`
}

// DecodeIdentity extracts claims from a JWT access token WITHOUT verifying its
// signature — this is for local display (`whoami`) only, never for trust
// decisions (the resource server does the real verification). Returns an error
// if the token isn't a decodable JWT.
func DecodeIdentity(accessToken string) (*Identity, error) {
	var id Identity
	if err := decodeClaims(accessToken, &id); err != nil {
		return nil, err
	}
	return &id, nil
}

// decodeClaims unmarshals a JWT's payload into v WITHOUT verifying its
// signature. Callers decode only the claims they use (DecodeIdentity for
// display, readCache for exp), so a claim one of them does not model cannot
// break the other.
func decodeClaims(accessToken string, v any) error {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return fmt.Errorf("not a JWT (cannot decode claims locally)")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// some encoders pad; tolerate standard base64url with padding
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return fmt.Errorf("decoding JWT payload: %w", err)
		}
	}
	if err := json.Unmarshal(payload, v); err != nil {
		return fmt.Errorf("parsing JWT claims: %w", err)
	}
	return nil
}

// Logout deletes the cached token for the app. Safe to call when no cache
// exists. It waits for a refresh in flight in another invocation, which would
// otherwise write the session back after the delete.
func Logout(ctx context.Context, cfg Config) error {
	path, err := defaultTokenPath(cfg.AppName)
	if err != nil {
		return err
	}
	return removeToken(ctx, path)
}

// WriteCachedToken persists a freshly-obtained token to the app's default cache
// location (used by `auth login`), recording cfg's API, issuer and client as
// what it was issued for. Bearer() handles caching on its own login path; this
// is the explicit-login entrypoint. It waits for a refresh in flight in another
// invocation, which would otherwise overwrite the new token with the refreshed
// old session.
func WriteCachedToken(ctx context.Context, cfg Config, tok *oauth2.Token) error {
	path, err := defaultTokenPath(cfg.AppName)
	if err != nil {
		return err
	}
	return storeToken(ctx, path, session{binding: cfg.binding(), Token: tok})
}

// CachedIdentity loads the cached access token and decodes its identity for
// `whoami` and `doctor`, without triggering a login or a refresh, and reports
// whether the cache holds a refresh token. Returns an error if there's no
// cache.
func CachedIdentity(cfg Config) (*Identity, error) {
	path, err := defaultTokenPath(cfg.AppName)
	if err != nil {
		return nil, err
	}
	s, err := readCache(path)
	if err != nil {
		return nil, fmt.Errorf("not logged in (no cached token): %w", err)
	}
	id, err := DecodeIdentity(s.Token.AccessToken)
	if err != nil {
		return nil, err
	}
	id.Refreshable = s.Token.RefreshToken != ""
	return id, nil
}
