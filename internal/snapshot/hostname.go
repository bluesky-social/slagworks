package snapshot

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/bluesky-social/slagworks/internal/model"
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
// same hostname.
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
	t.suffix = raw[locs[len(locs)-1][1]:]
	return t, nil
}

// String returns the template as configured ("" when unset).
func (t HostnameTemplate) String() string { return t.raw }

// Expand returns the hostname for a NetBox device name. Returns "" when the
// template uses {node} or {zone} and the name has no dot, or nothing on one
// side of its last dot (no zone to place), and when the result is not a
// valid hostname (model.ValidHostname: NetBox device names are free text, and
// a space or newline would corrupt /etc/hostname and /etc/hosts). The agent
// then leaves the deployed image's default hostname rather than writing a
// malformed one. The no-zone check runs before the already-qualified
// passthrough, so it holds even for a name that ends in the template's
// trailing literal.
func (t HostnameTemplate) Expand(name string) string {
	if h := t.expand(name); model.ValidHostname(h) {
		return h
	}
	return ""
}

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
