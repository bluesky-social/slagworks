package snapshot

import (
	"testing"

	"github.com/bluesky-social/slagworks/internal/configscript"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// These tests exercise the geographic match fields added to the netscript
// matcher (location/rack/u) plus the existing site/device_type/hostname keys,
// against matchNetworkScript directly. They are the field-level Phase 0 tests;
// the cross-check against the *real* shipped scripts is the Phase
// 1.4 collective test.

// parseNet builds a parsed network script from header lines + a trivial body,
// going through the same ParseNetworkScript path the build uses (so an unknown
// match field fails here exactly as it would at upload time).
func parseNet(t *testing.T, name string, headerLines ...string) configscript.Script {
	t.Helper()
	body := "#!/usr/bin/env python3\n"
	for _, h := range headerLines {
		body += "# " + h + "\n"
	}
	body += "print('{}')\n"
	s, err := ParseNetworkScript(name, []byte(body))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return *s
}

//go:fix inline

// matchedName runs a device through matchNetworkScript over the given scripts
// and returns the matched script name, or "" for no match.
func matchedName(scripts []configscript.Script, dev netbox.Device) string {
	got := matchNetworkScript(scripts, dev)
	if got == nil {
		return ""
	}
	return got.Name
}

// TestNetscriptLocationMatch is the load-bearing case the location field was
// added for: two devices in the SAME site but DIFFERENT locations (pop2's two
// cages) must select different scripts — something site alone can't do.
func TestNetscriptLocationMatch(t *testing.T) {
	oldCage := parseNet(t, "pop2-oldcage",
		"slagworks:match site=pop2",
		"slagworks:match location=pop2-01-005330",
		"slagworks:priority 100")
	newCage := parseNet(t, "pop2-newcage",
		"slagworks:match site=pop2",
		"slagworks:match location=pop2-01-005250",
		"slagworks:priority 100")
	scripts := []configscript.Script{oldCage, newCage}

	dev := func(loc string) netbox.Device {
		return netbox.Device{
			Name:     "host.pop2",
			Site:     &netbox.NestedRef{Slug: "pop2"},
			Location: &netbox.NestedRef{Slug: loc},
		}
	}

	if got := matchedName(scripts, dev("pop2-01-005330")); got != "pop2-oldcage" {
		t.Errorf("old-cage device matched %q, want pop2-oldcage", got)
	}
	if got := matchedName(scripts, dev("pop2-01-005250")); got != "pop2-newcage" {
		t.Errorf("new-cage device matched %q, want pop2-newcage", got)
	}
	// A pop2 device in neither cage matches neither script.
	if got := matchedName(scripts, dev("pop2-01-009999")); got != "" {
		t.Errorf("device in an unmodeled location matched %q, want no match", got)
	}
	// A device with no location set never matches a location= rule.
	noLoc := netbox.Device{Name: "host.pop2", Site: &netbox.NestedRef{Slug: "pop2"}}
	if got := matchedName(scripts, noLoc); got != "" {
		t.Errorf("device with nil location matched %q, want no match", got)
	}
}

// TestNetscriptRackMatch: a rack key splits hosts within one location. Racks
// are matched by NAME (NetBox leaves rack slugs empty on the device serializer).
func TestNetscriptRackMatch(t *testing.T) {
	rackA := parseNet(t, "rack-a",
		"slagworks:match location=loc1",
		"slagworks:match rack=0205",
		"slagworks:priority 100")
	rackB := parseNet(t, "rack-b",
		"slagworks:match location=loc1",
		"slagworks:match rack=0206",
		"slagworks:priority 100")
	scripts := []configscript.Script{rackA, rackB}

	dev := func(rack string) netbox.Device {
		return netbox.Device{
			Name:     "host",
			Location: &netbox.NestedRef{Slug: "loc1"},
			Rack:     &netbox.NestedRef{Name: rack},
		}
	}
	if got := matchedName(scripts, dev("0205")); got != "rack-a" {
		t.Errorf("rack 0205 matched %q, want rack-a", got)
	}
	if got := matchedName(scripts, dev("0206")); got != "rack-b" {
		t.Errorf("rack 0206 matched %q, want rack-b", got)
	}
	// nil rack never matches a rack= rule.
	noRack := netbox.Device{Name: "host", Location: &netbox.NestedRef{Slug: "loc1"}}
	if got := matchedName(scripts, noRack); got != "" {
		t.Errorf("device with nil rack matched %q, want no match", got)
	}
}

// TestNetscriptUMatch pins the U-position resolver: whole-U slots format
// without a trailing ".0", half-U slots round-trip, and a nil position never
// matches.
func TestNetscriptUMatch(t *testing.T) {
	whole := parseNet(t, "u-whole",
		"slagworks:match u=12",
		"slagworks:priority 100")
	half := parseNet(t, "u-half",
		"slagworks:match u=12.5",
		"slagworks:priority 100")
	scripts := []configscript.Script{whole, half}

	// Position 12.0 must match "u=12" (minimal float formatting), NOT "u=12.0".
	if got := matchedName(scripts, netbox.Device{Position: new(12.0)}); got != "u-whole" {
		t.Errorf("position 12.0 matched %q, want u-whole (must format as \"12\")", got)
	}
	// Half-U 12.5 round-trips.
	if got := matchedName(scripts, netbox.Device{Position: new(12.5)}); got != "u-half" {
		t.Errorf("position 12.5 matched %q, want u-half", got)
	}
	// A different U matches neither.
	if got := matchedName(scripts, netbox.Device{Position: new(float64(13))}); got != "" {
		t.Errorf("position 13 matched %q, want no match", got)
	}
	// nil position never matches a u= rule.
	if got := matchedName(scripts, netbox.Device{}); got != "" {
		t.Errorf("nil position matched %q, want no match", got)
	}
}

// TestNetscriptANDedGeoPrecision confirms a script can pin several geographic
// levels at once (site+location+rack+u), and that a device matches only when
// ALL of them hold — the AND semantics that make finer precision a narrowing.
func TestNetscriptANDedGeoPrecision(t *testing.T) {
	precise := parseNet(t, "precise",
		"slagworks:match site=pop2",
		"slagworks:match location=loc1",
		"slagworks:match rack=0205",
		"slagworks:match u=20",
		"slagworks:priority 100")
	scripts := []configscript.Script{precise}

	full := netbox.Device{
		Site:     &netbox.NestedRef{Slug: "pop2"},
		Location: &netbox.NestedRef{Slug: "loc1"},
		Rack:     &netbox.NestedRef{Name: "0205"},
		Position: new(float64(20)),
	}
	if got := matchedName(scripts, full); got != "precise" {
		t.Errorf("fully-matching device matched %q, want precise", got)
	}
	// Wrong U alone breaks the whole match (AND).
	wrongU := full
	wrongU.Position = new(float64(21))
	if got := matchedName(scripts, wrongU); got != "" {
		t.Errorf("device off by U matched %q, want no match (AND semantics)", got)
	}
}

// TestNetscriptUnknownFieldRejectedAtParse keeps the existing loud-fail
// behavior: a typo'd / unsupported match field is an upload-time error, not a
// silently-never-matching script.
func TestNetscriptUnknownFieldRejectedAtParse(t *testing.T) {
	body := "#!/usr/bin/env python3\n# slagworks:match cage=newcage\n# slagworks:priority 100\nprint('{}')\n"
	if _, err := ParseNetworkScript("bad", []byte(body)); err == nil {
		t.Fatal("expected parse error for unknown match field 'cage', got nil")
	}
}
