package configscript

import (
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"pgregory.net/rapid"
)

// Property-based coverage for the shared header grammar and selection
// engine. Generated scripts draw from a small field vocabulary and a small
// value pool (exact strings and /regex/ forms that overlap on purpose) so
// several scripts match one host, priorities tie, and regex/exact rules
// interact.
//
// Oracles:
//   - round trip: headers rendered from a generated rule list (with random
//     but legal formatting: CRLF, blank lines, shebang, "#" with and without
//     a space, a trailing body that itself contains header-looking comments)
//     parse back to exactly that rule list and priority;
//   - an independent reference matcher (written from the Match doc comment)
//     agrees with Match on winner/ambiguity/no-match;
//   - Match is invariant under permutation of the script slice;
//   - Parse never panics on arbitrary bytes and only returns rules from the
//     vocabulary (fuzz target below).

var propFields = map[string]struct{}{"site": {}, "location": {}, "hostname": {}, "u": {}}

var propFieldList = []string{"site", "location", "hostname", "u"}

// valuePool overlaps: "pop2" exact, /pop[12]/ regex, /.*/ catch-all, etc.
var valuePool = map[string][]string{
	"site":     {"pop1", "pop2", "/pop[12]/", "/.*/", "/pop2|pop3/"},
	"location": {"cage-a", "cage-b", "/cage-.*/"},
	"hostname": {"hot-spare-1.pop2", "/hot-spare-[12]\\.pop2/", "/kworker-.*/", "/.*\\.pop2/"},
	"u":        {"12", "12.5", "/1[0-9]/"},
}

var hostPool = map[string][]string{
	"site":     {"pop1", "pop2", "pop3", ""},
	"location": {"cage-a", "cage-b", ""},
	"hostname": {"hot-spare-1.pop2", "hot-spare-2.pop2", "kworker-1.pop1", "kworker-1.pop2", ""},
	"u":        {"12", "12.5", "15", ""},
}

type genRule struct{ field, value string }

type genScript struct {
	name     string
	rules    []genRule
	priority int
	hasPrio  bool
}

func drawScript(t *rapid.T, idx int) genScript {
	g := genScript{name: fmt.Sprintf("s%d", idx)}
	n := rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("s%dnRules", idx))
	for i := 0; i < n; i++ {
		f := rapid.SampledFrom(propFieldList).Draw(t, fmt.Sprintf("s%dr%dfield", idx, i))
		v := rapid.SampledFrom(valuePool[f]).Draw(t, fmt.Sprintf("s%dr%dval", idx, i))
		g.rules = append(g.rules, genRule{f, v})
	}
	g.hasPrio = rapid.IntRange(0, 4).Draw(t, fmt.Sprintf("s%dhasPrio", idx)) != 0
	if g.hasPrio {
		g.priority = rapid.SampledFrom([]int{-5, 0, 100, 100, 125, 150, 200}).Draw(t, fmt.Sprintf("s%dprio", idx))
	}
	return g
}

// render writes the headers with legal formatting variations.
func render(t *rapid.T, g genScript, idx int) string {
	nl := "\n"
	if rapid.IntRange(0, 4).Draw(t, fmt.Sprintf("s%dcrlf", idx)) == 0 {
		nl = "\r\n"
	}
	var b strings.Builder
	if rapid.Bool().Draw(t, fmt.Sprintf("s%dshebang", idx)) {
		b.WriteString("#!/usr/bin/env python3" + nl)
	}
	hash := func(i int) string {
		return rapid.SampledFrom([]string{"# ", "#", "#  ", "  # "}).Draw(t, fmt.Sprintf("s%dhash%d", idx, i))
	}
	lines := []string{}
	for i, r := range g.rules {
		sp := rapid.SampledFrom([]string{"", " "}).Draw(t, fmt.Sprintf("s%dsp%d", idx, i))
		lines = append(lines, hash(i)+"slagworks:match "+sp+r.field+sp+"="+sp+r.value+sp)
	}
	if g.hasPrio {
		lines = append(lines, hash(99)+fmt.Sprintf("slagworks:priority %d", g.priority))
	}
	// Interleave plain comments and blank lines.
	for i, l := range lines {
		b.WriteString(l + nl)
		switch rapid.IntRange(0, 3).Draw(t, fmt.Sprintf("s%dfill%d", idx, i)) {
		case 0:
			b.WriteString(nl)
		case 1:
			b.WriteString("# just a comment" + nl)
		}
	}
	// Body: header-looking lines after the first statement must be ignored.
	b.WriteString("import sys" + nl)
	b.WriteString("# slagworks:match site=pop3" + nl)
	b.WriteString("# slagworks:priority 9999" + nl)
	return b.String()
}

// refMatch: candidates = scripts with >=1 rule whose rules all match
// (exact string equality, or full-string regex for /re/ values longer than
// two chars); winner = unique candidate at the max priority; a tie at the
// max priority is ambiguous.
func refMatch(gs []genScript, host map[string]string) (winner string, ambiguous bool) {
	best := 0
	var at []string
	for _, g := range gs {
		if len(g.rules) == 0 {
			continue
		}
		ok := true
		for _, r := range g.rules {
			v := host[r.field]
			if len(r.value) > 2 && r.value[0] == '/' && r.value[len(r.value)-1] == '/' {
				re := regexp.MustCompile(`\A(?:` + r.value[1:len(r.value)-1] + `)\z`)
				if !re.MatchString(v) {
					ok = false
				}
			} else if r.value != v {
				ok = false
			}
		}
		if !ok {
			continue
		}
		switch {
		case len(at) == 0 || g.priority > best:
			best, at = g.priority, []string{g.name}
		case g.priority == best:
			at = append(at, g.name)
		}
	}
	switch len(at) {
	case 0:
		return "", false
	case 1:
		return at[0], false
	default:
		return "", true
	}
}

var cvMatch, cvAmbig, cvNone, cvRegexWin atomic.Int64

func TestProp_ParseRoundTripAndMatch(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 5).Draw(t, "nScripts")
		var gens []genScript
		var parsed []Script
		for i := 0; i < n; i++ {
			g := drawScript(t, i)
			content := render(t, g, i)
			s, err := Parse(g.name, []byte(content), propFields)
			if err != nil {
				t.Fatalf("Parse(%q): %v", content, err)
			}
			// Round trip.
			if len(s.Rules) != len(g.rules) {
				t.Fatalf("parsed %d rules, generated %d\ncontent=%q\nrules=%+v", len(s.Rules), len(g.rules), content, s.Rules)
			}
			for j, r := range s.Rules {
				if r.Field != g.rules[j].field || r.Value != g.rules[j].value {
					t.Fatalf("rule %d = %s=%q, want %s=%q (content %q)", j, r.Field, r.Value, g.rules[j].field, g.rules[j].value, content)
				}
			}
			if s.Priority != g.priority {
				t.Fatalf("priority %d, want %d (content %q)", s.Priority, g.priority, content)
			}
			if string(s.Content) != content {
				t.Fatalf("content not preserved")
			}
			gens = append(gens, g)
			parsed = append(parsed, *s)
		}
		host := map[string]string{}
		for _, f := range propFieldList {
			host[f] = rapid.SampledFrom(hostPool[f]).Draw(t, "host_"+f)
		}
		resolve := func(f string) string { return host[f] }
		want, wantAmbig := refMatch(gens, host)

		check := func(label string, scripts []Script) {
			got, err := Match(scripts, resolve)
			switch {
			case wantAmbig:
				if err == nil {
					t.Fatalf("%s: want ambiguity error, got %v", label, got)
				}
			case want == "":
				if err != nil || got != nil {
					t.Fatalf("%s: want no match, got %v err %v", label, got, err)
				}
			default:
				if err != nil || got == nil || got.Path != want {
					t.Fatalf("%s: want %s, got %+v err %v", label, want, got, err)
				}
			}
		}
		check("orig", parsed)
		// Permutation invariance (and Match must not reorder its input).
		before := make([]string, len(parsed))
		for i := range parsed {
			before[i] = parsed[i].Path
		}
		perm := append([]Script(nil), parsed...)
		for i := len(perm) - 1; i > 0; i-- {
			j := rapid.IntRange(0, i).Draw(t, fmt.Sprintf("perm%d", i))
			perm[i], perm[j] = perm[j], perm[i]
		}
		check("permuted", perm)
		for i := range parsed {
			if parsed[i].Path != before[i] {
				t.Fatalf("Match mutated its input slice")
			}
		}
		switch {
		case wantAmbig:
			cvAmbig.Add(1)
		case want == "":
			cvNone.Add(1)
		default:
			cvMatch.Add(1)
			for _, g := range gens {
				if g.name == want {
					for _, r := range g.rules {
						if strings.HasPrefix(r.value, "/") {
							cvRegexWin.Add(1)
							break
						}
					}
				}
			}
		}
	})
	t.Logf("coverage: matched=%d (regex-rule winners=%d) ambiguous=%d none=%d", cvMatch.Load(), cvRegexWin.Load(), cvAmbig.Load(), cvNone.Load())
}

// TestProp_DirectiveNeverSilentlyDropped: a header comment whose text names
// a slagworks:match / slagworks:priority directive is either applied or
// rejected; it is never ignored. Generated variants use whitespace that a
// person or editor plausibly types (tab after the directive, a colon, a
// missing space).
func TestProp_DirectiveNeverSilentlyDropped(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sep := rapid.SampledFrom([]string{" ", "\t", ":", "  ", ": "}).Draw(t, "sep")
		directive := rapid.SampledFrom([]string{"match", "priority"}).Draw(t, "directive")
		arg := "site=pop2"
		if directive == "priority" {
			arg = "150"
		}
		content := "#!/bin/sh\n# slagworks:match hostname=/h.*/\n# slagworks:" + directive + sep + arg + "\necho\n"
		s, err := Parse("x", []byte(content), propFields)
		if err != nil {
			return // rejected loudly: fine
		}
		applied := false
		if directive == "match" {
			for _, r := range s.Rules {
				if r.Field == "site" && r.Value == "pop2" {
					applied = true
				}
			}
		} else {
			applied = s.Priority == 150
		}
		if !applied {
			t.Fatalf("header %q neither applied nor rejected: rules=%+v priority=%d", "# slagworks:"+directive+sep+arg, s.Rules, s.Priority)
		}
	})
}

func FuzzParse(f *testing.F) {
	f.Add([]byte("#!/bin/sh\n# slagworks:match site=pop2\n# slagworks:priority 100\n"))
	f.Add([]byte("# slagworks:match hostname=/(/\n"))
	f.Add([]byte("# slagworks:match u=//\n# slagworks:priority -1\r\n"))
	f.Add([]byte("\xff\xfe# slagworks:match site=\x00\n"))
	f.Fuzz(func(t *testing.T, content []byte) {
		s, err := Parse("fuzz", content, propFields)
		if err != nil {
			return
		}
		for _, r := range s.Rules {
			if _, ok := propFields[r.Field]; !ok {
				t.Fatalf("rule field %q outside vocabulary", r.Field)
			}
			if r.Value == "" {
				t.Fatalf("empty rule value accepted")
			}
		}
		// Matching against any resolver must not panic.
		_, _ = Match([]Script{*s, *s}, func(string) string { return "pop2" })
	})
}
