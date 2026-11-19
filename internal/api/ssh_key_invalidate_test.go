package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/labstack/echo/v5"
	cryptossh "golang.org/x/crypto/ssh"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

// TestSSHKeyMutations_InvalidateAllSites is the regression for SSH key
// changes not reaching site workers. The agent writes the deployed host's
// /root/.ssh/authorized_keys from the worker's /boot/ssh-keys, which serves
// SiteSnapshot.SSHKeys; nothing in a deploy's own lifecycle rebuilds the
// snapshot before that fetch. So adding or removing a key MUST push a fresh
// snapshot to every site. Before the fix neither handler invalidated: a key
// removed with `slagctl ssh-key remove` was still installed as root's
// authorized key on hosts deployed before the next unrelated rebuild, and a
// newly added key could be missing from them.
func TestSSHKeyMutations_InvalidateAllSites(t *testing.T) {
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)

	// Two site_configs so we can assert the fan-out hits every site.
	for _, slug := range []string{"pop-a", "pop-b"} {
		if err := st.CreateSiteConfig(&model.SiteConfig{
			Slug:               slug,
			NetBoxSite:         slug,
			EphemeralPoolStart: "10.0.0.10",
			EphemeralPoolEnd:   "10.0.0.250",
		}); err != nil {
			t.Fatalf("seed site_config %s: %v", slug, err)
		}
	}

	inv := &fakeInvalidator{}
	s := &Server{store: st, invalidator: inv, echo: echo.New()}
	s.echo.POST("/api/v1/ssh-keys", s.createSSHKey)
	s.echo.DELETE("/api/v1/ssh-keys/:id", s.deleteSSHKey)

	srv := httptest.NewServer(s.echo)
	defer srv.Close()

	// requireAllSitesSince asserts that every site was invalidated after the
	// first `from` recorded calls and returns the new high-water mark.
	requireAllSitesSince := func(what string, from int) int {
		t.Helper()
		all := inv.seen()
		got := append([]string(nil), all[from:]...)
		sort.Strings(got)
		if len(got) != 2 || got[0] != "pop-a" || got[1] != "pop-b" {
			t.Errorf("%s must invalidate every site [pop-a pop-b], got %v", what, got)
		}
		return len(all)
	}

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := cryptossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := string(bytes.TrimSpace(cryptossh.MarshalAuthorizedKey(sshPub)))

	// slagctl ssh-key add
	body, _ := json.Marshal(map[string]string{"public_key": line, "comment": "operator"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/ssh-keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST ssh-key: %v", err)
	}
	var created model.SSHKey
	decodeErr := json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST ssh-key: status %d", resp.StatusCode)
	}
	if decodeErr != nil {
		t.Fatalf("POST ssh-key: decode response: %v", decodeErr)
	}
	mark := requireAllSitesSince("adding an SSH key", 0)

	// slagctl ssh-key remove: the revocation path.
	req, _ = http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/ssh-keys/"+created.ID.String(), nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE ssh-key: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE ssh-key: status %d", resp.StatusCode)
	}
	if keys, err := st.ListSSHPublicKeys(); err != nil || len(keys) != 0 {
		t.Fatalf("after delete: ListSSHPublicKeys = %v, %v; want none", keys, err)
	}
	requireAllSitesSince("removing an SSH key", mark)
}
