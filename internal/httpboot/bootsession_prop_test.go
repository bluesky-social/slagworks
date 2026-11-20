package httpboot_test

// Property coverage for how a boot image reaches a target across one boot,
// over the real pieces of a site worker: the cache applies site snapshots
// and syncs their image rows in the background (worker.Cache, imagepull),
// and the boot handler and its file server serve the result (httpboot).
//
// One boot fetches image files at three separate moments:
//
//	pxe:        the boot script, then the kernel and initrd it names (iPXE)
//	initrd:     root.squashfs, by slagworks.image= (the BOOT hook)
//	bootloader: vmlinuz and initrd-local, by slagworks.image= (the agent,
//	            after partitioning and rsync, minutes later)
//
// An image is one artifact: its kernel, initrds and squashfs only work
// together. A history interleaves snapshots that re-pin, roll back, rename
// or remove the image rows (some downloads fail, some are held open while
// boots go on) with the stages of up to four concurrent boots.
//
// Reference model: per image name, the version its link names, the
// versions on disk (the linked one and the one before it), and a plain
// directory where an earlier release left one. A boot script names the
// linked version of the row the rows select, or the bare name when that row
// has never synced.
//
// Properties:
//
//   - a boot script names the image the model gives for the rows and the
//     directory at that moment, in its kernel URL, its initrd URL and
//     slagworks.image= alike;
//   - every file a boot fetches from a version directory is that version's,
//     or 404 once two later switches have removed the version; never
//     another version's;
//   - while a snapshot's image is still downloading, a pending deploy is
//     withheld from the executor and its machine boots from disk, and a
//     claimed deploy's script keeps naming the installed version.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/httpboot"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/worker"
)

const (
	sessionDefaultImage  = "noble-pxe"
	sessionRollbackImage = "noble-pxe-rollback"
	sessionPendingMAC    = "52:54:00:aa:bb:02"
)

var (
	sessionPendingMachine = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	initrdLine            = regexp.MustCompile(`(?m)^initrd (\S+)`)
)

// sessionBucket is the object storage the image rows point at.
type sessionBucket struct {
	mu      sync.Mutex
	blobs   map[string][]byte
	fail    map[string]bool
	hold    map[string]chan struct{}
	arrived chan string
}

func (b *sessionBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	body, fail, hold := b.blobs[r.URL.Path], b.fail[r.URL.Path], b.hold[r.URL.Path]
	b.mu.Unlock()
	if fail || body == nil {
		http.Error(w, "SlowDown", http.StatusServiceUnavailable)
		return
	}
	if hold != nil {
		select {
		case b.arrived <- r.URL.Path:
		default:
		}
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	_, _ = w.Write(body)
}

// sessionModel is the reference model of the images directory and the rows.
type sessionModel struct {
	rows  map[string]string // row name -> version directory it pins
	link  map[string]string // image name -> version directory its link names
	dirs  map[string]string // version directory on disk -> tag of its tarball
	plain map[string]string // image name -> tag of a plain directory at <name>
}

// sync applies one row's sync; ok is whether a download of it succeeds. It
// reports whether the row is downloaded.
func (m *sessionModel) sync(name, ver, tag string, ok bool) (fetched bool) {
	if m.link[name] == ver {
		return false
	}
	if _, onDisk := m.dirs[ver]; !onDisk {
		if !ok {
			return true
		}
		m.dirs[ver] = tag
		fetched = true
	}
	old, linked := m.link[name]
	m.link[name] = ver
	delete(m.plain, name)
	for d := range m.dirs {
		if strings.HasPrefix(d, name+"@") && d != ver && (!linked || d != old) {
			delete(m.dirs, d)
		}
	}
	return fetched
}

// bootImage is the image directory a boot script names.
func (m *sessionModel) bootImage() string {
	name := sessionDefaultImage
	if _, ok := m.rows[sessionDefaultImage]; !ok && len(m.rows) == 1 {
		for n := range m.rows {
			name = n
		}
	}
	if ver, ok := m.link[name]; ok {
		return ver
	}
	return name
}

// fileTag is the tag of the tarball a fetch of a file under image returns,
// and false when the fetch gets 404.
func (m *sessionModel) fileTag(image string) (string, bool) {
	if strings.Contains(image, "@") {
		tag, ok := m.dirs[image]
		return tag, ok
	}
	if ver, ok := m.link[image]; ok {
		return m.dirs[ver], true
	}
	tag, ok := m.plain[image]
	return tag, ok
}

type bootSession struct {
	image string // slagworks.image= of its boot script
	stage int
	tags  map[string]bool
}

func TestProp_BootSession_OneImageVersionPerBoot(t *testing.T) {
	// The cache and its image sync log through slog.Default.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(prev)

	const versions = 4
	tarballs := make([][]byte, versions)
	sums := make([]string, versions)
	for v := range tarballs {
		tarballs[v] = imageTarball(t, fmt.Sprintf("v%d", v+1))
		s := sha256.Sum256(tarballs[v])
		sums[v] = hex.EncodeToString(s[:])
	}
	stats := map[string]int{}

	rapid.Check(t, func(rt *rapid.T) {
		imagesDir, err := os.MkdirTemp("", "bootsession")
		if err != nil {
			rt.Fatalf("mkdtemp: %v", err)
		}
		defer os.RemoveAll(imagesDir)
		bucket := &sessionBucket{blobs: map[string][]byte{}, fail: map[string]bool{}, hold: map[string]chan struct{}{}, arrived: make(chan string, 8)}
		ts := httptest.NewServer(bucket)
		defer ts.Close()

		m := &sessionModel{rows: map[string]string{}, link: map[string]string{}, dirs: map[string]string{}, plain: map[string]string{}}
		if rapid.Bool().Draw(rt, "plain_dir_from_an_earlier_release") {
			m.plain[sessionDefaultImage] = "v0"
			if err := os.MkdirAll(filepath.Join(imagesDir, sessionDefaultImage), 0o755); err != nil {
				rt.Fatalf("seed: %v", err)
			}
			for _, f := range imageFiles {
				if err := os.WriteFile(filepath.Join(imagesDir, sessionDefaultImage, f), []byte("v0:"+f), 0o644); err != nil {
					rt.Fatalf("seed: %v", err)
				}
			}
		}

		c := worker.NewCache(nil)
		c.ImagesDir = imagesDir
		srv := httpboot.NewServer(":0", httpboot.ServerConfig{
			ImagesDir: imagesDir, DefaultImage: sessionDefaultImage, HTTPPort: 8080, DeployMode: true, Provider: c,
		})
		srv.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
		h := srv.Build()
		get := func(target string) (int, string) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
			return rec.Code, rec.Body.String()
		}

		var seq int64
		apply := func(images []model.Image) {
			seq++
			machine := func(id uuid.UUID, mac, systemUUID string) model.MachineSnapshot {
				return model.MachineSnapshot{
					ID: id, State: model.MachineStateCommissioned, PXEEligible: true,
					SystemUUID: systemUUID, KernelParams: "console=ttyS1,115200n8",
					Interfaces: []model.SnapshotInterface{{Name: "eth0", MAC: mac}},
				}
			}
			c.ApplySnapshot(&model.SiteSnapshot{SeqNum: seq, Images: images, Machines: []model.MachineSnapshot{
				machine(imageRowMachine, imageRowMAC, "4c4c4544-0042-3510-8051-b4c04f4a4433"),
				machine(sessionPendingMachine, sessionPendingMAC, "4c4c4544-0042-3510-8051-b4c04f4a4434"),
			}})
		}
		// The executor sees the pending deploy exactly when no image sync is
		// in flight.
		idle := func() bool { return len(c.PendingLifecycleOps()) == 1 }
		waitIdle := func(when string) {
			for deadline := time.Now().Add(20 * time.Second); !idle(); time.Sleep(time.Millisecond) {
				if time.Now().After(deadline) {
					rt.Fatalf("%s: the image sync did not finish within 20s", when)
				}
			}
		}
		apply(nil)
		c.ApplyOpResync(nil, nil)
		c.ApplyOpDelta(model.Operation{ID: uuid.New(), MachineID: imageRowMachine, Type: model.OpDeploy, Status: model.OpClaimed, Version: 1}, model.MachineStateDeploying)
		c.ApplyOpDelta(model.Operation{ID: uuid.New(), MachineID: sessionPendingMachine, Type: model.OpDeploy, Status: model.OpPending, Version: 1}, model.MachineStateCommissioned)
		waitIdle("first snapshot")
		// Whatever the history did, end with no rows and no sync running, so
		// no retry fires into a directory that is being removed.
		defer func() {
			bucket.mu.Lock()
			for p, ch := range bucket.hold {
				close(ch)
				delete(bucket.hold, p)
			}
			bucket.mu.Unlock()
			apply(nil)
			for deadline := time.Now().Add(20 * time.Second); !idle() && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			}
		}()

		// fetch GETs one file of image and checks it against the model.
		fetch := func(s *bootSession, what, target string) {
			code, body := get(target)
			wantTag, ok := m.fileTag(s.image)
			if !ok {
				if code != http.StatusNotFound {
					rt.Fatalf("%s: GET %s = %d %q; the model has nothing on disk for %s, want 404", what, target, code, body, s.image)
				}
				stats["fetch_404_version_removed_or_never_synced"]++
				return
			}
			tag, file, _ := strings.Cut(body, ":")
			if code != http.StatusOK || tag != wantTag || !strings.HasSuffix(target, "/"+file) {
				rt.Fatalf("%s: GET %s = %d %q, want 200 from image %s", what, target, code, body, wantTag)
			}
			s.tags[tag] = true
			stats["fetch_ok"]++
		}
		step := func(idx int, s *bootSession) {
			what := fmt.Sprintf("boot %d stage %d", idx, s.stage)
			switch s.stage {
			case 0:
				code, script := get("/boot/ipxe?mac=" + imageRowMAC)
				kernel := kernelLine.FindStringSubmatch(script)
				initrd := initrdLine.FindStringSubmatch(script)
				kdir := kernelImageDir.FindStringSubmatch(script)
				idir := initrdImageDir.FindStringSubmatch(script)
				image := cmdlineImage.FindStringSubmatch(script)
				if code != http.StatusOK || kernel == nil || initrd == nil || kdir == nil || idir == nil || image == nil {
					rt.Fatalf("%s: GET /boot/ipxe = %d, not a deploy boot script:\n%s", what, code, script)
				}
				want := m.bootImage()
				if kdir[1] != want || idir[1] != want || image[1] != want {
					rt.Fatalf("%s: the script names kernel dir %s, initrd dir %s, slagworks.image=%s; the rows and the images dir give %s",
						what, kdir[1], idir[1], image[1], want)
				}
				s.image = want
				// iPXE fetches the two URLs the script names: it expands
				// ${next-server} and percent-encodes '@' in the request path.
				for _, raw := range []string{kernel[1], initrd[1]} {
					u, err := url.Parse(strings.ReplaceAll(raw, "${next-server}", "worker"))
					if err != nil {
						rt.Fatalf("%s: script URL %q: %v", what, raw, err)
					}
					fetch(s, what, strings.ReplaceAll(u.EscapedPath(), "@", "%40"))
				}
			case 1:
				fetch(s, what, "/files/images/"+s.image+"/root.squashfs")
			case 2:
				fetch(s, what, "/files/images/"+s.image+"/vmlinuz")
				fetch(s, what, "/files/images/"+s.image+"/initrd-local")
			}
			s.stage++
			if s.stage == 3 {
				stats["boots_completed"]++
				if len(s.tags) > 1 {
					// Only a boot whose script named the bare name can get
					// here: a plain directory from an earlier release,
					// replaced by the link while the boot was in flight.
					if strings.Contains(s.image, "@") {
						rt.Fatalf("boot %d on %s fetched files of %d image versions: %v", idx, s.image, len(s.tags), s.tags)
					}
					stats["boots_by_name_across_the_first_switch"]++
				}
			}
		}

		var sessions []*bootSession
		stepSession := func() {
			var live []int
			for i, s := range sessions {
				if s.stage < 3 {
					live = append(live, i)
				}
			}
			if len(live) == 0 || (len(sessions) < 4 && rapid.Bool().Draw(rt, "new_boot")) {
				sessions = append(sessions, &bootSession{tags: map[string]bool{}})
				live = append(live, len(sessions)-1)
				stats["boots"]++
			}
			i := rapid.SampledFrom(live).Draw(rt, "boot")
			step(i, sessions[i])
		}

		nOps := rapid.IntRange(1, 30).Draw(rt, "n_ops")
		for op := 0; op < nOps; op++ {
			if rapid.IntRange(0, 2).Draw(rt, "op") > 0 {
				stepSession()
				continue
			}
			// A snapshot with new image rows.
			pattern := rapid.SampledFrom([]string{"default", "default", "default", "rollback-only", "both", "none"}).Draw(rt, "rows")
			var names []string
			switch pattern {
			case "default":
				names = []string{sessionDefaultImage}
			case "rollback-only":
				names = []string{sessionRollbackImage}
			case "both":
				names = []string{sessionDefaultImage, sessionRollbackImage}
			}
			var images []model.Image
			m.rows = map[string]string{}
			var held []string
			type pending struct {
				name, ver, tag string
				ok             bool
			}
			var syncs []pending
			after := &sessionModel{rows: m.rows, link: maps.Clone(m.link), dirs: maps.Clone(m.dirs), plain: maps.Clone(m.plain)}
			for _, name := range names {
				v := rapid.IntRange(0, versions-1).Draw(rt, "version")
				fault := rapid.SampledFrom([]string{"", "", "", "fail", "hold"}).Draw(rt, "fault")
				p := fmt.Sprintf("/s%d/%s/v%d.tar.gz", seq, name, v+1)
				ver := name + "@" + sums[v][:16]
				m.rows[name] = ver
				bucket.mu.Lock()
				bucket.blobs[p] = tarballs[v]
				bucket.fail[p] = fault == "fail"
				bucket.mu.Unlock()
				fetched := after.sync(name, ver, fmt.Sprintf("v%d", v+1), fault != "fail")
				if fault == "hold" && fetched && len(held) == 0 {
					bucket.mu.Lock()
					bucket.hold[p] = make(chan struct{})
					bucket.mu.Unlock()
					held = append(held, p)
				}
				syncs = append(syncs, pending{name, ver, fmt.Sprintf("v%d", v+1), fault != "fail"})
				images = append(images, model.Image{Name: name, Version: "24.04", SHA256: sums[v], URL: ts.URL + p})
				stats["rows_"+fault]++
			}
			stats["snapshots_"+pattern]++
			apply(images)
			if len(held) > 0 {
				// The download is in flight. The directory is as it was (the
				// other row of this snapshot may have synced already, or
				// may be waiting behind this one, so only snapshots with
				// one row check the directory against the model here).
				select {
				case <-bucket.arrived:
				case <-time.After(20 * time.Second):
					rt.Fatalf("snapshot %d: the worker never requested %s", seq, held[0])
				}
				if n := len(c.PendingLifecycleOps()); n != 0 {
					rt.Fatalf("snapshot %d: %d pending deploy handed to the executor while the snapshot's image is still downloading", seq, n)
				}
				if _, script := get("/boot/ipxe?mac=" + sessionPendingMAC); kernelLine.MatchString(script) {
					rt.Fatalf("snapshot %d: a pending deploy's machine was given a kernel while the snapshot's image is still downloading:\n%s", seq, script)
				}
				if len(names) == 1 {
					for i, n := 0, rapid.IntRange(0, 4).Draw(rt, "boots_during_download"); i < n; i++ {
						stepSession()
						stats["boot_stages_during_a_download"]++
					}
				}
				bucket.mu.Lock()
				close(bucket.hold[held[0]])
				delete(bucket.hold, held[0])
				bucket.mu.Unlock()
			}
			waitIdle(fmt.Sprintf("snapshot %d", seq))
			for _, s := range syncs {
				m.sync(s.name, s.ver, s.tag, s.ok)
			}
		}
	})
	t.Logf("coverage: %v", stats)
}
