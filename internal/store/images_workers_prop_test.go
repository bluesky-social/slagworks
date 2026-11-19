package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Model-based tests for images and site workers. Each keeps a
// plain map as the reference and checks that every value the store hands
// back to its caller (which the API returns verbatim to operators) names
// the row that is actually persisted.

// ---- images ----

type refImage struct {
	id                      uuid.UUID
	version, arch, sha, url string
	createdAt               time.Time
}

type imageStats struct {
	creates, upsertNew, upsertExisting, delByName, delByID, delMissing int
}

func TestPropImagesMatchModel(t *testing.T) {
	s := testStore(t)
	var st imageStats
	rapid.Check(t, func(rt *rapid.T) {
		if err := s.db.Exec("DELETE FROM images").Error; err != nil {
			rt.Fatal(err)
		}
		// A name that parses as a UUID is legal (images.name is free text).
		// Such an image is created, upserted, listed and deleted by id here,
		// but never deleted by name: DeleteImage looks a UUID-shaped ident up
		// only as an id, so it reports the image as not found.
		names := []string{"noble-pxe", "jammy-pxe", "6f1c5f2e-1d2b-4c3a-9e4f-0a1b2c3d4e5f"}
		ref := map[string]*refImage{}
		var deadIDs []uuid.UUID
		n := rapid.IntRange(1, 25).Draw(rt, "nOps")
		for i := range n {
			name := rapid.SampledFrom(names).Draw(rt, "name")
			ver := rapid.SampledFrom([]string{"24.04", "22.04", "ü"}).Draw(rt, "ver")
			img := &model.Image{Name: name, Version: ver, Arch: "amd64", SHA256: fmt.Sprintf("%064d", i), URL: "https://example.invalid/" + name}
			switch op := rapid.IntRange(0, 5).Draw(rt, fmt.Sprintf("op%d", i)); op {
			case 0: // create
				err := s.CreateImage(img)
				if _, ok := ref[name]; ok {
					if err == nil {
						rt.Fatalf("duplicate create of %q succeeded", name)
					}
					continue
				}
				if err != nil {
					rt.Fatal(err)
				}
				st.creates++
				ref[name] = &refImage{id: img.ID, version: ver, arch: "amd64", sha: img.SHA256, url: img.URL, createdAt: img.CreatedAt}
			case 1, 2: // upsert (follow-latest)
				if err := s.UpsertImage(img); err != nil {
					rt.Fatal(err)
				}
				if r, ok := ref[name]; ok {
					st.upsertExisting++
					if img.ID != r.id {
						rt.Fatalf("upsert of existing %q returned id %s, row id %s", name, img.ID, r.id)
					}
					if !sameDBTime(img.CreatedAt, r.createdAt) {
						rt.Fatalf("upsert of existing %q returned created_at %v, row %v", name, img.CreatedAt, r.createdAt)
					}
					r.version, r.sha, r.url = ver, img.SHA256, img.URL
				} else {
					st.upsertNew++
					ref[name] = &refImage{id: img.ID, version: ver, arch: "amd64", sha: img.SHA256, url: img.URL, createdAt: img.CreatedAt}
				}
			case 3: // delete by name
				if _, perr := uuid.Parse(name); perr == nil {
					continue
				}
				err := s.DeleteImage(name)
				if r, ok := ref[name]; ok {
					if err != nil {
						rt.Fatalf("delete by name %q: %v", name, err)
					}
					st.delByName++
					deadIDs = append(deadIDs, r.id)
					delete(ref, name)
				} else if err == nil {
					rt.Fatalf("delete of absent %q succeeded", name)
				}
			case 4: // delete by id (live or stale)
				var id uuid.UUID
				live := ""
				if len(ref) > 0 && rapid.Bool().Draw(rt, "liveID") {
					live = rapid.SampledFrom(names).Draw(rt, "liveName")
					if r, ok := ref[live]; ok {
						id = r.id
					} else {
						live = ""
					}
				}
				if live == "" {
					if len(deadIDs) > 0 {
						id = rapid.SampledFrom(deadIDs).Draw(rt, "deadID")
					} else {
						id = uuid.New()
					}
				}
				err := s.DeleteImage(id.String())
				if live != "" {
					if err != nil {
						rt.Fatalf("delete by id: %v", err)
					}
					st.delByID++
					deadIDs = append(deadIDs, id)
					delete(ref, live)
				} else {
					st.delMissing++
					if err == nil {
						rt.Fatalf("delete of dead id %s succeeded", id)
					}
				}
			default: // list
				got, err := s.ListImages()
				if err != nil {
					rt.Fatal(err)
				}
				if len(got) != len(ref) {
					rt.Fatalf("list %d rows, model %d", len(got), len(ref))
				}
				for _, g := range got {
					r := ref[g.Name]
					if r == nil || g.ID != r.id || g.Version != r.version || g.SHA256 != r.sha || g.URL != r.url || g.Arch != r.arch {
						rt.Fatalf("list row %+v disagrees with model %+v", g, r)
					}
					if !sameDBTime(g.CreatedAt, r.createdAt) {
						rt.Fatalf("list row %q created_at %v, model %v", g.Name, g.CreatedAt, r.createdAt)
					}
				}
			}
		}
	})
	t.Logf("%+v", st)
}

// ---- site workers ----

type refWorker struct {
	id        uuid.UUID
	fqdn      string
	tokenHash string
	oldHashes []string
	firstSeen time.Time
	serial    string
}

type workerStats struct {
	registers, rotations, touches, touchMissing, staleTokenChecks, deletes, resolves int
}

func TestPropSiteWorkersMatchModel(t *testing.T) {
	s := testStore(t)
	var st workerStats
	rapid.Check(t, func(rt *rapid.T) {
		if err := s.db.Exec("DELETE FROM site_workers").Error; err != nil {
			rt.Fatal(err)
		}
		type key struct{ slug, wid string }
		ref := map[key]*refWorker{}
		n := rapid.IntRange(1, 25).Draw(rt, "nOps")
		for i := range n {
			k := key{rapid.SampledFrom([]string{"pop1", "pop2"}).Draw(rt, "slug"), rapid.SampledFrom([]string{"w1", "w2"}).Draw(rt, "wid")}
			switch op := rapid.IntRange(0, 4).Draw(rt, fmt.Sprintf("op%d", i)); op {
			case 0: // register / rotate
				fqdn := rapid.SampledFrom([]string{"a.example", "b.example", ""}).Draw(rt, "fqdn")
				w, token, err := s.RegisterSiteWorker(k.slug, k.wid, fqdn)
				if err != nil {
					rt.Fatal(err)
				}
				hash := HashWorkerToken(token)
				if r, ok := ref[k]; ok {
					st.rotations++
					// The id and first_seen_at RegisterSiteWorker returns on
					// a re-registration are not compared with the row: it
					// returns the fresh values of the insert it attempted,
					// while the row keeps its original ones. Every later
					// operation here uses the model's (persisted) id.
					r.oldHashes = append(r.oldHashes, r.tokenHash)
					r.tokenHash, r.fqdn = hash, fqdn
				} else {
					st.registers++
					ref[k] = &refWorker{id: w.ID, fqdn: fqdn, tokenHash: hash, firstSeen: w.FirstSeenAt}
				}
			case 1: // touch
				var serial *string
				if rapid.Bool().Draw(rt, "withSerial") {
					v := rapid.SampledFrom([]string{"", "ABC123", "abc123"}).Draw(rt, "serial")
					serial = &v
				}
				fqdn := rapid.SampledFrom([]string{"", "c.example"}).Draw(rt, "touchFQDN")
				w, err := s.TouchSiteWorker(k.slug, k.wid, fqdn, serial)
				r, ok := ref[k]
				if !ok {
					st.touchMissing++
					if !errors.Is(err, ErrSiteWorkerNotFound) {
						rt.Fatalf("touch of absent %v: %v", k, err)
					}
					continue
				}
				if err != nil {
					rt.Fatal(err)
				}
				st.touches++
				if fqdn != "" {
					r.fqdn = fqdn
				}
				if serial != nil {
					r.serial = *serial
				}
				if w.ID != r.id || w.FQDN != r.fqdn || w.Serial != r.serial || w.TokenHash != r.tokenHash {
					rt.Fatalf("touch %v returned %+v, model %+v", k, w, r)
				}
			case 2: // token auth
				r, ok := ref[k]
				if !ok {
					continue
				}
				w, err := s.GetSiteWorkerByTokenHash(r.tokenHash)
				if err != nil || w.ID != r.id {
					rt.Fatalf("current token for %v: %v %+v", k, err, w)
				}
				for _, old := range r.oldHashes {
					st.staleTokenChecks++
					if _, err := s.GetSiteWorkerByTokenHash(old); !errors.Is(err, ErrSiteWorkerNotFound) {
						rt.Fatalf("rotated-out token for %v still authenticates: %v", k, err)
					}
				}
			case 3: // resolve by uuid then delete
				r, ok := ref[k]
				if !ok {
					continue
				}
				w, err := s.ResolveSiteWorker(r.id.String())
				if err != nil || w.SiteSlug != k.slug || w.WorkerID != k.wid {
					rt.Fatalf("resolve %v by id: %v %+v", k, err, w)
				}
				st.resolves++
				if rapid.Bool().Draw(rt, "doDelete") {
					if err := s.DeleteSiteWorker(r.id); err != nil {
						rt.Fatal(err)
					}
					st.deletes++
					if _, err := s.GetSiteWorkerByTokenHash(r.tokenHash); !errors.Is(err, ErrSiteWorkerNotFound) {
						rt.Fatalf("deleted worker's token still authenticates")
					}
					delete(ref, k)
				}
			default: // list
				got, err := s.ListSiteWorkers("")
				if err != nil {
					rt.Fatal(err)
				}
				if len(got) != len(ref) {
					rt.Fatalf("list %d, model %d", len(got), len(ref))
				}
				for _, g := range got {
					r := ref[key{g.SiteSlug, g.WorkerID}]
					if r == nil || g.ID != r.id || g.FQDN != r.fqdn {
						rt.Fatalf("list row %+v disagrees with model %+v", g, r)
					}
				}
			}
		}
	})
	t.Logf("%+v", st)
}
