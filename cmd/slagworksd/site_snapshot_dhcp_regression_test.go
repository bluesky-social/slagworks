package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/worker"
)

const dhcpChildEnv = "SLAGWORKSD_SNAPSHOT_DHCP_REGRESSION_CHILD"

// The fabric DHCP gateway and resolvers in a site snapshot are free-text
// NetBox custom fields on the site's production prefix (gateway_ip,
// dns_servers); global copies them into the snapshot without parsing them.
// A running worker survives a value it cannot parse: the snapshot-applied
// hot reload logs it and keeps the previous pools. The same value must not
// take the worker down when it next starts: the snapshot is in its LocalDB
// (and is what global hands it again), so every restart would die the same
// way and the site would have no DHCP, TFTP, HTTP boot, phone-home endpoint
// or operations until someone corrects the NetBox field.
//
// Each case seeds worker.db with such a snapshot, starts the real
// `slagworksd site` (this test binary re-executed) with global unreachable,
// and expects the process to stay up and its HTTP boot server to answer.
func TestSiteWorkerStartsOnUnparseableSnapshotDHCP(t *testing.T) {
	if os.Getenv(dhcpChildEnv) == "1" {
		os.Args = []string{"slagworksd", "site"}
		main()
		os.Exit(0)
	}

	valid := model.DHCPConfig{Subnet: "10.77.0.0/24", Gateway: "10.77.0.1", EphemeralPoolStart: "10.77.0.100", EphemeralPoolEnd: "10.77.0.150"}
	with := func(edit func(*model.DHCPConfig)) model.DHCPConfig {
		c := valid
		edit(&c)
		return c
	}
	for _, tc := range []struct {
		name string
		dhcp model.DHCPConfig
	}{
		{"gateway_ip typed with its mask", with(func(c *model.DHCPConfig) { c.Gateway = "10.77.0.1/24" })},
		{"gateway_ip cleared", with(func(c *model.DHCPConfig) { c.Gateway = "" })},
		{"dns_servers separated with semicolons", with(func(c *model.DHCPConfig) { c.DNS = []string{"10.77.0.2; 10.77.0.3"} })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, err := os.MkdirTemp("", "swdhcp")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			for _, d := range []string{"data", "tftp", "images"} {
				if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			db, err := worker.OpenLocalDB(filepath.Join(dir, "data", "worker.db"))
			if err != nil {
				t.Fatal(err)
			}
			db.SaveSnapshot(&model.SiteSnapshot{
				SeqNum:  7,
				Site:    model.SiteInfo{Name: "site-a", Slug: "site-a"},
				DHCP:    tc.dhcp,
				SSHKeys: []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5 persisted-snapshot"},
			})

			freePort := func() string {
				l, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = l.Close() }()
				return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
			}
			// Nothing listens on the port global is configured at.
			apiURL := "http://127.0.0.1:" + freePort()
			httpPort := freePort()
			iface := "lo"
			if _, err := net.InterfaceByName("lo0"); err == nil {
				iface = "lo0"
			}
			logPath := filepath.Join(dir, "worker.log")
			logf, err := os.Create(logPath)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSiteWorkerStartsOnUnparseableSnapshotDHCP$", "-test.timeout=0")
			cmd.Stdout, cmd.Stderr = logf, logf
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.Getenv("TMPDIR"),
				dhcpChildEnv + "=1",
				"SLAGWORKS_API_URL=" + apiURL,
				"SLAGWORKS_TOKEN=token",
				"SLAGWORKS_WORKER_ID=worker-a",
				"SLAGWORKS_SERVER_IP=127.0.0.1",
				"SLAGWORKS_IFACE=" + iface,
				"SLAGWORKS_HTTP_PORT=" + httpPort,
				"SLAGWORKS_HEALTH_ADDR=127.0.0.1:" + freePort(),
				"SLAGWORKS_DATA_DIR=" + filepath.Join(dir, "data"),
				"SLAGWORKS_TFTP_DIR=" + filepath.Join(dir, "tftp"),
				"SLAGWORKS_IMAGES_DIR=" + filepath.Join(dir, "images"),
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
				}
				_ = logf.Close()
			})

			// runSite waits 5s for global, then starts its services.
			deadline := time.After(20 * time.Second)
			tick := time.NewTicker(200 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-exited:
					tail, _ := os.ReadFile(logPath)
					t.Fatalf("the worker exited (%v) on a snapshot whose DHCP config it cannot parse; it must start without the fabric pool and say so\n%s", waitErr, tail)
				case <-deadline:
					tail, _ := os.ReadFile(logPath)
					t.Fatalf("the worker's HTTP boot server did not come up within 20s\n%s", tail)
				case <-tick.C:
					resp, err := (&http.Client{Timeout: time.Second}).Get("http://127.0.0.1:" + httpPort + "/boot/ssh-keys")
					if err != nil {
						continue
					}
					body, _ := io.ReadAll(resp.Body)
					_ = resp.Body.Close()
					if strings.Contains(string(body), "persisted-snapshot") {
						return // up and serving the rest of the snapshot
					}
				}
			}
		})
	}
}
