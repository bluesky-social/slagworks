package cmd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
)

func TestSiteConfigList_Empty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]model.SiteConfig{})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "site-config", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "SLUG") {
		t.Errorf("expected header row, got: %q", out)
	}
}

func TestSiteConfigList_WithData(t *testing.T) {
	cfgs := []model.SiteConfig{
		{Slug: "pop3", EphemeralPoolStart: "10.252.1.100", EphemeralPoolEnd: "10.252.1.200", CreatedAt: time.Now().Add(-5 * time.Minute)},
		{Slug: "pop4", EphemeralPoolStart: "10.252.2.100", EphemeralPoolEnd: "10.252.2.200", CreatedAt: time.Now().Add(-24 * time.Hour)},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(cfgs)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "site-config", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pop3") {
		t.Errorf("expected pop3 in output, got: %q", out)
	}
	if !strings.Contains(out, "pop4") {
		t.Errorf("expected pop4 in output, got: %q", out)
	}
	if !strings.Contains(out, "10.252.1.100") {
		t.Errorf("expected pool start in output, got: %q", out)
	}
}

func TestSiteConfigList_JSON(t *testing.T) {
	cfgs := []model.SiteConfig{{Slug: "pop3", EphemeralPoolStart: "10.252.1.100", EphemeralPoolEnd: "10.252.1.200"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(cfgs)
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "site-config", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []model.SiteConfig
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %q", err, out)
	}
	if len(got) != 1 || got[0].Slug != "pop3" {
		t.Errorf("unexpected JSON output: %v", got)
	}
}

func TestSiteConfigCreate_RequiredFlags(t *testing.T) {
	cl := testServer(t, http.NewServeMux())
	_, err := execCmd(t, cl, "site-config", "create", "--slug", "pop3")
	if err == nil {
		t.Fatal("expected error for missing pool flags")
	}
}

func TestSiteConfigCreate_Posts(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/site-configs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(model.SiteConfig{
			Slug: "pop3", EphemeralPoolStart: "10.252.1.100", EphemeralPoolEnd: "10.252.1.200",
		})
	})
	cl := testServer(t, mux)
	out, err := execCmd(t, cl, "site-config", "create",
		"--slug", "pop3",
		"--pool-start", "10.252.1.100",
		"--pool-end", "10.252.1.200")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Created site config pop3") {
		t.Errorf("expected creation message, got: %q", out)
	}
}
