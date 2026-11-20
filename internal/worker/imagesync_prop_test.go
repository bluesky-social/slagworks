package worker

// Property coverage for the site worker's background image syncer
// (Cache.requestImageSync, runImageSyncs, retryImageSync) over the real
// imagepull.SyncImages and a local bucket.
//
// A history applies site snapshots whose image rows change while earlier
// downloads are still in flight, fail, hang and are then cut, or are pinned
// with a wrong digest. Alongside the worker, the same rows are synced into
// the same images dir from outside the cache, as `slagctl image sync --dir`
// run on the worker's host does.
//
// Nothing but a new snapshot asks a connected worker to sync again, so the
// worker has to get there on its own:
//
//   - once the bucket stops failing, every row of the latest snapshot whose
//     digest is right is installed, with no further snapshot, and stays
//     installed (a retry armed for an older snapshot must not undo it);
//   - a row whose bytes do not match its digest is downloaded once per sync
//     that carries it, and is left out of the retries of other rows;
//   - once everything is installed the worker stops requesting the bucket;
//   - the executor is handed a pending deploy only when no sync is running.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/imagepull"
	"github.com/bluesky-social/slagworks/internal/model"
)

// syncerBucket serves each path's script of faults, one entry per request,
// and the tarball once the script is used up.
type syncerBucket struct {
	mu       sync.Mutex
	blobs    map[string][]byte
	script   map[string][]string // "503", "cut", "hold"
	requests map[string]int
	holds    []chan struct{}
	open     bool // holds no longer block
}

func (b *syncerBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	body := b.blobs[r.URL.Path]
	b.requests[r.URL.Path]++
	var fault string
	if s := b.script[r.URL.Path]; len(s) > 0 {
		fault, b.script[r.URL.Path] = s[0], s[1:]
	}
	var hold chan struct{}
	if fault == "hold" && !b.open {
		hold = make(chan struct{})
		b.holds = append(b.holds, hold)
	}
	b.mu.Unlock()
	switch fault {
	case "503":
		http.Error(w, "SlowDown", http.StatusServiceUnavailable)
		return
	case "cut":
		// Half the body, then the connection closes: net/http closes it
		// when the handler returns short of the declared length.
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body[:len(body)/2])
		return
	}
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	_, _ = w.Write(body)
}

// releaseOne lets the oldest held download go on; it reports whether one
// was held.
func (b *syncerBucket) releaseOne() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.holds) == 0 {
		return false
	}
	close(b.holds[0])
	b.holds = b.holds[1:]
	return true
}

// releaseAll lets every held download go on, now and from here on.
func (b *syncerBucket) releaseAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.open = true
	for _, h := range b.holds {
		close(h)
	}
	b.holds = nil
}

func (b *syncerBucket) requestCounts() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]int, len(b.requests))
	for k, v := range b.requests {
		out[k] = v
	}
	return out
}

// syncerTarball is an image tarball whose four files read "<tag>:<file>".
func syncerTarball(tag string) ([]byte, string) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "image/", Typeflag: tar.TypeDir, Mode: 0o755})
	for _, f := range []string{"vmlinuz", "initrd", "initrd-local", "root.squashfs"} {
		body := tag + ":" + f
		_ = tw.WriteHeader(&tar.Header{Name: "image/" + f, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func TestProp_ImageSyncer_ConvergesOnTheLatestSnapshot(t *testing.T) {
	// The cache and its image sync log through slog.Default.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	names := []string{"noble-pxe", "jammy-pxe"}
	type built struct {
		blob []byte
		sum  string
		tag  string
	}
	builds := map[string][]built{}
	for _, name := range names {
		for i := 0; i < 3; i++ {
			tag := fmt.Sprintf("%s-build%d", name, i)
			blob, sum := syncerTarball(tag)
			builds[name] = append(builds[name], built{blob, sum, tag})
		}
	}
	stats := map[string]int{}

	rapid.Check(t, func(rt *rapid.T) {
		imagesDir, err := os.MkdirTemp("", "imgsyncer")
		if err != nil {
			rt.Fatalf("mkdtemp: %v", err)
		}
		defer os.RemoveAll(imagesDir)
		bucket := &syncerBucket{blobs: map[string][]byte{}, script: map[string][]string{}, requests: map[string]int{}}
		ts := httptest.NewServer(bucket)
		defer ts.Close()

		c := NewCache(nil)
		c.ImagesDir = imagesDir
		fastImageRetry(c)
		pendingMachine := uuid.New()
		var seq int64
		apply := func(images []model.Image) {
			seq++
			c.ApplySnapshot(&model.SiteSnapshot{SeqNum: seq, Images: images, Machines: []model.MachineSnapshot{{
				ID: pendingMachine, State: model.MachineStateCommissioned, PXEEligible: true,
				Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: "52:54:00:aa:bb:07"}},
			}}})
		}
		apply(nil)
		c.ApplyOpResync(nil, nil)
		c.ApplyOpDelta(model.Operation{ID: uuid.New(), MachineID: pendingMachine, Type: model.OpDeploy, Status: model.OpPending, Version: 1}, model.MachineStateCommissioned)

		var externals sync.WaitGroup
		// However the history ends, leave no syncer, retry or outside sync
		// running against a directory that is being removed.
		defer func() {
			bucket.releaseAll()
			externals.Wait()
			apply(nil)
			for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				c.imageSyncMu.Lock()
				done := !c.imageSyncRunning && c.imageRetryTimer == nil
				c.imageSyncMu.Unlock()
				if done {
					return
				}
			}
		}()

		// checkHold: while the bucket is holding a download and no outside
		// sync is running, the download is the worker's, so its sync is in
		// flight and the executor must not be handed the pending deploy.
		var outsideRunning atomic.Int32
		checkHold := func(when string) {
			bucket.mu.Lock()
			held := len(bucket.holds)
			bucket.mu.Unlock()
			if held == 0 || outsideRunning.Load() > 0 {
				return
			}
			stats["checked_while_a_worker_download_was_held"]++
			if n := len(c.PendingLifecycleOps()); n != 0 {
				rt.Fatalf("%s: the executor is handed %d pending deploys while the worker's image download is held by the bucket", when, n)
			}
		}

		type pinned struct {
			name, path, version, tag string
			wrongDigest              bool
			scriptLen                int
		}
		var latest []pinned
		latestImages := map[string]*model.Image{}
		externalSyncs := map[string]int{} // path -> outside syncs that carried it

		nOps := rapid.IntRange(1, 12).Draw(rt, "n_ops")
		for op := 0; op < nOps; op++ {
			switch rapid.SampledFrom([]string{"snapshot", "snapshot", "outside-sync", "release", "wait"}).Draw(rt, "op") {
			case "snapshot":
				// An outside sync reads its rows from the API, so by the
				// time the rows change the previous one is assumed done: a
				// sync still running with rows that are no longer pinned
				// could install them after the worker caught up.
				// Without one running, downloads held for the previous
				// snapshot stay held and this snapshot supersedes them.
				if outsideRunning.Load() > 0 {
					bucket.releaseAll()
					externals.Wait()
					bucket.mu.Lock()
					bucket.open = false
					bucket.mu.Unlock()
				}
				latest, latestImages = nil, map[string]*model.Image{}
				var images []model.Image
				for _, name := range names {
					if !rapid.Bool().Draw(rt, "row_"+name) {
						continue
					}
					idx := rapid.IntRange(0, 2).Draw(rt, "build")
					b := builds[name][idx]
					script := rapid.SliceOfN(rapid.SampledFrom([]string{"503", "cut", "hold"}), 0, 3).Draw(rt, "bucket_script")
					p := fmt.Sprintf("/s%d/%s/%d.tar.gz", seq, name, idx)
					bucket.mu.Lock()
					bucket.blobs[p] = b.blob
					bucket.script[p] = script
					bucket.mu.Unlock()
					pin := pinned{name: name, path: p, version: name + "@" + b.sum[:16], tag: b.tag, scriptLen: len(script)}
					sum := b.sum
					if rapid.IntRange(0, 7).Draw(rt, "wrong_digest") == 0 {
						sum = strings.Repeat("0", 64)
						pin.wrongDigest = true
						stats["rows_wrong_digest"]++
					}
					img := model.Image{Name: name, Version: "24.04", SHA256: sum, URL: ts.URL + p}
					images = append(images, img)
					latest = append(latest, pin)
					latestImages[name] = &img
					stats["rows"]++
					stats["bucket_faults"] += len(script)
				}
				apply(images)
				stats["snapshots"]++
			case "outside-sync":
				if len(latestImages) == 0 {
					continue
				}
				images := latestImages
				for _, p := range latest {
					externalSyncs[p.path]++
				}
				externals.Add(1)
				outsideRunning.Add(1)
				go func() {
					defer externals.Done()
					_ = imagepull.SyncImages(quiet, imagesDir, images)
					outsideRunning.Add(-1)
				}()
				stats["outside_syncs"]++
			case "release":
				if bucket.releaseOne() {
					stats["releases"]++
				}
			case "wait":
				time.Sleep(time.Duration(rapid.IntRange(1, 30).Draw(rt, "ms")) * time.Millisecond)
			}
			checkHold(fmt.Sprintf("op %d", op))
		}

		// The bucket stops holding; scripted failures run out by themselves.
		bucket.releaseAll()
		externals.Wait()

		installed := func() (string, bool) {
			for _, p := range latest {
				if p.wrongDigest {
					continue
				}
				if v, _ := imagepull.Installed(imagesDir, p.name); v != p.version {
					return fmt.Sprintf("%s links to %q, the latest snapshot pins %s", p.name, v, p.version), false
				}
				for _, f := range []string{"vmlinuz", "initrd", "initrd-local", "root.squashfs"} {
					if b, err := os.ReadFile(filepath.Join(imagesDir, p.name, f)); err != nil || string(b) != p.tag+":"+f {
						return fmt.Sprintf("%s/%s reads %q (%v), want %s's", p.name, f, b, err, p.tag), false
					}
				}
			}
			return "", true
		}
		quiescent := func() bool {
			c.imageSyncMu.Lock()
			defer c.imageSyncMu.Unlock()
			return !c.imageSyncRunning && c.imageRetryTimer == nil
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			_, ok := installed()
			if ok && quiescent() {
				break
			}
			if time.Now().After(deadline) {
				why, _ := installed()
				rt.Fatalf("20s after the bucket stopped failing, with no new snapshot, the worker has not settled on the latest snapshot's images: %s (syncer or retry still pending: %v)", why, !quiescent())
			}
			time.Sleep(time.Millisecond)
		}
		// It stays there, and the worker asks the bucket for nothing more.
		before := bucket.requestCounts()
		time.Sleep(3 * c.imageRetryMax / 2)
		if why, ok := installed(); !ok {
			rt.Fatalf("the worker installed the latest snapshot's images and then moved off them: %s", why)
		}
		after := bucket.requestCounts()
		for p, n := range after {
			if n != before[p] {
				rt.Fatalf("the worker requested %s %d more times after everything it can install was installed", p, n-before[p])
			}
		}
		if n := len(c.PendingLifecycleOps()); n != 1 {
			rt.Fatalf("no image sync is running, yet the executor is handed %d pending deploys, want 1", n)
		}
		// A row whose bytes do not match its digest fails the same way on
		// every attempt, each one a whole download: once per sync that
		// carries the row, after the scripted bucket faults, and never as
		// part of another row's retries.
		for _, p := range latest {
			if !p.wrongDigest {
				continue
			}
			if got, max := after[p.path], p.scriptLen+1+externalSyncs[p.path]; got < 1 || got > max {
				rt.Fatalf("%s is pinned with a wrong digest and was requested %d times; %d bucket faults, one worker sync and %d outside syncs allow 1 to %d",
					p.path, got, p.scriptLen, externalSyncs[p.path], max)
			}
		}
		stats["histories_converged"]++
	})
	t.Logf("coverage: %v", stats)
}
