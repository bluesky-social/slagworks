// Package configscript is the shared rule-matching engine behind slagworks'
// pluggable per-host config scripts. Both storage scripts (matched on DMI
// commission fields) and network-config scripts (matched on NetBox identity
// fields) parse the same slagworks:match / slagworks:priority header grammar
// and select a single highest-priority script per host; only the valid field
// set and the per-host field resolver differ. This package owns the grammar
// and the selection; callers supply the field vocabulary and the resolver.
package configscript

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Script holds the parsed metadata and content of a config script.
type Script struct {
	Path     string
	Content  []byte
	Priority int
	Rules    []MatchRule // AND'd; empty = never matches
}

// MatchRule is one slagworks:match directive from a script header.
type MatchRule struct {
	Field   string // one of the caller's valid fields
	Value   string // raw value from header (exact string or /regex/)
	pattern *regexp.Regexp
}

// Parse scans the slagworks metadata headers from a script's content. Only
// comment lines at the top of the file are scanned; scanning stops at the
// first non-comment, non-shebang line. validFields gates the allowed match
// fields — a slagworks:match against any other field is an error, so a typo'd
// field name fails loudly at upload time instead of silently never matching.
func Parse(path string, content []byte, validFields map[string]struct{}) (*Script, error) {
	s := &Script{
		Path:    path,
		Content: content,
	}

	for line := range strings.SplitSeq(string(content), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#!") {
			continue // shebang
		}
		if !strings.HasPrefix(trimmed, "#") {
			break // stop at first non-comment line
		}

		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))

		if after, ok := strings.CutPrefix(rest, "slagworks:match "); ok {
			parts := strings.SplitN(strings.TrimSpace(after), "=", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, fmt.Errorf("%s: invalid match rule: %q", path, after)
			}
			rule := MatchRule{
				Field: strings.TrimSpace(parts[0]),
				Value: strings.TrimSpace(parts[1]),
			}
			if _, ok := validFields[rule.Field]; !ok {
				return nil, fmt.Errorf("%s: unknown match field %q (valid: %s)", path, rule.Field, sortedFields(validFields))
			}
			if strings.HasPrefix(rule.Value, "/") && strings.HasSuffix(rule.Value, "/") && len(rule.Value) > 2 {
				re := rule.Value[1 : len(rule.Value)-1]
				compiled, err := regexp.Compile("^(?:" + re + ")$")
				if err != nil {
					return nil, fmt.Errorf("%s: invalid regex in match rule: %w", path, err)
				}
				rule.pattern = compiled
			}
			s.Rules = append(s.Rules, rule)
		} else if after, ok := strings.CutPrefix(rest, "slagworks:priority "); ok {
			p, err := parsePriority(after)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			s.Priority = p
		}
	}

	return s, nil
}

// Match returns the highest-priority script whose rules all match, using
// resolve to look up each match field's value for the candidate host. A
// script with no rules never matches. The input slice is not modified.
//
// If two or more matching scripts TIE at the winning priority, Match returns
// an error rather than silently picking one. Priority is the tiebreaker, so a
// tie is genuinely ambiguous — the previous sort.Slice (not stable) would pick
// non-deterministically, which means a host could get different layouts/configs
// on different snapshot builds. Surfacing it as an error makes the preflight
// reject op creation (and the snapshot build fail loudly) so the operator fixes
// the overlap (raise one script's priority, or tighten its match) instead of
// shipping a coin-flip. This is exactly the condition that arose when a
// chassis-family regex script and an exact-match script for the same model both
// sat at priority 200.
func Match(scripts []Script, resolve func(field string) string) (*Script, error) {
	sorted := make([]Script, len(scripts))
	copy(sorted, scripts)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Priority > sorted[j].Priority
	})

	var winner *Script
	var tied []string
	for i := range sorted {
		s := &sorted[i]
		if len(s.Rules) == 0 {
			continue
		}
		if !matchesAll(s.Rules, resolve) {
			continue
		}
		if winner == nil {
			winner = s
			tied = []string{scriptLabel(s)}
			continue
		}
		// Past the first match: only a SAME-priority match is ambiguous.
		// Lower-priority matches are correctly losing to the winner.
		if s.Priority == winner.Priority {
			tied = append(tied, scriptLabel(s))
			continue
		}
		break // sorted desc; once priority drops below the winner's, stop.
	}
	if winner == nil {
		return nil, nil
	}
	if len(tied) > 1 {
		return nil, fmt.Errorf("ambiguous match: %d scripts match this host at priority %d (%s); priority must break the tie — raise one script's priority or tighten its match rules",
			len(tied), winner.Priority, strings.Join(tied, ", "))
	}
	return winner, nil
}

// scriptLabel names a script for an ambiguity error: the path's base name when
// available (the upload name / filename), else a rule summary.
func scriptLabel(s *Script) string {
	if s.Path != "" {
		return s.Path
	}
	parts := make([]string, 0, len(s.Rules))
	for _, r := range s.Rules {
		parts = append(parts, r.Field+"="+r.Value)
	}
	return "{" + strings.Join(parts, " ") + "}"
}

func matchesAll(rules []MatchRule, resolve func(field string) string) bool {
	for _, rule := range rules {
		if !ruleMatches(rule, resolve(rule.Field)) {
			return false
		}
	}
	return true
}

func ruleMatches(rule MatchRule, value string) bool {
	if rule.pattern != nil {
		return rule.pattern.MatchString(value)
	}
	return rule.Value == value
}

func parsePriority(s string) (int, error) {
	t := strings.TrimSpace(s)
	p, err := strconv.Atoi(t)
	if err != nil {
		return 0, fmt.Errorf("invalid priority %q: %w", t, err)
	}
	return p, nil
}

func sortedFields(fields map[string]struct{}) string {
	names := make([]string, 0, len(fields))
	for f := range fields {
		names = append(names, f)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
