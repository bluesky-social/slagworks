package dockermirror

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/pkg/authconfig"
	mobyclient "github.com/moby/moby/client"
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

// fakeDaemon serves the slice of the Docker API that Pull touches:
// ping, image inspect (404 unless present) and image create (pull),
// recording each pull's ref and decoded X-Registry-Auth.
type fakeDaemon struct {
	present bool

	mu    sync.Mutex
	pulls []string
	auths []string
}

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/_ping":
		w.Header().Set("Api-Version", "1.47")
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/images/"):
		if !d.present {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such image"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Id":"sha256:0"}`))
	case strings.HasSuffix(r.URL.Path, "/images/create") && r.Method == http.MethodPost:
		auth, err := authconfig.Decode(r.Header.Get("X-Registry-Auth"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.pulls = append(d.pulls, r.URL.Query().Get("fromImage")+":"+r.URL.Query().Get("tag"))
		d.auths = append(d.auths, auth.Username+"|"+auth.Password+"|"+auth.ServerAddress)
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"Pulled"}` + "\n"))
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotImplemented)
	}
}

// recorded returns the pulls served so far and their decoded auth.
func (d *fakeDaemon) recorded() (pulls, auths []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.pulls...), append([]string(nil), d.auths...)
}

func newFakeClient(t *testing.T, d *fakeDaemon) *mobyclient.Client {
	t.Helper()
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	c, err := mobyclient.New(mobyclient.WithHost("tcp://" + srv.Listener.Addr().String()))
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestPull_MissingImage_SendsAuth(t *testing.T) {
	t.Setenv("SLAGWORKS_ECR_AUTH_USERNAME", "AWS")
	t.Setenv("SLAGWORKS_ECR_AUTH_PASSWORD", "pass")
	t.Setenv("SLAGWORKS_ECR_AUTH_SERVER", "ecr.example")
	d := &fakeDaemon{}
	if err := Pull(t.Context(), newFakeClient(t, d), "ecr.example/cache/library/postgres", "18-alpine"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	pulls, auths := d.recorded()
	if len(pulls) != 1 || pulls[0] != "ecr.example/cache/library/postgres:18-alpine" {
		t.Fatalf("pulls = %v, want one of ecr.example/cache/library/postgres:18-alpine", pulls)
	}
	if auths[0] != "AWS|pass|ecr.example" {
		t.Fatalf("pull auth = %q, want the SLAGWORKS_ECR_AUTH_* creds", auths[0])
	}
}

func TestPull_MissingImage_NoCreds_Anonymous(t *testing.T) {
	t.Setenv("SLAGWORKS_ECR_AUTH_USERNAME", "")
	t.Setenv("SLAGWORKS_ECR_AUTH_PASSWORD", "")
	t.Setenv("SLAGWORKS_ECR_AUTH_SERVER", "")
	d := &fakeDaemon{}
	if err := Pull(t.Context(), newFakeClient(t, d), "postgres", "18-alpine"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	pulls, auths := d.recorded()
	if len(pulls) != 1 || auths[0] != "||" {
		t.Fatalf("pulls = %v auths = %v, want one anonymous pull", pulls, auths)
	}
}

func TestPull_PresentImage_NoPull(t *testing.T) {
	d := &fakeDaemon{present: true}
	if err := Pull(t.Context(), newFakeClient(t, d), "postgres", "18-alpine"); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if pulls, _ := d.recorded(); len(pulls) != 0 {
		t.Fatalf("pulls = %v, want none for an image the daemon has", pulls)
	}
}
