package snapshot

import (
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// Property-based coverage for HostnameTemplate.Expand.
//
// Oracles:
//   - an independent reference expansion written from
//     docs/user/configuration.md (SLAGWORKS_HOSTNAME_TEMPLATE);
//   - validity: the result is "" or a valid RFC 1123 hostname (labels
//     1..63 of [a-z0-9-], no leading/trailing hyphen, total <= 253);
//   - stability: expanding is a pure function of (template, trimmed name);
//   - a device name carrying characters no hostname may contain
//     (whitespace, newline, '/', '#') expands to "" and so never reaches
//     MachineSnapshot.FQDN, which the agent writes verbatim into
//     /etc/hostname and /etc/hosts.

var labelChars = []rune("abcdefghijklmnopqrstuvwxyz0123456789-")

func genLabel() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		n := rapid.IntRange(1, 8).Draw(t, "labelLen")
		rs := make([]rune, n)
		for i := range rs {
			rs[i] = rapid.SampledFrom(labelChars).Draw(t, "ch")
		}
		if rs[0] == '-' {
			rs[0] = 'a'
		}
		if rs[n-1] == '-' {
			rs[n-1] = 'z'
		}
		return string(rs)
	})
}

func genValidName() *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		labels := rapid.SliceOfN(genLabel(), 1, 4).Draw(t, "labels")
		return strings.Join(labels, ".")
	})
}

var validTemplates = []string{
	"{name}",
	"{node}.{zone}",
	"{node}.internal.{zone}.example.net",
	"{name}.example.net",
	"{node}-{zone}",
	"host-{name}",
	"{zone}.{node}.example.net",
}

func validHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// refExpand mirrors the documented rules, not the implementation: the
// substituted template, or "" when that is not a valid hostname.
func refExpand(tmpl, name string) string {
	if h := refSubstitute(tmpl, name); validHostname(h) {
		return h
	}
	return ""
}

func refSubstitute(tmpl, name string) string {
	name = strings.TrimSpace(name)
	if tmpl == "" || name == "" {
		return name
	}
	usesSplit := strings.Contains(tmpl, "{node}") || strings.Contains(tmpl, "{zone}")
	node, zone := "", ""
	if usesSplit {
		i := strings.LastIndexByte(name, '.')
		if i < 1 || i == len(name)-1 {
			return ""
		}
		node, zone = name[:i], name[i+1:]
	}
	last := max(strings.LastIndex(tmpl, "{name}")+len("{name}"), strings.LastIndex(tmpl, "{node}")+len("{node}"), strings.LastIndex(tmpl, "{zone}")+len("{zone}"))
	trailing := tmpl[last:]
	if trailing != "" && strings.HasSuffix(name, trailing) {
		return name
	}
	var b strings.Builder
	for i := 0; i < len(tmpl); {
		switch {
		case strings.HasPrefix(tmpl[i:], "{name}"):
			b.WriteString(name)
			i += 6
		case strings.HasPrefix(tmpl[i:], "{node}"):
			b.WriteString(node)
			i += 6
		case strings.HasPrefix(tmpl[i:], "{zone}"):
			b.WriteString(zone)
			i += 6
		default:
			b.WriteByte(tmpl[i])
			i++
		}
	}
	return b.String()
}

func TestProp_HostnameExpand(t *testing.T) {
	var expanded, passthrough, empty int
	rapid.Check(t, func(t *rapid.T) {
		raw := rapid.SampledFrom(validTemplates).Draw(t, "tmpl")
		tmpl, err := ParseHostnameTemplate(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		name := genValidName().Draw(t, "name")
		pad := rapid.SampledFrom([]string{"", " ", "\t", " \n"}).Draw(t, "pad")
		got := tmpl.Expand(pad + name + pad)
		if want := refExpand(raw, pad+name+pad); got != want {
			t.Fatalf("Expand(%q) with %q = %q, want %q", name, raw, got, want)
		}
		if got != "" && !validHostname(got) {
			t.Fatalf("Expand(%q) with %q = %q: not a valid hostname", name, raw, got)
		}
		if got2 := tmpl.Expand(name); got2 != got {
			t.Fatalf("not stable under surrounding whitespace: %q vs %q", got, got2)
		}
		switch {
		case got == "":
			empty++
		case got == name:
			passthrough++
		default:
			expanded++
		}
	})
	t.Logf("coverage: expanded=%d passthrough=%d empty=%d", expanded, passthrough, empty)
}

func TestProp_HostnameRejectsUnsafeNames(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		raw := rapid.SampledFrom(append([]string{""}, validTemplates...)).Draw(t, "tmpl")
		tmpl, err := ParseHostnameTemplate(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		a := genValidName().Draw(t, "a")
		b := genValidName().Draw(t, "b")
		bad := rapid.SampledFrom([]string{"\n", " ", "/", "#", "\t"}).Draw(t, "bad")
		name := a + bad + b
		if got := tmpl.Expand(name); got != "" {
			t.Fatalf("Expand(%q) with template %q = %q, want \"\": the name holds a character no hostname may contain", name, raw, got)
		}
	})
}
