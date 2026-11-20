package store

// Pairwise survey for the interleaving explorer: every unordered pair of
// candidate calls, in every starting situation, under a fixed family of
// context-bounded schedules. Unlike TestOpsmConc_RandomPairs it does not
// stop at the first hybrid: it reports every (situation, pair) class that has
// a non-linearizable schedule, and fails on any class opsmAcceptedRace does
// not name. It takes minutes, so it only runs when asked:
//
//	OPSM_SURVEY=1 go test ./internal/store/ -run TestOpsmConc_Survey -count=1 -v -timeout 2h
//	OPSM_SURVEY=2 ...   # also two-preemption schedules (about 2.5x slower)
//	OPSM_SURVEY_GOALS=deploying,awaiting_phone_home  # restrict situations

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

type opsmSurveySchedule struct {
	first int
	segs  []int
}

func opsmSurveySchedules(tier string) []opsmSurveySchedule {
	var out []opsmSurveySchedule
	for first := 0; first < 2; first++ {
		// One preemption: `first` sends k statements, the other runs to
		// completion (or until it blocks on first's locks), first finishes.
		for k := 1; k <= 6; k++ {
			out = append(out, opsmSurveySchedule{first, []int{k, 99}})
		}
		if tier == "2" {
			// Two preemptions: first k1, other k2, first to completion.
			for k1 := 1; k1 <= 4; k1++ {
				for k2 := 1; k2 <= 4; k2++ {
					out = append(out, opsmSurveySchedule{first, []int{k1, k2, 99}})
				}
			}
		}
	}
	return out
}

func TestOpsmConc_Survey(t *testing.T) {
	tier := os.Getenv("OPSM_SURVEY")
	if tier == "" {
		t.Skip("set OPSM_SURVEY=1 (or 2) to run the pairwise interleaving survey (minutes)")
	}
	st := testStore(t)
	goals := opsmConcGoals
	if g := os.Getenv("OPSM_SURVEY_GOALS"); g != "" {
		goals = strings.Split(g, ",")
	}
	schedules := opsmSurveySchedules(tier)
	start := time.Now()

	type hybrid struct {
		goal, accepted string
		a, b           opsmAction
		sched          opsmSurveySchedule
		report         string
	}
	var hybrids []hybrid
	runs, classes := 0, 0
	nCands := len(opsmConcCandidates(opsmBogusOp))
	for _, goal := range goals {
		for i := 0; i < nCands; i++ {
			for j := i; j < nCands; j++ {
				classes++
				for _, sc := range schedules {
					proto := opsmConcCandidates(opsmBogusOp)
					h, op := opsmSetupPair(t, st, goal, opsmBystanderGoal(proto[i], proto[j]))
					cands := opsmConcCandidates(op)
					a, b := cands[i], cands[j]
					res := h.runPair(a, b, opsmSegmentChooser(sc.first, sc.segs))
					opsmRecordPair(a, b, res)
					runs++
					if !res.ok {
						hybrids = append(hybrids, hybrid{goal, opsmAcceptedRace(a, b), a, b, sc, res.report})
						break // one witness per class is enough
					}
				}
			}
		}
		t.Logf("survey: goal %s done (%d runs so far, %s elapsed)", goal, runs, time.Since(start).Round(time.Second))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "interleaving survey: tier=%s goals=%d classes=%d runs=%d elapsed=%s\n",
		tier, len(goals), classes, runs, time.Since(start).Round(time.Second))
	fmt.Fprintf(&b, "%s\n", opsmConcReport())
	byReason := map[string][]hybrid{}
	for _, f := range hybrids {
		byReason[f.accepted] = append(byReason[f.accepted], f)
	}
	reasons := make([]string, 0, len(byReason))
	for k := range byReason {
		reasons = append(reasons, k)
	}
	sort.Strings(reasons)
	fmt.Fprintf(&b, "non-linearizable classes: %d\n", len(hybrids))
	for _, k := range reasons {
		name := k
		if name == "" {
			name = "NOT ACCEPTED"
		}
		fmt.Fprintf(&b, "\n== %s: %d classes ==\n", name, len(byReason[k]))
		for _, f := range byReason[k] {
			fmt.Fprintf(&b, "  goal=%-20s A=%-52s B=%s\n", f.goal, f.a.String(), f.b.String())
		}
	}
	t.Log(b.String())
	for _, f := range hybrids {
		if f.accepted == "" {
			t.Errorf("non-linearizable pair in situation %s (first=%d segments=%v): %s",
				f.goal, f.sched.first, f.sched.segs, f.report)
		}
	}
}
