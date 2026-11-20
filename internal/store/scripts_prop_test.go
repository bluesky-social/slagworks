package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// Model-based differential test for versioned storage/network scripts.
//
// The reference model is a map name -> {id, []version}; nothing in it calls
// production helpers. Operations run over a three-name pool so creates
// collide, updates hit live, deleted and never-created names, and deletes
// use live, stale (already deleted) and random IDs. Content occasionally
// carries a NUL byte or invalid UTF-8, which Postgres text rejects: that is
// the fault schedule, and the property is that a failed create or update is
// atomic (no orphan identity row, head pointer not advanced).

type scriptKind struct {
	name         string
	create       func(s *Store, id uuid.UUID, name string, prio int, content string) (uuid.UUID, error)
	update       func(s *Store, name string, prio int, content string) (uuid.UUID, int, int, string, error)
	del          func(s *Store, id uuid.UUID) error
	list         func(s *Store) ([]model.StorageScript, error)
	listVersions func(s *Store, name string) ([]model.ScriptVersion, error)
}

var scriptKinds = []scriptKind{
	{
		name: "storage",
		create: func(s *Store, id uuid.UUID, name string, prio int, content string) (uuid.UUID, error) {
			sc := &model.StorageScript{ID: id, Name: name, Priority: prio, Content: content}
			err := s.CreateStorageScript(sc)
			return sc.ID, err
		},
		update: func(s *Store, name string, prio int, content string) (uuid.UUID, int, int, string, error) {
			sc, v, err := s.UpdateStorageScriptByName(name, prio, content)
			if err != nil {
				return uuid.Nil, 0, 0, "", err
			}
			return sc.ID, v, sc.Priority, sc.Content, nil
		},
		del:          func(s *Store, id uuid.UUID) error { return s.DeleteStorageScript(id) },
		list:         func(s *Store) ([]model.StorageScript, error) { return s.ListStorageScripts() },
		listVersions: func(s *Store, name string) ([]model.ScriptVersion, error) { return s.ListStorageScriptVersions(name) },
	},
	{
		name: "network",
		create: func(s *Store, id uuid.UUID, name string, prio int, content string) (uuid.UUID, error) {
			sc := &model.NetworkScript{ID: id, Name: name, Priority: prio, Content: content}
			err := s.CreateNetworkScript(sc)
			return sc.ID, err
		},
		update: func(s *Store, name string, prio int, content string) (uuid.UUID, int, int, string, error) {
			sc, v, err := s.UpdateNetworkScriptByName(name, prio, content)
			if err != nil {
				return uuid.Nil, 0, 0, "", err
			}
			return sc.ID, v, sc.Priority, sc.Content, nil
		},
		del: func(s *Store, id uuid.UUID) error { return s.DeleteNetworkScript(id) },
		list: func(s *Store) ([]model.StorageScript, error) {
			ns, err := s.ListNetworkScripts()
			out := make([]model.StorageScript, len(ns))
			for i, n := range ns {
				out[i] = model.StorageScript(n)
			}
			return out, err
		},
		listVersions: func(s *Store, name string) ([]model.ScriptVersion, error) { return s.ListNetworkScriptVersions(name) },
	},
}

type refVersion struct {
	prio    int
	content string
}

type refScript struct {
	id       uuid.UUID
	versions []refVersion
}

type scriptStats struct {
	creates, dupCreates, updates, missingUpdates, deletes, staleDeletes, faults, reinserts, listChecks int
	maxVersions                                                                                        int
}

func validPGText(s string) bool {
	return !strings.ContainsRune(s, 0) && strings.ToValidUTF8(s, "�") == s
}

func drawContent(t *rapid.T) string {
	switch rapid.IntRange(0, 19).Draw(t, "contentKind") {
	case 0:
		return "#!/bin/sh\necho \x00nul"
	case 1:
		return "#!/bin/sh\necho \xff\xfe"
	case 2:
		return ""
	default:
		return "#!/bin/sh\n# slagworks:priority x\n" + rapid.StringN(0, 40, -1).Draw(t, "contentBody")
	}
}

func drawPriority(t *rapid.T) int {
	return rapid.SampledFrom([]int{0, 100, 100, 200, -5, 1 << 40, -(1 << 62)}).Draw(t, "prio")
}

func clearScripts(t *rapid.T, s *Store) {
	// DELETE, not TRUNCATE: on tables this small it is much cheaper, and it
	// runs once per generated history. Version rows go with their script
	// (ON DELETE CASCADE).
	for _, q := range []string{"DELETE FROM storage_scripts", "DELETE FROM network_scripts"} {
		if err := s.db.Exec(q).Error; err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func checkScripts(t *rapid.T, s *Store, k scriptKind, ref map[string]*refScript, names []string, st *scriptStats) {
	st.listChecks++
	got, err := k.list(s)
	if err != nil {
		t.Fatalf("%s list: %v", k.name, err)
	}
	if len(got) != len(ref) {
		t.Fatalf("%s list: %d rows, model has %d (%v)", k.name, len(got), len(ref), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Priority > got[i].Priority {
			t.Fatalf("%s list not ordered by priority asc: %d then %d", k.name, got[i-1].Priority, got[i].Priority)
		}
	}
	for _, g := range got {
		r, ok := ref[g.Name]
		if !ok {
			t.Fatalf("%s list has %q which the model does not", k.name, g.Name)
		}
		head := r.versions[len(r.versions)-1]
		if g.ID != r.id || g.Priority != head.prio || g.Content != head.content {
			t.Fatalf("%s %q: got id=%s prio=%d content=%q, model id=%s prio=%d content=%q",
				k.name, g.Name, g.ID, g.Priority, g.Content, r.id, head.prio, head.content)
		}
	}
	for _, n := range names {
		vs, err := k.listVersions(s, n)
		r, ok := ref[n]
		if !ok {
			if !errors.Is(err, ErrScriptNotFound) {
				t.Fatalf("%s versions of absent %q: err=%v rows=%v", k.name, n, err, vs)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s versions %q: %v", k.name, n, err)
		}
		if len(vs) != len(r.versions) {
			t.Fatalf("%s versions %q: %d rows, model %d", k.name, n, len(vs), len(r.versions))
		}
		heads := 0
		for i, v := range vs {
			if v.Version != i+1 || v.Priority != r.versions[i].prio || v.Content != r.versions[i].content {
				t.Fatalf("%s %q version row %d = %+v, model v%d %+v", k.name, n, i, v, i+1, r.versions[i])
			}
			if v.Head {
				heads++
				if i != len(vs)-1 {
					t.Fatalf("%s %q: head marked on v%d, not latest v%d", k.name, n, v.Version, len(vs))
				}
			}
		}
		if heads != 1 {
			t.Fatalf("%s %q: %d head rows", k.name, n, heads)
		}
	}
}

func runScriptHistory(t *rapid.T, s *Store, k scriptKind, st *scriptStats) {
	names := []string{"a", "b", "ñ-script"}
	ref := map[string]*refScript{}
	var deadIDs []uuid.UUID
	everDeleted := map[string]bool{}
	nOps := rapid.IntRange(1, 30).Draw(t, "nOps")
	for i := range nOps {
		name := rapid.SampledFrom(names).Draw(t, "name")
		switch op := rapid.IntRange(0, 9).Draw(t, fmt.Sprintf("op%d", i)); {
		case op <= 2: // create
			content := drawContent(t)
			prio := drawPriority(t)
			var id uuid.UUID
			if len(deadIDs) > 0 && rapid.IntRange(0, 4).Draw(t, "reuseDeadID") == 0 {
				id = rapid.SampledFrom(deadIDs).Draw(t, "deadID")
			}
			_, exists := ref[name]
			gotID, err := k.create(s, id, name, prio, content)
			switch {
			case exists:
				st.dupCreates++
				if err == nil {
					t.Fatalf("%s create of existing %q succeeded", k.name, name)
				}
			case !validPGText(content):
				st.faults++
				if err == nil {
					t.Fatalf("%s create with content Postgres cannot store succeeded", k.name)
				}
			default:
				if err != nil {
					t.Fatalf("%s create %q: %v", k.name, name, err)
				}
				if id != uuid.Nil && gotID != id {
					t.Fatalf("%s create ignored caller ID", k.name)
				}
				if everDeleted[name] {
					st.reinserts++
				}
				st.creates++
				ref[name] = &refScript{id: gotID, versions: []refVersion{{prio, content}}}
				// A reused dead ID is live again.
				for j := 0; j < len(deadIDs); j++ {
					if deadIDs[j] == gotID {
						deadIDs = append(deadIDs[:j], deadIDs[j+1:]...)
						j--
					}
				}
			}
		case op <= 5: // update
			content := drawContent(t)
			prio := drawPriority(t)
			gotID, ver, gotPrio, gotContent, err := k.update(s, name, prio, content)
			r, exists := ref[name]
			switch {
			case !exists:
				st.missingUpdates++
				if !errors.Is(err, ErrScriptNotFound) {
					t.Fatalf("%s update of absent %q: err=%v", k.name, name, err)
				}
			case !validPGText(content):
				st.faults++
				if err == nil {
					t.Fatalf("%s update with content Postgres cannot store succeeded", k.name)
				}
			default:
				if err != nil {
					t.Fatalf("%s update %q: %v", k.name, name, err)
				}
				r.versions = append(r.versions, refVersion{prio, content})
				st.updates++
				if len(r.versions) > st.maxVersions {
					st.maxVersions = len(r.versions)
				}
				if gotID != r.id || ver != len(r.versions) || gotPrio != prio || gotContent != content {
					t.Fatalf("%s update %q returned id=%s v=%d prio=%d, model id=%s v=%d prio=%d",
						k.name, name, gotID, ver, gotPrio, r.id, len(r.versions), prio)
				}
			}
		case op <= 7: // delete by live, stale or random id
			var id uuid.UUID
			live := false
			if r, ok := ref[name]; ok && rapid.Bool().Draw(t, "delLive") {
				id, live = r.id, true
			} else if len(deadIDs) > 0 && rapid.Bool().Draw(t, "delStale") {
				id = rapid.SampledFrom(deadIDs).Draw(t, "staleID")
				st.staleDeletes++
			} else {
				id = uuid.New()
			}
			liveName := ""
			for n, r := range ref {
				if r.id == id {
					liveName, live = n, true
				}
			}
			if live {
				name = liveName
			}
			err := k.del(s, id)
			if live {
				if err != nil {
					t.Fatalf("%s delete live %q: %v", k.name, name, err)
				}
				delete(ref, name)
				deadIDs = append(deadIDs, id)
				everDeleted[name] = true
				st.deletes++
			} else if err == nil {
				t.Fatalf("%s delete of non-live id %s succeeded", k.name, id)
			}
		default:
			checkScripts(t, s, k, ref, names, st)
		}
	}
	checkScripts(t, s, k, ref, names, st)
}

// TestPropScriptVersioningMatchesModel draws the script kind per history
// rather than running the full check count once per kind: the two stores
// share one implementation shape, and every history costs a few dozen
// database round trips.
func TestPropScriptVersioningMatchesModel(t *testing.T) {
	s := testStore(t)
	st := map[string]*scriptStats{}
	for _, k := range scriptKinds {
		st[k.name] = &scriptStats{}
	}
	rapid.Check(t, func(rt *rapid.T) {
		k := scriptKinds[rapid.IntRange(0, len(scriptKinds)-1).Draw(rt, "kind")]
		clearScripts(rt, s)
		runScriptHistory(rt, s, k, st[k.name])
	})
	for _, k := range scriptKinds {
		t.Logf("%s: %+v", k.name, *st[k.name])
	}
}
