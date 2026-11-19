package cmd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// TestMachineDeploy_Property_FlagsMapOntoBody: the deploy request body's
// destructive / wipe_all_disks / recovery fields are exactly what the typed
// flags say, under every spelling the flag parser accepts (bare, =true/=false,
// ParseBool forms, repeats where the last wins), and any argv the parser
// rejects sends no request at all. The reference is a hand-written model of
// pflag bool semantics, not pflag.
func TestMachineDeploy_Property_FlagsMapOntoBody(t *testing.T) {
	machineID := uuid.New()
	var mu sync.Mutex
	var gotBody map[string]any
	posted := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/machines/"+machineID.String()+"/deploy", func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		posted, gotBody = true, body
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(model.Operation{ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy})
	})
	cl := testServer(t, mux)
	rapid.Check(t, func(rt *rapid.T) {
		mu.Lock()
		posted, gotBody = false, nil
		mu.Unlock()
		type flagUse struct {
			name, spelling string
		}
		names := []string{"destructive", "wipe-all-disks", "recovery"}
		values := []string{"", "=true", "=false", "=1", "=0", "=t", "=F", "=TRUE", "=False", "=yes", " false", " true"}
		uses := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) flagUse {
			return flagUse{rapid.SampledFrom(names).Draw(t, "flag"), rapid.SampledFrom(values).Draw(t, "val")}
		}), 0, 5).Draw(rt, "uses")
		pos := rapid.IntRange(0, len(uses)).Draw(rt, "machinePos")

		// Reference model.
		state := map[string]bool{}
		parseOK := true
		extraPositional := 0
		var argv []string
		for i, u := range uses {
			if i == pos {
				argv = append(argv, machineID.String())
			}
			switch {
			case u.spelling == "":
				state[u.name] = true
				argv = append(argv, "--"+u.name)
			case strings.HasPrefix(u.spelling, " "):
				// "--flag false": a bool flag takes no separate value, so the
				// word becomes a second positional argument.
				state[u.name] = true
				extraPositional++
				argv = append(argv, "--"+u.name, strings.TrimSpace(u.spelling))
			default:
				v := strings.TrimPrefix(u.spelling, "=")
				b, err := strconv.ParseBool(v)
				if err != nil {
					parseOK = false
				}
				state[u.name] = b
				argv = append(argv, "--"+u.name+u.spelling)
			}
		}
		if pos == len(uses) {
			argv = append(argv, machineID.String())
		}
		if extraPositional > 0 {
			parseOK = false
		}

		args := append([]string{"machine", "deploy"}, argv...)
		_, err := execCmd(t, cl, args...)
		mu.Lock()
		sent, body := posted, gotBody
		mu.Unlock()
		if !parseOK {
			if err == nil || sent {
				rt.Fatalf("argv %q should be rejected without a request; err=%v posted=%v body=%v", argv, err, sent, body)
			}
			return
		}
		if err != nil {
			rt.Fatalf("argv %q: unexpected error %v", argv, err)
		}
		if !sent {
			rt.Fatalf("argv %q: accepted, but no deploy request was sent", argv)
		}
		wantDestructive := state["destructive"] || state["wipe-all-disks"]
		check := func(key string, want bool) {
			got, _ := body[key].(bool)
			if got != want {
				rt.Fatalf("argv %q: body[%q]=%v, want %v (body=%v)", argv, key, body[key], want, body)
			}
		}
		check("destructive", wantDestructive)
		check("wipe_all_disks", state["wipe-all-disks"])
		check("recovery", state["recovery"])
	})
}
