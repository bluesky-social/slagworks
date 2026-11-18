// Package tailscale mints single-use, pre-authorized, tagged Tailscale auth
// keys for deploy-time host enrollment. It is used by slagworks global to
// produce one key per deploy operation; the key travels in the op's
// tailscale_runtime params block to the host, which runs `tailscale up` once.
//
// The minting capability lives ONLY in global (the OAuth client secret is in
// kubes1/slagworks/creds, read by global). What reaches a host is a single-use
// key that auto-expires. Workload Identity Federation (no long-lived secret at
// all) is the planned successor.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultBaseURL is the Tailscale API root. Overridable in Minter for tests.
const defaultBaseURL = "https://api.tailscale.com"

// Minter mints auth keys from an OAuth client with the auth_keys scope.
//
// The OAuth client must own the tags it mints for (in the tailnet ACL
// tagOwners). For slagworks the client is `slagworks-deploy`, owning
// tag:pop1/tag:pop2/tag:pop3; its id+secret live in kubes1/slagworks/creds.
type Minter struct {
	clientID     string
	clientSecret string
	baseURL      string
	httpClient   *http.Client
}

// NewMinter builds a Minter from an OAuth client id+secret. A nil httpClient
// uses a default with a bounded timeout. baseURL "" uses the production
// Tailscale API.
func NewMinter(clientID, clientSecret string) *Minter {
	return &Minter{
		clientID:     clientID,
		clientSecret: clientSecret,
		baseURL:      defaultBaseURL,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

// KeyRequest describes the auth key to mint.
type KeyRequest struct {
	// Tags the minted key assigns to the device (e.g. ["tag:pop2"]). The
	// OAuth client must own these in tagOwners or the mint is rejected
	// (HTTP 400 "requested tags ... invalid or not permitted").
	Tags []string
	// ExpirySeconds bounds the key's validity window. The key must still be
	// valid when the host boots and runs `tailscale up`, so this is set
	// generous-but-bounded relative to deploy duration (see plan OQ1).
	ExpirySeconds int
	// Description is a human-readable label shown in the admin console. The
	// API rejects some punctuation (parens), so keep it plain.
	Description string
}

// MintedKey is the result of a successful mint.
type MintedKey struct {
	// Key is the secret tskey-auth-... value. Single-use: Tailscale revokes
	// it automatically once a node registers with it.
	Key string
	// ID is the key's admin-console identifier (NOT secret), useful for
	// audit/revocation.
	ID string
}

// oauthToken exchanges the client credentials for a short-lived access token
// scoped to auth_keys. Tailscale's OAuth endpoint is form-urlencoded with HTTP
// Basic auth (client_id:client_secret).
func (m *Minter) oauthToken(ctx context.Context) (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {"auth_keys"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.baseURL+"/api/v2/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("tailscale: build oauth request: %w", err)
	}
	req.SetBasicAuth(m.clientID, m.clientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("tailscale: oauth token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("tailscale: oauth token: HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("tailscale: decode oauth token: %w", err)
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("tailscale: oauth token response had no access_token")
	}
	return parsed.AccessToken, nil
}

// Mint creates a single-use, pre-authorized, tagged auth key.
//
// The node registered with this key is PERSISTENT (ephemeral=false): a server
// that reboots as part of its lifecycle must not be auto-removed when it goes
// offline. The KEY is one-off (reusable=false) — Tailscale revokes it the
// moment a node registers with it, so a leaked post-use key is worthless.
// preauthorized=true skips manual device approval.
func (m *Minter) Mint(ctx context.Context, kr KeyRequest) (MintedKey, error) {
	if len(kr.Tags) == 0 {
		return MintedKey{}, fmt.Errorf("tailscale: Mint requires at least one tag")
	}
	if kr.ExpirySeconds <= 0 {
		return MintedKey{}, fmt.Errorf("tailscale: Mint requires a positive ExpirySeconds")
	}

	token, err := m.oauthToken(ctx)
	if err != nil {
		return MintedKey{}, err
	}

	reqBody := keyCreateRequest{ExpirySeconds: kr.ExpirySeconds, Description: kr.Description}
	reqBody.Capabilities.Devices.Create.Reusable = false
	reqBody.Capabilities.Devices.Create.Ephemeral = false
	reqBody.Capabilities.Devices.Create.Preauthorized = true
	reqBody.Capabilities.Devices.Create.Tags = kr.Tags

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return MintedKey{}, fmt.Errorf("tailscale: marshal key request: %w", err)
	}

	// The "-" in /tailnet/-/keys means "the tailnet this credential belongs
	// to" — no need to name it.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		m.baseURL+"/api/v2/tailnet/-/keys", bytes.NewReader(payload))
	if err != nil {
		return MintedKey{}, fmt.Errorf("tailscale: build key request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return MintedKey{}, fmt.Errorf("tailscale: create key request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return MintedKey{}, fmt.Errorf("tailscale: create key: HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var parsed struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return MintedKey{}, fmt.Errorf("tailscale: decode key response: %w", err)
	}
	if parsed.Key == "" {
		return MintedKey{}, fmt.Errorf("tailscale: key response had no key")
	}
	return MintedKey{Key: parsed.Key, ID: parsed.ID}, nil
}

// keyCreateRequest is the POST /api/v2/tailnet/-/keys body shape.
type keyCreateRequest struct {
	Capabilities struct {
		Devices struct {
			Create struct {
				Reusable      bool     `json:"reusable"`
				Ephemeral     bool     `json:"ephemeral"`
				Preauthorized bool     `json:"preauthorized"`
				Tags          []string `json:"tags"`
			} `json:"create"`
		} `json:"devices"`
	} `json:"capabilities"`
	ExpirySeconds int    `json:"expirySeconds"`
	Description   string `json:"description"`
}

// snippet bounds an error-body excerpt so a huge/HTML error page doesn't bloat
// the wrapped error. Note: a Tailscale auth-key value never appears in these
// API error bodies, so this is safe to log.
func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
