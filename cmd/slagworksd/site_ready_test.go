package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/worker"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// readyChildEnv marks a re-executed test binary as the `slagworksd site`
// child of TestSiteWorkerReportsReadyWithoutGlobal.
const readyChildEnv = "SLAGWORKSD_READY_TEST_CHILD"

// readyAuth authenticates the token "ready-token" as worker-a of site-a.
type readyAuth struct{}

func (readyAuth) GetSiteWorkerByTokenHash(h string) (*model.SiteWorker, error) {
	sum := sha256.Sum256([]byte("ready-token"))
	if h != hex.EncodeToString(sum[:]) {
		return nil, errors.New("unknown token")
	}
	return &model.SiteWorker{ID: uuid.New(), SiteSlug: "site-a", WorkerID: "worker-a"}, nil
}

func (readyAuth) TouchSiteWorker(slug, workerID, fqdn string, _ *string) (*model.SiteWorker, error) {
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID, FQDN: fqdn}, nil
}

func (readyAuth) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

// readyNoSnapshot is a snapshot rebuilder that cannot build the site, so the
// hub answers the worker's hello with rebuild_failed.
type readyNoSnapshot struct{}

func (readyNoSnapshot) RebuildAndWait(context.Context, string) (model.SiteSnapshot, error) {
	return model.SiteSnapshot{}, errors.New("site site-a: no slagworks-production prefix")
}

// A site worker serves its boot plane from the snapshot in its LocalDB
// whatever global is doing (docs/user/installation.md, "State"). Under the
// packaged unit (Type=notify, 90s start timeout, Restart=always) it must
// therefore also report READY=1 without global; otherwise systemd kills it
// at the start timeout and restarts it every ~95s for as long as global does
// not accept its hello.
//
// Each case starts the real `slagworksd site` (this test binary re-executed)
// with a NOTIFY_SOCKET, a worker.db holding a snapshot, and a global that
// never accepts the hello, and expects READY=1 well inside systemd's budget.
// An active worker must also be serving the persisted snapshot over HTTP; a
// standby worker without its VIP starts no services but is ready to take
// over.
func TestSiteWorkerReportsReadyWithoutGlobal(t *testing.T) {
	if os.Getenv(readyChildEnv) == "1" {
		os.Args = []string{"slagworksd", "site"}
		main()
		os.Exit(0)
	}

	// Global is up and authenticates the worker, but cannot build the
	// site's snapshot: the real hub answers the hello with rebuild_failed.
	noSnapshot := httptest.NewServer(ws.NewHub(ws.HubConfig{
		Auth: readyAuth{}, Rebuilder: readyNoSnapshot{},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(noSnapshot.Close)
	// Global refuses the worker's token.
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(refused.Close)
	// Nothing listens here.
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := "http://" + dead.Addr().String()
	_ = dead.Close()

	for _, tc := range []struct {
		name    string
		apiURL  string
		standby bool
	}{
		{name: "global cannot build the site snapshot", apiURL: noSnapshot.URL},
		{name: "global refuses the worker", apiURL: refused.URL},
		{name: "global unreachable", apiURL: unreachable},
		{name: "standby without its VIP, global unreachable", apiURL: unreachable, standby: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Short path: a unix socket address is limited to ~100 bytes,
			// which t.TempDir() can exceed.
			dir, err := os.MkdirTemp("", "swready")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			for _, d := range []string{"data", "tftp", "images"} {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			// The snapshot a previous run of the worker persisted.
			db, err := worker.OpenLocalDB(filepath.Join(dir, "data", "worker.db"))
			if err != nil {
				t.Fatal(err)
			}
			db.SaveSnapshot(&model.SiteSnapshot{
				SeqNum:  7,
				Site:    model.SiteInfo{Name: "site-a", Slug: "site-a"},
				DHCP:    model.DHCPConfig{Subnet: "10.77.0.0/24", Gateway: "10.77.0.1", EphemeralPoolStart: "10.77.0.100", EphemeralPoolEnd: "10.77.0.150"},
				SSHKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 persisted-snapshot"},
			})

			sock := filepath.Join(dir, "notify")
			notify, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = notify.Close() })

			freePort := func() string {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = l.Close() }()
				return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
			}
			httpPort := freePort()
			logPath := filepath.Join(dir, "worker.log")
			logf, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSiteWorkerReportsReadyWithoutGlobal$", "-test.timeout=0")
			cmd.Stdout, cmd.Stderr = logf, logf
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.Getenv("TMPDIR"),
				readyChildEnv + "=1",
				"NOTIFY_SOCKET=" + sock,
				"SLAGWORKS_API_URL=" + tc.apiURL,
				"SLAGWORKS_TOKEN=ready-token",
				"SLAGWORKS_WORKER_ID=worker-a",
				"SLAGWORKS_SERVER_IP=127.0.0.1",
				"SLAGWORKS_IFACE=" + loopbackIface(),
				"SLAGWORKS_HTTP_PORT=" + httpPort,
				"SLAGWORKS_HEALTH_ADDR=127.0.0.1:" + freePort(),
				"SLAGWORKS_DATA_DIR=" + filepath.Join(dir, "data"),
				"SLAGWORKS_TFTP_DIR=" + filepath.Join(dir, "tftp"),
				"SLAGWORKS_IMAGES_DIR=" + filepath.Join(dir, "images"),
			}
			if tc.standby {
				// TEST-NET-1: never assigned to this host, so the standby
				// worker waits for the VIP and starts no services.
				cmd.Env = append(cmd.Env, "SLAGWORKS_STANDBY=true", "SLAGWORKS_VIP=192.0.2.10")
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var waitErr error
			exited := make(chan struct{}) // closed once the worker has exited
			go func() { waitErr = cmd.Wait(); close(exited) }()
			t.Cleanup(func() {
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-exited:
				case <-time.After(15 * time.Second):
					_ = cmd.Process.Kill()
					<-exited
				}
				_ = logf.Close()
			})
			workerLog := func() string {
				b, _ := os.ReadFile(logPath)
				return string(b)
			}

			ready := make(chan struct{})
			go func() {
				buf := make([]byte, 256)
				for {
					n, _, err := notify.ReadFromUnix(buf)
					if err != nil {
						return
					}
					if strings.Contains(string(buf[:n]), "READY=1") {
						close(ready)
						return
					}
				}
			}()

			// systemd allows 90s. runSite waits at most 5s for global before
			// starting its services, so 20s is generous.
			const budget = 20 * time.Second
			deadline := time.Now().Add(budget)

			if !tc.standby {
				// The active worker serves boot traffic from the persisted
				// snapshot.
				serving := false
				client := &http.Client{Timeout: time.Second}
				for time.Now().Before(deadline) && !serving {
					resp, err := client.Get("http://127.0.0.1:" + httpPort + "/boot/ssh-keys")
					if err == nil {
						body, _ := io.ReadAll(resp.Body)
						_ = resp.Body.Close()
						serving = strings.Contains(string(body), "persisted-snapshot")
					}
					if !serving {
						time.Sleep(200 * time.Millisecond)
					}
				}
				if !serving {
					t.Fatalf("the worker's HTTP boot server did not serve the persisted snapshot within %v\n%s", budget, workerLog())
				}
			}

			select {
			case <-ready:
			case <-exited:
				t.Fatalf("the worker exited (%v) instead of running without global\n%s", waitErr, workerLog())
			case <-time.After(time.Until(deadline)):
				t.Fatalf("no READY=1 within %v of starting while global does not accept the worker's hello: "+
					"under the packaged Type=notify unit systemd kills it at the 90s start timeout and restarts it\n%s",
					budget, workerLog())
			}
		})
	}
}
