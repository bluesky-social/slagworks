package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// Missing required site env vars must classify as configError so main
// exits EX_CONFIG (78) and systemd (RestartPreventExitStatus=78) marks
// the unit failed-permanent instead of restart-looping. Regression for
// the oob-gw-1 slagworksd-global loop (195k restarts on missing env).
func TestLoadSiteConfig_MissingEnvIsConfigError(t *testing.T) {
	for _, k := range []string{
		"SLAGWORKS_API_URL", "SLAGWORKS_TOKEN", "SLAGWORKS_SERVER_IP", "SLAGWORKS_IFACE",
	} {
		t.Setenv(k, "")
	}

	_, err := loadSiteConfig()
	if err == nil {
		t.Fatal("expected error with no site env set")
	}
	var ce configError
	if !errors.As(err, &ce) {
		t.Fatalf("expected configError, got %T: %v", err, err)
	}
}

func TestLoadSiteConfig_BadPortIsConfigError(t *testing.T) {
	t.Setenv("SLAGWORKS_API_URL", "http://example.invalid")
	t.Setenv("SLAGWORKS_TOKEN", "tok")
	t.Setenv("SLAGWORKS_SERVER_IP", "192.0.2.1")
	t.Setenv("SLAGWORKS_IFACE", "eth0")
	t.Setenv("SLAGWORKS_HTTP_PORT", "not-a-port")

	_, err := loadSiteConfig()
	var ce configError
	if !errors.As(err, &ce) {
		t.Fatalf("expected configError for bad port, got %T: %v", err, err)
	}
}

func TestBuildGlobal_MissingOIDCIsConfigError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, err := buildGlobal(context.Background(), log, globalConfig{})
	if err == nil {
		t.Fatal("expected error with empty OIDC config")
	}
	var ce configError
	if !errors.As(err, &ce) {
		t.Fatalf("expected configError, got %T: %v", err, err)
	}
}

// An invalid hostname template refuses startup before any database work,
// naming the flag and env var to fix.
func TestBuildGlobal_BadHostnameTemplateIsConfigError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tmpl := range []string{"{host}.example.net", "host.example.net", "{node}.internal.{zone}.example_net"} {
		_, err := buildGlobal(context.Background(), log, globalConfig{
			// Unreachable: the template check must fail before any connect.
			DSN:              "host=nonexistent.invalid port=1 user=x password=x dbname=x sslmode=disable connect_timeout=1",
			OIDCIssuer:       "https://idp.example/application/o/slagworks/",
			OIDCAudience:     "test-client-id",
			OIDCGroups:       []string{"operators"},
			HostnameTemplate: tmpl,
		})
		if err == nil {
			t.Fatalf("template %q: expected an error", tmpl)
		}
		var ce configError
		if !errors.As(err, &ce) {
			t.Fatalf("template %q: expected configError, got %T: %v", tmpl, err, err)
		}
		for _, want := range []string{"--hostname-template", "SLAGWORKS_HOSTNAME_TEMPLATE", tmpl} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("template %q: error %q does not mention %q", tmpl, err, want)
			}
		}
	}
}

func TestExitCodeForErr(t *testing.T) {
	if got := exitCodeFor(errors.New("transient")); got != 1 {
		t.Fatalf("transient error: want 1, got %d", got)
	}
	wrapped := configErrorf("missing thing")
	if got := exitCodeFor(wrapped); got != exitConfig {
		t.Fatalf("config error: want %d, got %d", exitConfig, got)
	}
}
