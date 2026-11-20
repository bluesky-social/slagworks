package snapshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/snapshot/shapeworld"
)

// The production-shape tests can run over a recorded deployment as well as
// over generated worlds. The recording lives outside the repository:
//
//	SLAGWORKS_SHAPE_DIR         directory holding store.json (a
//	                            shapeworld.StoreData: site_configs, machines,
//	                            images and scripts exported from the API) and,
//	                            once recorded, world.json
//	SLAGWORKS_SHAPE_NETBOX_GET  a read-only command that takes one argument,
//	                            an API path with query, and prints the
//	                            response body; setting it makes
//	                            TestShapeRecord record world.json
//
// Every test that needs the recording skips when SLAGWORKS_SHAPE_DIR is
// unset. Nothing recorded is ever written into the repository.

const (
	shapeDirEnv = "SLAGWORKS_SHAPE_DIR"
	shapeGetEnv = "SLAGWORKS_SHAPE_NETBOX_GET"
)

// cmdTransport answers GET requests by running a command with the request's
// path and query as its one argument. Any other method fails: the recorder
// must not be able to write to the NetBox it reads.
type cmdTransport struct {
	argv   []string
	rawDir string // each response body is also kept here, for the shape survey

	mu    sync.Mutex
	index map[string]string // request URI -> file name under rawDir
}

func (c *cmdTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("shape recorder refuses %s %s: read-only", req.Method, req.URL.Path)
	}
	uri := req.URL.RequestURI()
	args := append(append([]string{}, c.argv[1:]...), uri)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(req.Context(), c.argv[0], args...)
	cmd.Stderr = &stderr
	body, err := cmd.Output()
	status := http.StatusOK
	if err != nil {
		status = http.StatusBadGateway
		body = []byte(fmt.Sprintf("%v: %s", err, stderr.String()))
	} else if c.rawDir != "" {
		sum := sha256.Sum256([]byte(uri))
		name := hex.EncodeToString(sum[:8]) + ".json"
		if err := os.WriteFile(filepath.Join(c.rawDir, name), body, 0o600); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.index[uri] = name
		c.mu.Unlock()
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}, nil
}

func shapeLoadStore(t testing.TB, dir string) *shapeworld.StoreData {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("read store.json: %v", err)
	}
	var sd shapeworld.StoreData
	if err := json.Unmarshal(raw, &sd); err != nil {
		t.Fatalf("decode store.json: %v", err)
	}
	return &sd
}

func shapeLoadWorld(t testing.TB, dir string) *shapeworld.World {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "world.json"))
	if err != nil {
		t.Fatalf("read world.json (record it with %s set): %v", shapeGetEnv, err)
	}
	w := shapeworld.New()
	if err := json.Unmarshal(raw, w); err != nil {
		t.Fatalf("decode world.json: %v", err)
	}
	return w
}

// TestShapeRecord runs the real dispatcher gather, through the real NetBox
// client, for every site_config in store.json, and records what NetBox
// answered as world.json. It also reports whether each config builds: the
// direct answer to "does this release build the deployment as it is now".
func TestShapeRecord(t *testing.T) {
	dir := os.Getenv(shapeDirEnv)
	get := os.Getenv(shapeGetEnv)
	if dir == "" || get == "" {
		t.Skipf("set %s and %s to record a deployment", shapeDirEnv, shapeGetEnv)
	}
	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tr := &cmdTransport{argv: strings.Fields(get), rawDir: rawDir, index: map[string]string{}}
	client := netbox.NewClient(netbox.ClientConfig{
		BaseURL:    "http://netbox.invalid",
		Token:      "unused",
		HTTPClient: &http.Client{Transport: tr, Timeout: 2 * time.Minute},
	})
	sd := shapeLoadStore(t, dir)
	w := shapeworld.New()
	rec := &shapeworld.Recorder{Inner: client, W: w}
	d := NewDispatcher(rec, &shapeworld.Store{D: sd}, shapeworld.Sink{})

	for _, cfg := range sd.Configs {
		snap, err := d.BuildSnapshot(context.Background(), cfg.Slug)
		if err != nil {
			t.Errorf("config %s does not build: %v", cfg.Slug, err)
			continue
		}
		t.Logf("config %s builds: %d machines, %d reservations, %d relay pools",
			cfg.Slug, len(snap.Machines), len(snap.Reservations), len(snap.DHCP.RelayPools))
	}

	out, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "world.json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	idx, _ := json.MarshalIndent(tr.index, "", " ")
	if err := os.WriteFile(filepath.Join(rawDir, "index.json"), idx, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded %d requests", len(tr.index))
}
