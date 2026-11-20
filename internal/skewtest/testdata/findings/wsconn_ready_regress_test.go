package worker

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestWSConn_ReportsReadyFromACachedSnapshotWhenGlobalRefuses pins what the
// packaged Type=notify unit needs from a worker that restarts while global
// is unreachable or refuses it (a global on the other side of a breaking
// release, a revoked token, an outage). The worker loads its last snapshot
// from worker.db and serves DHCP, TFTP and boot scripts from it, so it must
// tell systemd it is up. If READY=1 waits for a connection that is not
// coming, systemd's start timeout (90s by default) kills the worker and
// Restart=always starts it again, for as long as global stays away: the
// site loses its boot services for several seconds out of every start
// timeout, and `systemctl restart slagworksd-site` in the package's
// post-install script blocks for the whole timeout and then fails.
func TestWSConn_ReportsReadyFromACachedSnapshotWhenGlobalRefuses(t *testing.T) {
	// A notify socket, as systemd passes one. A short path: unix socket
	// paths are limited to about 100 bytes.
	dir, err := os.MkdirTemp("/tmp", "sdn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "notify.sock")
	pc, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen on notify socket: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	t.Setenv("NOTIFY_SOCKET", sock)

	// A global that refuses the worker, as a pre-0.6 global answers a
	// worker that sends its token in the Authorization header.
	global := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(global.Close)

	// A worker.db holding the snapshot of an earlier run.
	dbPath := filepath.Join(t.TempDir(), "worker.db")
	db, err := OpenLocalDB(dbPath)
	if err != nil {
		t.Fatalf("open worker.db: %v", err)
	}
	db.SaveSnapshot(&model.SiteSnapshot{
		SeqNum:   7,
		Site:     model.SiteInfo{Slug: "pop1"},
		DHCP:     model.DHCPConfig{Subnet: "192.0.2.0/24", Gateway: "192.0.2.1"},
		Machines: []model.MachineSnapshot{{ID: uuid.New(), Hostname: "host-1", State: model.MachineStateRunning}},
	})
	db.SetSyncCursor(7)
	cache := NewCache(db)
	if cache.MachineCount() != 1 {
		t.Fatalf("the cache loaded %d machines from worker.db, want 1", cache.MachineCount())
	}

	conn := NewWSConn(WSConnConfig{GlobalURL: global.URL, Token: "tok", WorkerID: "worker-1"}, cache, db, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = conn.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Run's first attempt is refused within milliseconds; give it 5s, the
	// time the daemon itself waits before serving from the cache.
	_ = pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 256)
	for {
		n, _, err := pc.ReadFromUnix(buf)
		if err != nil {
			t.Fatalf("no READY=1 on the notify socket within 5s of a refused connection, although the worker holds a cached snapshot it serves from: %v", err)
		}
		if strings.Contains(string(buf[:n]), "READY=1") {
			return
		}
	}
}
