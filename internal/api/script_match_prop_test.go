package api

// Script selection end to end: scripts uploaded through the API (create,
// then any number of updates, some deleted) are read back as the snapshot
// builder reads them, parsed, and matched against a generated host; the
// winner is compared with an independent matcher that works from the
// generated rule structure, never from the script text.
//
// Contract (internal/configscript, docs/user/storage-scripts.md): a script
// matches when it has at least one rule and every rule matches; the highest
// priority match wins; two matches tied at the winning priority are an
// error, not a coin flip; a /regex/ value must match the whole field.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/storage"
)

// smValue is a rule value with its match set stated extensionally over the
// host value pool {"A", "B", "AB", ""}, so the oracle never runs a regexp.
type smValue struct {
	text    string
	matches map[string]bool
}

var smValues = []smValue{
	{"A", map[string]bool{"A": true}},
	{"B", map[string]bool{"B": true}},
	{"/A|B/", map[string]bool{"A": true, "B": true}},                      // anchored: not "AB"
	{"/A.*/", map[string]bool{"A": true, "AB": true}},                     // prefix
	{"/.*/", map[string]bool{"A": true, "B": true, "AB": true, "": true}}, // everything, the empty field included
	{"/a/", map[string]bool{}},                                            // case-sensitive
	{"/B/", map[string]bool{"B": true}},                                   // anchored: not "AB"
}

type smRule struct {
	field string
	value smValue
}

type smScript struct {
	name     string
	priority int
	rules    []smRule
}

func (s smScript) text() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	for _, r := range s.rules {
		fmt.Fprintf(&b, "# slagworks:match %s=%s\n", r.field, r.value.text)
	}
	fmt.Fprintf(&b, "# slagworks:priority %d\n", s.priority)
	b.WriteString("echo layout\n")
	return b.String()
}

func TestProp_StorageScriptSelection(t *testing.T) {
	stats := newPropStats()
	defer stats.report(t)
	fields := []string{"system_manufacturer", "system_product", "hostname"}

	propCheck(t, 50, func(rt *rapid.T) {
		env := newPropEnv(t, nil)
		draw := func(name, label string) smScript {
			s := smScript{name: name, priority: rapid.SampledFrom([]int{0, 10, 10, 20, -1}).Draw(rt, label+"priority")}
			for i := range rapid.IntRange(0, 2).Draw(rt, label+"rules") {
				s.rules = append(s.rules, smRule{
					field: rapid.SampledFrom(fields).Draw(rt, fmt.Sprintf("%sfield%d", label, i)),
					value: rapid.SampledFrom(smValues).Draw(rt, fmt.Sprintf("%svalue%d", label, i)),
				})
			}
			return s
		}

		// Upload history: create, then update or delete some. The head
		// version is what must be matched.
		head := map[string]smScript{}
		ids := map[string]string{}
		n := rapid.IntRange(1, 5).Draw(rt, "scripts")
		for i := range n {
			name := fmt.Sprintf("s%d", i)
			s := draw(name, name)
			body, _ := json.Marshal(map[string]string{"name": name, "content": s.text()})
			resp := env.post("/api/v1/storage-scripts", string(body))
			var created model.StorageScript
			if resp.code != http.StatusCreated || resp.json(&created) != nil || created.Priority != s.priority {
				rt.Fatalf("create %s: %s (want priority %d)\n%s", name, resp, s.priority, s.text())
			}
			head[name], ids[name] = s, created.ID.String()
		}
		for i := range rapid.IntRange(0, 4).Draw(rt, "edits") {
			name := fmt.Sprintf("s%d", rapid.IntRange(0, n-1).Draw(rt, fmt.Sprintf("edit%d", i)))
			if _, ok := head[name]; !ok {
				continue
			}
			if rapid.IntRange(0, 3).Draw(rt, fmt.Sprintf("delete%d", i)) == 0 {
				if resp := env.do(propReq{method: http.MethodDelete, path: "/api/v1/storage-scripts/" + ids[name]}); resp.code != http.StatusNoContent {
					rt.Fatalf("delete %s: %s", name, resp)
				}
				delete(head, name)
				stats.add("deleted a script")
				continue
			}
			s := draw(name, fmt.Sprintf("edit%d_", i))
			body, _ := json.Marshal(map[string]string{"content": s.text()})
			if resp := env.do(propReq{method: http.MethodPut, path: "/api/v1/storage-scripts/" + name, body: string(body)}); resp.code != http.StatusOK {
				rt.Fatalf("update %s: %s", name, resp)
			}
			head[name] = s
			stats.add("updated a script")
		}

		host := map[string]string{}
		for _, f := range fields {
			host[f] = rapid.SampledFrom([]string{"A", "B", "AB", ""}).Draw(rt, "host_"+f)
		}

		// Independent matcher over the generated structure.
		var winners []string
		best := 0
		for name, s := range head {
			if len(s.rules) == 0 {
				continue
			}
			ok := true
			for _, r := range s.rules {
				if !r.value.matches[host[r.field]] {
					ok = false
				}
			}
			if !ok {
				continue
			}
			switch {
			case len(winners) == 0 || s.priority > best:
				winners, best = []string{name}, s.priority
			case s.priority == best:
				winners = append(winners, name)
			}
		}

		// Production path: the rows the snapshot builder reads, parsed and
		// matched by the functions it calls.
		rows, err := env.st.ListStorageScripts()
		if err != nil {
			rt.Fatalf("list scripts: %v", err)
		}
		if len(rows) != len(head) {
			rt.Fatalf("store lists %d scripts, %d exist", len(rows), len(head))
		}
		parsed := make([]storage.Script, 0, len(rows))
		for _, row := range rows {
			p, err := storage.ParseScript(row.Name, []byte(row.Content))
			if err != nil {
				rt.Fatalf("a script the API accepted does not parse: %v", err)
			}
			if p.Priority != row.Priority {
				rt.Fatalf("script %s: stored priority %d, header says %d", row.Name, row.Priority, p.Priority)
			}
			parsed = append(parsed, *p)
		}
		commission, _ := json.Marshal(map[string]any{"system": map[string]string{
			"manufacturer": host["system_manufacturer"], "product_name": host["system_product"]}})
		got, err := storage.Match(parsed, commission, map[string]string{"hostname": host["hostname"]})

		switch len(winners) {
		case 0:
			if err != nil || got != nil {
				rt.Fatalf("no script matches host %v, but Match returned %v, %v\nscripts: %+v", host, got, err, head)
			}
			stats.add("no match")
		case 1:
			if err != nil || got == nil || got.Path != winners[0] {
				name := "<nil>"
				if got != nil {
					name = got.Path
				}
				rt.Fatalf("host %v must get script %s (priority %d); Match returned %s, %v\nscripts: %+v", host, winners[0], best, name, err, head)
			}
			stats.add("unique winner")
		default:
			if err == nil {
				rt.Fatalf("scripts %v tie at priority %d for host %v; Match must refuse, returned %v", winners, best, host, got)
			}
			stats.add("tie refused")
		}
		stats.add("cases")
	})
}
