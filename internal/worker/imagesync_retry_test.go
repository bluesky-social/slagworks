package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/slagworks/internal/imagepull"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/ws"
)

// These tests run a real WSConn, Cache and LocalDB against a real ws.Hub
// whose only snapshot for the site pins one boot image, served by a local
// bucket. They pin what a connected site worker does when that image's
// download fails: nothing else pushes it another snapshot (an equal-seq
// snapshot is dropped, a reconnect gets no_change, re-pinning the same row
// leaves the snapshot unchanged), so the worker itself must retry.

// imageSyncGlobal is a minimal global serving one fixed snapshot for site-a.
type imageSyncGlobal struct{ snap model.SiteSnapshot }

func (g imageSyncGlobal) GetSiteWorkerByTokenHash(hash string) (*model.SiteWorker, error) {
	if hash != store.HashWorkerToken("tok-1") {
		return nil, store.ErrSiteWorkerNotFound
	}
	return &model.SiteWorker{SiteSlug: "site-a", WorkerID: "worker-1"}, nil
}

func (g imageSyncGlobal) TouchSiteWorker(slug, workerID, _ string, _ *string) (*model.SiteWorker, error) {
	return &model.SiteWorker{SiteSlug: slug, WorkerID: workerID}, nil
}

func (g imageSyncGlobal) ListSiteWorkers(string) ([]model.SiteWorker, error) { return nil, nil }

func (g imageSyncGlobal) RebuildAndWait(context.Context, string) (model.SiteSnapshot, error) {
	return g.snap, nil
}

// imageSyncTarball is a minimal noble-pxe image tarball and its sha256.
func imageSyncTarball() ([]byte, string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "noble-pxe/", Typeflag: tar.TypeDir, Mode: 0o755})
	for _, f := range []string{"vmlinuz", "initrd", "initrd-local", "root.squashfs"} {
		_ = tw.WriteHeader(&tar.Header{Name: "noble-pxe/" + f, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(f))})
		_, _ = tw.Write([]byte(f))
	}
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

// imageBucket serves body for every request after the first failFirst,
// which get a 503, and counts the requests.
func imageBucket(t *testing.T, body []byte, failFirst int64) (url string, requests *atomic.Int64) {
	t.Helper()
	requests = &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= failFirst {
			http.Error(w, "SlowDown", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/noble-pxe/x.tar.gz", requests
}

// startImageSyncWorker runs a worker whose site snapshot (seq 7) pins
// noble-pxe at url with digest, and returns once the worker has synced with
// global; the image download runs in the background. imagesDir is the
// worker's images dir.
func startImageSyncWorker(t *testing.T, url, digest string) (wsc *WSConn, cache *Cache, imagesDir string) {
	t.Helper()
	g := imageSyncGlobal{snap: model.SiteSnapshot{SeqNum: 7, Site: model.SiteInfo{Slug: "site-a"}, Images: []model.Image{{
		Name: "noble-pxe", Version: "24.04", SHA256: digest, URL: url,
	}}}}
	hub := ws.NewHub(ws.HubConfig{Auth: g, Rebuilder: g})
	srv := httptest.NewServer(hub)
	t.Cleanup(func() { hub.Close(); srv.Close() })
	localDB := openTestDB(t)
	imagesDir = t.TempDir()

	cache = NewCache(localDB)
	cache.ImagesDir = imagesDir
	fastImageRetry(cache)
	wsc = NewWSConn(WSConnConfig{GlobalURL: srv.URL, Token: "tok-1", WorkerID: "worker-1"}, cache, localDB, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = wsc.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done; waitImageSyncIdle(t, cache) })
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	defer scancel()
	if err := wsc.WaitForSync(sctx); err != nil {
		t.Fatalf("worker did not sync: %v", err)
	}
	return wsc, cache, imagesDir
}

// fastImageRetry shortens c's image sync retry backoff (5s doubling to 5
// minutes in production) so a retry happens within milliseconds.
func fastImageRetry(c *Cache) {
	c.imageRetryBase = 10 * time.Millisecond
	c.imageRetryMax = 40 * time.Millisecond
}

// installedVersion is the version directory noble-pxe links to in
// imagesDir, "" when the image is not installed.
func installedVersion(imagesDir string) string {
	v, _ := imagepull.Installed(imagesDir, "noble-pxe")
	return v
}

// versionOf is the version directory of noble-pxe pinned at digest.
func versionOf(digest string) string { return "noble-pxe@" + digest[:16] }

// waitImageSyncIdle waits for c's image syncer goroutine to exit and stops
// a pending retry, so nothing writes into the test's images dir while it is
// removed.
func waitImageSyncIdle(t *testing.T, c *Cache) {
	t.Helper()
	if !within(10*time.Second, func() bool {
		c.imageSyncMu.Lock()
		defer c.imageSyncMu.Unlock()
		if c.imageRetryTimer != nil {
			c.imageRetryTimer.Stop()
			c.imageRetryTimer = nil
		}
		return !c.imageSyncRunning
	}) {
		t.Error("image syncer still running after 10s")
	}
}

// The snapshot pinning a new image arrives and the first download gets an
// object-storage 503. The worker stays connected and nothing else about the
// site changes. It must converge on the pinned image by itself.
func TestImageSync_TransientFailureIsRetried(t *testing.T) {
	body, digest := imageSyncTarball()
	url, requests := imageBucket(t, body, 1)
	wsc, cache, imagesDir := startImageSyncWorker(t, url, digest)
	if !within(10*time.Second, func() bool { return requests.Load() > 0 }) {
		t.Fatal("setup: the worker never requested the image")
	}

	disconnects := 0
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if !wsc.Connected() {
			disconnects++
		}
		if installedVersion(imagesDir) == versionOf(digest) {
			return
		}
	}
	t.Fatalf("after one 503 the pinned image did not sync within 10s while the worker stayed connected "+
		"(%d download requests, %d polls saw the worker disconnected, worker holds seq %d, installed %q); "+
		"the worker keeps serving whatever image it had before",
		requests.Load(), disconnects, cache.LastSeqNum(), installedVersion(imagesDir))
}

// Control: the same fixture with a bucket that never fails converges on the
// first apply's sync, so a failure above is the missing retry and not the fixture.
func TestImageSync_ControlNoFailureConverges(t *testing.T) {
	body, digest := imageSyncTarball()
	url, requests := imageBucket(t, body, 0)
	_, _, imagesDir := startImageSyncWorker(t, url, digest)
	if !within(10*time.Second, func() bool { return installedVersion(imagesDir) == versionOf(digest) }) {
		t.Fatalf("control: installed = %q, want %s (%d download requests)", installedVersion(imagesDir), versionOf(digest), requests.Load())
	}
}

// A row whose URL serves bytes that do not match its digest fails only
// after the whole tarball (1.2 GB in production) has downloaded, and fails
// the same way every time. It must not be retried: the worker would
// re-download it on every backoff step, forever.
func TestImageSync_DigestMismatchIsNotRetried(t *testing.T) {
	body, _ := imageSyncTarball()
	url, requests := imageBucket(t, body, 0)
	_, _, imagesDir := startImageSyncWorker(t, url, strings.Repeat("0", 64))
	// Ten times the shortened backoff cap: a retry loop would have fetched
	// again several times by now.
	time.Sleep(400 * time.Millisecond)
	if n := requests.Load(); n != 1 {
		t.Fatalf("a digest-mismatched image was downloaded %d times, want 1", n)
	}
	if got := installedVersion(imagesDir); got != "" {
		t.Fatalf("installed = %q after a digest mismatch, want none", got)
	}
}

// The retry requests the image set whose fetch failed, and any newer
// request supersedes it: once a later snapshot moves the row to a URL that
// works and its own sync succeeds, the worker stops fetching the old URL.
func TestImageSync_NewerSnapshotSupersedesRetry(t *testing.T) {
	var oldRequests atomic.Int64
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		oldRequests.Add(1)
		http.Error(w, "SlowDown", http.StatusServiceUnavailable)
	}))
	t.Cleanup(broken.Close)
	body, digest := imageSyncTarball()
	url, _ := imageBucket(t, body, 0)

	c := NewCache(nil)
	c.ImagesDir = t.TempDir()
	fastImageRetry(c)
	t.Cleanup(func() { waitImageSyncIdle(t, c) })
	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 1, Images: []model.Image{{
		Name: "noble-pxe", SHA256: strings.Repeat("1", 64), URL: broken.URL + "/noble-pxe/old.tar.gz",
	}}})
	for deadline := time.Now().Add(5 * time.Second); oldRequests.Load() < 3; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the failing image was fetched %d times in 5s; want retries", oldRequests.Load())
		}
	}

	c.ApplySnapshot(&model.SiteSnapshot{SeqNum: 2, Images: []model.Image{{
		Name: "noble-pxe", SHA256: digest, URL: url,
	}}})
	if !within(10*time.Second, func() bool { return installedVersion(c.ImagesDir) == versionOf(digest) }) {
		t.Fatalf("installed = %q after the newer snapshot's sync, want %s", installedVersion(c.ImagesDir), versionOf(digest))
	}
	settled := oldRequests.Load()
	time.Sleep(400 * time.Millisecond)
	if n := oldRequests.Load(); n != settled {
		t.Fatalf("the superseded URL was fetched %d more times after the newer snapshot synced", n-settled)
	}
}
