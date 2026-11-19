package snapshot

import (
	"strings"
	"testing"
)

// internalZoneTemplate is the scheme of a fleet whose NetBox device names
// are "<node>.<zone>" and whose hosts live in a per-zone internal DNS zone.
// It reproduces the expansion global hardcoded before the hostname became
// configuration (with a neutral domain).
const internalZoneTemplate = "{node}.internal.{zone}.example.net"

func mustParseHostnameTemplate(t *testing.T, s string) HostnameTemplate {
	t.Helper()
	tmpl, err := ParseHostnameTemplate(s)
	if err != nil {
		t.Fatalf("ParseHostnameTemplate(%q): %v", s, err)
	}
	return tmpl
}

func assertExpansions(t *testing.T, tmpl HostnameTemplate, cases map[string]string) {
	t.Helper()
	for name, want := range cases {
		if got := tmpl.Expand(name); got != want {
			t.Errorf("template %q: Expand(%q) = %q, want %q", tmpl, name, got, want)
		}
	}
}

// The old built-in expansion's test table, run through the template that
// reproduces it.
func TestHostnameTemplate_InternalZoneScheme(t *testing.T) {
	assertExpansions(t, mustParseHostnameTemplate(t, internalZoneTemplate), map[string]string{
		"oob-gw-1.pop1":      "oob-gw-1.internal.pop1.example.net",
		"kworker-l34-1.pop1": "kworker-l34-1.internal.pop1.example.net",
		"leaf-3.pop2":        "leaf-3.internal.pop2.example.net",
		// node segment with dots keeps everything before the last dot.
		"foo.bar.pop1": "foo.bar.internal.pop1.example.net",
		// already qualified (ends with the template's trailing literal):
		// not double-expanded.
		"oob-gw-1.internal.pop1.example.net": "oob-gw-1.internal.pop1.example.net",
		// no dot, or an empty side of the last dot: no zone to place, so
		// empty (the agent leaves the image's default hostname).
		"justaname": "",
		"":          "",
		"foo.":      "",
		".pop1":     "",
		// surrounding whitespace is not part of the name.
		"  leaf-3.pop2 ": "leaf-3.internal.pop2.example.net",
	})
}

// No template configured: the hostname is the NetBox device name.
func TestHostnameTemplate_Unset(t *testing.T) {
	cases := map[string]string{
		"kworker-l34-1.pop1": "kworker-l34-1.pop1",
		"justaname":          "justaname",
		"foo.bar.pop1":       "foo.bar.pop1",
		" leaf-3.pop2 ":      "leaf-3.pop2",
		"":                   "",
	}
	assertExpansions(t, HostnameTemplate{}, cases)
	for _, raw := range []string{"", "   "} {
		assertExpansions(t, mustParseHostnameTemplate(t, raw), cases)
	}
}

// NetBox stores a device name as free text. A name that does not make a
// valid hostname gives none (the agent keeps the image's default) instead of
// a malformed /etc/hostname, and instead of a newline that would add lines
// to the deployed host's /etc/hosts.
func TestHostnameTemplate_NotAHostname(t *testing.T) {
	assertExpansions(t, HostnameTemplate{}, map[string]string{
		"web1\n203.0.113.9 registry.example.net": "",
		"web 1.pop1":                             "",
		"web_1.pop1":                             "",
		"web1..pop1":                             "",
		"-web1.pop1":                             "",
	})
	assertExpansions(t, mustParseHostnameTemplate(t, internalZoneTemplate), map[string]string{
		"web1\n203.0.113.9 registry.pop1": "",
		"web 1.pop1":                      "",
		"web1.po p1":                      "",
		// Already qualified: passed through, so still checked.
		"web 1.internal.pop1.example.net": "",
	})
}

func TestHostnameTemplate_NamePlaceholder(t *testing.T) {
	assertExpansions(t, mustParseHostnameTemplate(t, "{name}.example.net"), map[string]string{
		// {name} needs no dot: every non-empty name expands.
		"justaname":          "justaname.example.net",
		"kworker-l34-1.pop1": "kworker-l34-1.pop1.example.net",
		// already qualified.
		"host.example.net": "host.example.net",
		"":                 "",
	})
	assertExpansions(t, mustParseHostnameTemplate(t, "{name}"), map[string]string{
		"kworker-l34-1.pop1": "kworker-l34-1.pop1",
		"justaname":          "justaname",
	})
}

// A template ending in a placeholder has no trailing literal, so nothing
// counts as already qualified and every name is expanded.
func TestHostnameTemplate_NoTrailingLiteral(t *testing.T) {
	assertExpansions(t, mustParseHostnameTemplate(t, "{zone}-{node}"), map[string]string{
		"kworker-1.site1": "site1-kworker-1",
		"foo.bar.site1":   "site1-foo.bar",
		"justaname":       "",
	})
	assertExpansions(t, mustParseHostnameTemplate(t, "{node}.{zone}.{name}"), map[string]string{
		"a.b": "a.b.a.b",
	})
}

func TestParseHostnameTemplate_Invalid(t *testing.T) {
	cases := map[string]string{
		"{host}.example.net":   `unknown placeholder "{host}"`,
		"{node}.{Zone}":        `unknown placeholder "{Zone}"`,
		"{}.example.net":       `unknown placeholder "{}"`,
		"{node}.{zone}.{{x}}.": `unknown placeholder "{x}"`,
		"host.example.net":     "no placeholder",
		"{node.example.net":    `unmatched "{"`,
		"{node}}.example.net":  `unmatched "}"`,
	}
	for raw, wantInErr := range cases {
		_, err := ParseHostnameTemplate(raw)
		if err == nil {
			t.Errorf("ParseHostnameTemplate(%q): expected an error", raw)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, wantInErr) {
			t.Errorf("ParseHostnameTemplate(%q) error %q does not name the problem %q", raw, msg, wantInErr)
		}
		for _, allowed := range []string{"{name}", "{node}", "{zone}"} {
			if !strings.Contains(msg, allowed) {
				t.Errorf("ParseHostnameTemplate(%q) error %q does not list allowed placeholder %s", raw, msg, allowed)
			}
		}
	}
}
