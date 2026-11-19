package snapshot

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// HostnameTemplate derives a machine's deployed hostname
// (MachineSnapshot.FQDN) from its NetBox device name. Placeholders:
//
//	{name}  the full NetBox device name
//	{node}  the device name before its LAST dot
//	{zone}  the device name after its LAST dot
//
// so "kworker-1.site1" has node "kworker-1" and zone "site1", and
// "foo.bar.site1" has node "foo.bar" and zone "site1". With the template
// "{node}.internal.{zone}.example.net", "kworker-1.site1" becomes
// "kworker-1.internal.site1.example.net".
//
// The zero value is the unset template: the hostname is the device name.
type HostnameTemplate struct {
	raw string
	// splits is set when the template uses {node} or {zone}.
	splits bool
	// suffix is the literal text after the last placeholder. A device name
	// already ending in it is taken as already qualified.
	suffix string
}

var placeholderRE = regexp.MustCompile(`\{[^{}]*\}`)

const allowedPlaceholders = "{name}, {node}, {zone}"

// ParseHostnameTemplate validates a hostname template. Empty (or blank) is
// the unset template. Every {...} must be a known placeholder, and the
// template must use at least one: a constant would give every machine the
// same hostname. Its literal text must not hold a character, an empty label
// or a hyphen-edged label that would keep every expansion from being a valid
// hostname (validHostname).
func ParseHostnameTemplate(raw string) (HostnameTemplate, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return HostnameTemplate{}, nil
	}
	t := HostnameTemplate{raw: raw}
	locs := placeholderRE.FindAllStringIndex(raw, -1)
	for _, loc := range locs {
		switch p := raw[loc[0]:loc[1]]; p {
		case "{name}":
		case "{node}", "{zone}":
			t.splits = true
		default:
			return HostnameTemplate{}, fmt.Errorf("unknown placeholder %q (allowed: %s)", p, allowedPlaceholders)
		}
	}
	if rest := placeholderRE.ReplaceAllString(raw, ""); strings.ContainsAny(rest, "{}") {
		brace := rest[strings.IndexAny(rest, "{}")]
		return HostnameTemplate{}, fmt.Errorf("unmatched %q (allowed placeholders: %s)", string(brace), allowedPlaceholders)
	}
	if len(locs) == 0 {
		return HostnameTemplate{}, errors.New("no placeholder (use at least one of " + allowedPlaceholders + ")")
	}
	// Reject literal text that no device name could turn into a hostname,
	// where Expand would silently give every machine "". With each
	// placeholder as the shortest label, "a", the template must already
	// have a hostname's characters and shape. Label length is left to
	// Expand: {name} and {node} may hold dots that split a long literal
	// label in two.
	if sample := placeholderRE.ReplaceAllString(raw, "a"); !hostnameShape(sample, 253) {
		return HostnameTemplate{}, fmt.Errorf("with each placeholder as \"a\" it gives %q, which no device name can make a valid hostname (use only letters, digits, hyphens and dots, with no empty label and no label starting or ending with a hyphen)", sample)
	}
	t.suffix = raw[locs[len(locs)-1][1]:]
	return t, nil
}

// String returns the template as configured ("" when unset).
func (t HostnameTemplate) String() string { return t.raw }

// Expand returns the hostname for a NetBox device name, or "" when there is
// none to give:
//
//   - the template uses {node} or {zone} and the name has no dot, or nothing
//     on one side of its last dot (no zone to place); or
//   - the result is not a valid hostname (validHostname), as when the device
//     name holds a space, a '/', an '_' or a newline.
//
// On "" the agent leaves the deployed image's default hostname rather than
// writing a malformed one; it writes a non-empty result verbatim into
// /etc/hostname and the 127.0.1.1 line of /etc/hosts. Both checks also cover
// a name that ends in the template's trailing literal and so is passed
// through unexpanded.
func (t HostnameTemplate) Expand(name string) string {
	if h := t.expand(name); validHostname(h) {
		return h
	}
	return ""
}

// validHostname reports whether s is a hostname as RFC 1123 defines one:
// dot-separated labels of 1 to 63 ASCII letters, digits and hyphens, none
// starting or ending with a hyphen, and at most 253 characters in all. These
// are also the only characters systemd takes literally when it reads
// /etc/hostname (it drops the rest, bar its own '?' and '$' wildcards), so a
// name outside them would run as a different hostname from the one the agent
// puts in /etc/hosts.
func validHostname(s string) bool { return hostnameShape(s, 63) }

// hostnameShape is validHostname with labels of up to maxLabel characters.
func hostnameShape(s string, maxLabel int) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if label == "" || len(label) > maxLabel || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			switch {
			case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-':
			default:
				return false
			}
		}
	}
	return true
}

// expand is Expand without the validity check on its result.
func (t HostnameTemplate) expand(name string) string {
	name = strings.TrimSpace(name)
	if t.raw == "" || name == "" {
		return name
	}
	var node, zone string
	if t.splits {
		i := strings.LastIndex(name, ".")
		if i <= 0 || i == len(name)-1 {
			return ""
		}
		node, zone = name[:i], name[i+1:]
	}
	// Already qualified: a name that already ends the way an expansion
	// would must not be expanded twice.
	if t.suffix != "" && strings.HasSuffix(name, t.suffix) {
		return name
	}
	return strings.NewReplacer("{name}", name, "{node}", node, "{zone}", zone).Replace(t.raw)
}
