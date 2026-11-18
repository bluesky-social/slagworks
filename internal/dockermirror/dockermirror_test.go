package dockermirror

import (
	"testing"
)

func TestImage(t *testing.T) {
	cases := []struct {
		name, mirror, path, want string
	}{
		{"mirror, official image", "ecr.example/cache", "library/postgres", "ecr.example/cache/library/postgres"},
		{"mirror, community image", "ecr.example/cache", "netboxcommunity/netbox", "ecr.example/cache/netboxcommunity/netbox"},
		{"no mirror strips library/", "", "library/postgres", "postgres"},
		{"no mirror keeps community path", "", "netboxcommunity/netbox", "netboxcommunity/netbox"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envMirror, tc.mirror)
			if got := Image(tc.path); got != tc.want {
				t.Fatalf("Image(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestAuth_AllUnset_Empty(t *testing.T) {
	t.Setenv("SLAGWORKS_ECR_AUTH_USERNAME", "")
	t.Setenv("SLAGWORKS_ECR_AUTH_PASSWORD", "")
	t.Setenv("SLAGWORKS_ECR_AUTH_SERVER", "")
	a := Auth()
	if a.Username != "" || a.Password != "" || a.ServerAddress != "" {
		t.Errorf("expected empty AuthConfiguration, got %+v", a)
	}
}

func TestAuth_AllSet_Populated(t *testing.T) {
	t.Setenv("SLAGWORKS_ECR_AUTH_USERNAME", "AWS")
	t.Setenv("SLAGWORKS_ECR_AUTH_PASSWORD", "pass")
	t.Setenv("SLAGWORKS_ECR_AUTH_SERVER", "ecr.example")
	a := Auth()
	if a.Username != "AWS" || a.Password != "pass" || a.ServerAddress != "ecr.example" {
		t.Errorf("got %+v", a)
	}
}

func TestAuth_PartialUnset_Empty(t *testing.T) {
	// Missing password.
	t.Setenv("SLAGWORKS_ECR_AUTH_USERNAME", "AWS")
	t.Setenv("SLAGWORKS_ECR_AUTH_PASSWORD", "")
	t.Setenv("SLAGWORKS_ECR_AUTH_SERVER", "ecr.example")
	a := Auth()
	if a.Username != "" {
		t.Errorf("expected all-or-nothing — partial config returns empty, got %+v", a)
	}
}
