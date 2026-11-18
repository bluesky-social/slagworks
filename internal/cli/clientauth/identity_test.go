package clientauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// mkJWT builds an unsigned-but-structurally-valid JWT (header.payload.sig) for
// DecodeIdentity, which reads claims without verifying the signature.
func mkJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	pb, _ := json.Marshal(claims)
	payload := base64.RawURLEncoding.EncodeToString(pb)
	return hdr + "." + payload + ".sig"
}

func TestDecodeIdentity(t *testing.T) {
	tok := mkJWT(t, map[string]any{
		"sub":                "abc123",
		"email":              "operator@example.com",
		"preferred_username": "operator@example.com",
		"groups":             []string{"admins", "operators"},
		"exp":                1780038493,
	})
	id, err := DecodeIdentity(tok)
	if err != nil {
		t.Fatalf("DecodeIdentity: %v", err)
	}
	if id.Subject != "abc123" {
		t.Errorf("sub = %q", id.Subject)
	}
	if id.PreferredUsername != "operator@example.com" {
		t.Errorf("preferred_username = %q", id.PreferredUsername)
	}
	if len(id.Groups) != 2 || id.Groups[1] != "operators" {
		t.Errorf("groups = %v", id.Groups)
	}
	if id.Expiry != 1780038493 {
		t.Errorf("exp = %d", id.Expiry)
	}
}

func TestDecodeIdentity_NotAJWT(t *testing.T) {
	if _, err := DecodeIdentity("opaque-token"); err == nil {
		t.Error("expected error for non-JWT")
	}
}

func TestConfigValidate(t *testing.T) {
	good := Config{AppName: "slagctl", Issuer: "https://x/", ClientID: "cid", LoopbackPort: 8766}
	if err := good.validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
	// device flow doesn't need a loopback port
	dev := Config{AppName: "slagctl", Issuer: "https://x/", ClientID: "cid", Device: true}
	if err := dev.validate(); err != nil {
		t.Errorf("valid device config rejected: %v", err)
	}
	for _, bad := range []Config{
		{Issuer: "https://x/", ClientID: "c", LoopbackPort: 1}, // no AppName
		{AppName: "d", ClientID: "c", LoopbackPort: 1},         // no Issuer
		{AppName: "d", Issuer: "https://x/", LoopbackPort: 1},  // no ClientID
		{AppName: "d", Issuer: "https://x/", ClientID: "c"},    // loopback, no port
	} {
		if err := bad.validate(); err == nil {
			t.Errorf("expected validation error for %+v", bad)
		}
	}
}

// TestCachedTokenLifecycle drives the exported cache entrypoints the way the
// auth commands do and pins where the token lands: operators' existing
// sessions live at ~/.config/<AppName>/token.json, so moving it logs them out.
func TestCachedTokenLifecycle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	cfg := Config{AppName: "slagctl", Issuer: "https://issuer.invalid/", ClientID: "cid", LoopbackPort: 8765}
	wantPath := filepath.Join(home, ".config", "slagctl", "token.json")

	if _, err := CachedIdentity(cfg); err == nil {
		t.Fatal("CachedIdentity should fail before any token is cached")
	}

	access := mkJWT(t, map[string]any{"sub": "abc123", "preferred_username": "operator@example.com"})
	if err := WriteCachedToken(cfg, &oauth2.Token{
		AccessToken: access,
		Expiry:      time.Now().Add(time.Hour),
		TokenType:   "Bearer",
	}); err != nil {
		t.Fatalf("WriteCachedToken: %v", err)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("token not at %s: %v", wantPath, err)
	}

	id, err := CachedIdentity(cfg)
	if err != nil {
		t.Fatalf("CachedIdentity: %v", err)
	}
	if id.PreferredUsername != "operator@example.com" {
		t.Errorf("preferred_username = %q", id.PreferredUsername)
	}

	// A valid cached token needs no discovery, so the unresolvable issuer is
	// never contacted.
	got, err := CachedBearer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("CachedBearer: %v", err)
	}
	if got != access {
		t.Errorf("CachedBearer returned a different token than the cached one")
	}

	if err := Logout(cfg); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Errorf("token should be removed after Logout, stat err = %v", err)
	}
	if _, err := CachedBearer(context.Background(), cfg); err == nil {
		t.Error("CachedBearer should fail after Logout")
	}
}
