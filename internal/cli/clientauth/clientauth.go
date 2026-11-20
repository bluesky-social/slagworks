// Package clientauth implements slagctl's OIDC operator login.
//
//   - Public client (no secret); loopback authorization-code + PKCE by default,
//     device-authorization grant (RFC 8628) when Config.Device is set.
//   - Tokens stored at ~/.config/<AppName>/token.json (0600, via the userpath
//     package — unix-style on all platforms), refreshed silently via a
//     persisting, file-locked TokenSource so concurrent invocations don't race
//     the refresh-token rotation and lock the operator out. Login and logout
//     change the cache under the same lock, so a refresh in flight elsewhere
//     cannot undo them.
//   - The cache records the API, issuer and client each token was issued for,
//     and a token is handed out, or refreshed, only for a Config naming all
//     three.
//   - The access token is the API bearer; the slagworks API validates it
//     offline against the issuer's signing keys. This package never verifies
//     it.
//
// The package is parameterized entirely through Config — it bakes in no
// specific issuer, client ID, or group.
package clientauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"golang.org/x/oauth2"

	"github.com/bluesky-social/slagworks/internal/cli/userpath"
)

// DefaultScopes are requested in both flows. `profile` carries the groups
// claim on providers such as Authentik; `offline_access` is REQUIRED for a
// refresh token to be issued — without it the provider may silently drop it
// and there's no silent refresh, kicking the operator back to a full login
// every access-token lifetime.
var DefaultScopes = []string{"openid", "email", "profile", "offline_access"}

// Config fully parameterizes the auth flows. There are no package-level
// provider defaults: the caller passes its own values (typically sourced
// from its own env-var conventions). The zero value is unusable.
type Config struct {
	// AppName names the tool. Used for the config directory
	// (~/.config/<AppName>/token.json) and user-facing prompt strings.
	AppName string
	// API is the URL of the server the access token is the bearer for. A
	// cached token is used only for the API, Issuer and ClientID it was
	// issued for.
	API string
	// Issuer is the OIDC issuer URL.
	Issuer string
	// ClientID is the public client ID (not a secret).
	ClientID string
	// LoopbackPort is the fixed registered redirect port. Providers with
	// strict redirect matching (Authentik among them) can't match a wildcard
	// loopback port, so a specific port is registered on the provider and
	// bound here.
	LoopbackPort int
	// Scopes overrides DefaultScopes when non-empty.
	Scopes []string
	// Device selects the device-authorization grant instead of loopback.
	Device bool
	// Stderr is where interactive prompts go (the auth URL, etc.). Prompts
	// never go to stdout, so command output (often piped/JSON) stays clean.
	// Defaults to os.Stderr when nil.
	Stderr io.Writer
}

func (c Config) scopes() []string {
	if len(c.Scopes) > 0 {
		return c.Scopes
	}
	return DefaultScopes
}

func (c Config) stderr() io.Writer {
	if c.Stderr != nil {
		return c.Stderr
	}
	return os.Stderr
}

func (c Config) binding() binding {
	return binding{API: c.API, Issuer: c.Issuer, ClientID: c.ClientID}
}

// errOtherServer marks a cached session issued for a different API, issuer
// or client than the Config names.
var errOtherServer = errors.New("the cached session is for another server or identity provider")

// notSignedIn also matches ErrLoginRequired: only a login to this server
// clears it, so a command that retries token fetches stops on it.
func notSignedIn(want binding) error {
	return fmt.Errorf("not signed in to %s: %w (%w)", want.API, errOtherServer, ErrLoginRequired)
}

func (c Config) validate() error {
	if c.AppName == "" {
		return fmt.Errorf("clientauth: Config.AppName is required")
	}
	if c.API == "" {
		return fmt.Errorf("clientauth: Config.API is required")
	}
	if c.Issuer == "" {
		return fmt.Errorf("clientauth: Config.Issuer is required")
	}
	if c.ClientID == "" {
		return fmt.Errorf("clientauth: Config.ClientID is required")
	}
	if !c.Device && c.LoopbackPort == 0 {
		return fmt.Errorf("clientauth: Config.LoopbackPort is required for the loopback flow")
	}
	return nil
}

// Bearer returns a valid access token for the API client, performing a silent
// refresh if the cached token is expired, or a full interactive login if there
// is no usable cached token. An identity provider that does not answer is an
// error, not a reason to log in. This is the entrypoint a CLI's root command
// calls before every server-touching subcommand.
//
// A cached session for another server is also an error, not a reason to log
// in: the operator signed in there last, and replacing that session is a
// choice they make with an explicit login.
func Bearer(ctx context.Context, cfg Config) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}
	cache, err := defaultTokenPath(cfg.AppName)
	if err != nil {
		return "", err
	}
	tok, err := loadFromCacheOrRefresh(ctx, cfg, cache)
	if err == nil && tok.Valid() {
		return tok.AccessToken, nil
	}
	// A provider that did not answer the refresh will not answer a login
	// either; report it rather than open a browser against it.
	if isNoAnswer(err) || errors.Is(err, errOtherServer) {
		return "", err
	}
	// No usable cached/refreshed token — full interactive login.
	tok, err = Login(ctx, cfg)
	if err != nil {
		return "", err
	}
	if err := storeToken(ctx, cache, session{binding: cfg.binding(), Token: tok}); err != nil {
		return "", fmt.Errorf("caching token: %w", err)
	}
	return tok.AccessToken, nil
}

// ErrLoginRequired matches (errors.Is) a CachedBearer error that only a new
// interactive login clears: nothing is cached, the cached session is for
// another server, the access token expired with no refresh token, or the
// identity provider refused the refresh token.
// Any other error, such as the provider being unreachable or answering 5xx,
// may clear on a retry. A command that retries token fetches stops on this
// one: each retry would ask the provider to refresh a token it already
// refused.
var ErrLoginRequired = errors.New("sign-in required")

// CachedBearer is Bearer without the interactive fallback: it returns a
// valid access token from the cache (silently refreshing if expired and a
// refresh token is present) or an error. It never opens a browser or starts
// a device flow, so it is safe for headless/agent contexts — callers surface
// the error as "run <cli> auth login". It reads the cache on every call, so
// a long-running command can call it before each request to get a token
// that is still valid.
func CachedBearer(ctx context.Context, cfg Config) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}
	cache, err := defaultTokenPath(cfg.AppName)
	if err != nil {
		return "", err
	}
	tok, err := loadFromCacheOrRefresh(ctx, cfg, cache)
	if err != nil {
		return "", err
	}
	if !tok.Valid() {
		return "", fmt.Errorf("cached token unusable (expired and refresh failed)")
	}
	return tok.AccessToken, nil
}

// RefreshRejected returns an access token to resend a request with after the
// API refused rejected with 401. The API can refuse a token whose expiry says
// it is still valid: its clock runs ahead of the identity provider's, or the
// provider rotated its signing keys. Under the cache lock it returns the
// cached token if another invocation has already replaced rejected with a
// valid one, and otherwise spends the refresh token whatever the expiry and
// writes the rotated token back. Like CachedBearer it never starts an
// interactive login: it runs in the middle of a command.
func RefreshRejected(ctx context.Context, cfg Config, rejected string) (string, error) {
	if err := cfg.validate(); err != nil {
		return "", err
	}
	cache, err := defaultTokenPath(cfg.AppName)
	if err != nil {
		return "", err
	}
	oc, err := oauthConfig(ctx, cfg)
	if err != nil {
		return "", err
	}
	ts := newPersistingTokenSource(ctx, oc, cfg.binding(), cache)
	ts.rejected = rejected
	tok, err := ts.Token()
	if err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

// loadFromCacheOrRefresh reads the cache and, if the access token is expired
// but a refresh token is present, refreshes it (under lock, with write-back).
// Returns an error if there is no usable token: errOtherServer when the cache
// holds a session for another server, otherwise one after which the caller
// may do a full login.
func loadFromCacheOrRefresh(ctx context.Context, cfg Config, cachePath string) (*oauth2.Token, error) {
	cached, err := readCache(cachePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: no cached token", ErrLoginRequired)
	}
	if err != nil {
		return nil, err // unreadable — caller logs in
	}
	if cached.binding != cfg.binding() {
		return nil, notSignedIn(cfg.binding())
	}
	if cached.Token.Valid() {
		return cached.Token, nil
	}
	if cached.Token.RefreshToken == "" {
		return nil, fmt.Errorf("%w: the cached token expired and has no refresh token", ErrLoginRequired)
	}
	// Expired access token + refresh token present: refresh under lock. The
	// token source checks the binding again under the lock, since a login
	// elsewhere may replace the session before then.
	oc, err := oauthConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ts := newPersistingTokenSource(ctx, oc, cfg.binding(), cachePath)
	return ts.Token()
}

// defaultTokenPath is ~/.config/<AppName>/token.json on all platforms (via
// userpath, which honors $XDG_CONFIG_HOME and never emits ~/Library). Named
// "token" not "cache": it holds the OIDC session (access + refresh token),
// which is durable operator config, not regenerable cache.
func defaultTokenPath(appName string) (string, error) {
	return userpath.ConfigFile(appName, "token.json")
}
