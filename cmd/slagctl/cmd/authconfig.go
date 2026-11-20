package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bluesky-social/slagworks/internal/cli/clientauth"
	"github.com/bluesky-social/slagworks/internal/cli/userpath"
)

const (
	appName = "slagctl"
	// oidcLoopbackPort is the fixed redirect port to register with the
	// identity provider (some providers match redirect URIs strictly and
	// cannot wildcard loopback ports).
	oidcLoopbackPort = 8765
)

// savedConfig is ~/.config/slagctl/config.json: the server `slagctl auth
// login` last signed in to, and the sign-in settings that server reported.
// It exists so an operator names the API URL once and so everyday commands
// don't ask the server how to sign in on every run.
type savedConfig struct {
	API          string `json:"api"`
	OIDCIssuer   string `json:"oidc_issuer"`
	OIDCClientID string `json:"oidc_client_id"`
}

func savedConfigPath() (string, error) {
	return userpath.ConfigFile(appName, "config.json")
}

// loadSavedConfig returns the saved config, or a zero value when none has
// been written yet.
func loadSavedConfig() (savedConfig, error) {
	var c savedConfig
	path, err := savedConfigPath()
	if err != nil {
		return c, err
	}
	data, err := userpath.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parsing %s: %w\n  delete it and run `slagctl auth login --api <url>`", path, err)
	}
	return c, nil
}

func writeSavedConfig(c savedConfig) error {
	path, err := savedConfigPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return userpath.WriteFile(path, append(data, '\n'))
}

// resolveAPIURL picks the API server: --api, then SLAGWORKS_API, then the
// server saved by the last `slagctl auth login`. There is no built-in
// default.
func resolveAPIURL(flagValue string) (string, error) {
	for _, v := range []string{flagValue, os.Getenv("SLAGWORKS_API")} {
		if v != "" {
			return strings.TrimRight(v, "/"), nil
		}
	}
	saved, err := loadSavedConfig()
	if err != nil {
		return "", err
	}
	if saved.API == "" {
		return "", errors.New("no API server configured\n  run `slagctl auth login --api https://<your slagworks server>` once, or set SLAGWORKS_API")
	}
	return saved.API, nil
}

// loginSettings returns the issuer and client ID for a new login to api:
// SLAGWORKS_OIDC_ISSUER and SLAGWORKS_OIDC_CLIENT_ID when both are set (for
// development, or a server too old to report them), otherwise what the
// server's client-config endpoint reports.
func loginSettings(ctx context.Context, api string) (issuer, clientID string, err error) {
	issuer, clientID = os.Getenv("SLAGWORKS_OIDC_ISSUER"), os.Getenv("SLAGWORKS_OIDC_CLIENT_ID")
	if issuer != "" && clientID != "" {
		return issuer, clientID, nil
	}
	return fetchClientConfig(ctx, api)
}

// fetchClientConfig asks the server how to sign in to it.
func fetchClientConfig(ctx context.Context, api string) (issuer, clientID string, err error) {
	url := api + "/api/v1/client-config"
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("building request for %s: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("asking %s for sign-in settings: %w\n  check the API URL (--api, SLAGWORKS_API)", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", "", fmt.Errorf("reading %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("%s returned %s\n  the server may predate this slagctl; upgrade it, or set SLAGWORKS_OIDC_ISSUER and SLAGWORKS_OIDC_CLIENT_ID", url, resp.Status)
	}
	var cc struct {
		OIDCIssuer   string `json:"oidc_issuer"`
		OIDCClientID string `json:"oidc_client_id"`
	}
	if err := json.Unmarshal(body, &cc); err != nil {
		return "", "", fmt.Errorf("parsing %s: %w", url, err)
	}
	if cc.OIDCIssuer == "" || cc.OIDCClientID == "" {
		return "", "", fmt.Errorf("%s did not return an OIDC issuer and client ID; the server is misconfigured", url)
	}
	return cc.OIDCIssuer, cc.OIDCClientID, nil
}

// authConfig returns the sign-in configuration for commands that use the
// cached token against api. The token and the identity provider it is
// refreshed with belong to the server of the last `slagctl auth login`, so
// they are used only for that server: pointing slagctl at another API must
// never hand it this token, nor send the refresh token to an identity
// provider that other server names. Any other server needs its own login.
//
// The saved config is written after the token, so the two can disagree (an
// interactive login of an earlier command lands after a later `auth login`
// elsewhere, or the config write fails). clientauth therefore checks the
// API, issuer and client recorded with the token itself as well.
func authConfig(api string) (clientauth.Config, error) {
	saved, err := loadSavedConfig()
	if err != nil {
		return clientauth.Config{}, err
	}
	if saved.API != api || saved.OIDCIssuer == "" || saved.OIDCClientID == "" {
		return clientauth.Config{}, fmt.Errorf("not signed in to %s\n  run `slagctl auth login --api %s`", api, api)
	}
	return clientauth.Config{
		AppName:      appName,
		API:          api,
		Issuer:       saved.OIDCIssuer,
		ClientID:     saved.OIDCClientID,
		LoopbackPort: oidcLoopbackPort,
	}, nil
}

// bearerSource returns the access-token source for requests to api. Each
// call re-reads the saved server and the token cache and refreshes an
// expired access token with the refresh token (clientauth.CachedBearer);
// it never starts an interactive login. Checking the saved server on every
// call keeps authConfig's promise for the whole command, not just its
// start: once `slagctl auth login --api <other>` has run in another
// terminal, requests here fail instead of carrying the other server's
// token. The server is checked again after the cache is read, because a
// login writes its token before the saved server. A request that reads the
// cache in the instant between those two writes is refused by clientauth,
// which checks the server recorded with the token itself.
// Errors that only a new login clears match clientauth.ErrLoginRequired.
func bearerSource(api string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		cfg, err := authConfig(api)
		if err != nil {
			return "", fmt.Errorf("%w: %w", clientauth.ErrLoginRequired, err)
		}
		token, err := clientauth.CachedBearer(ctx, cfg)
		if errors.Is(err, clientauth.ErrLoginRequired) {
			return "", fmt.Errorf("%w\n  run `slagctl auth login`", err)
		}
		if err != nil {
			return "", err
		}
		if _, err := authConfig(api); err != nil {
			return "", fmt.Errorf("%w: %w", clientauth.ErrLoginRequired, err)
		}
		return token, nil
	}
}

// tokenCacheConfig is enough to find the cached token (logout, whoami)
// without knowing or contacting a server.
func tokenCacheConfig() clientauth.Config {
	return clientauth.Config{AppName: appName}
}
