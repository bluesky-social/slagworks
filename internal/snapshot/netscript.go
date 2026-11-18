package snapshot

import (
	"log/slog"
	"strconv"

	"github.com/bluesky-social/slagworks/internal/configscript"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// This file selects the per-machine host network-config script. It is the
// network analog of the storage-script match in builder.go, but matches on
// NetBox *identity* (where the host sits in the fabric) rather than DMI
// commission fields (what the hardware is). The grammar and selection are
// the shared configscript engine; only the field vocabulary and the
// per-host resolver differ.
//
// See docs/dev/extensibility.md ("Netscript contract").

// NetworkScriptFields are the NetBox-identity fields a network-config
// script may match on. They form a geographic hierarchy (site ⊃ location ⊃
// rack ⊃ U) plus two non-geographic keys; a script ANDs whatever precision
// its cohort needs:
//
//   - site:        the pop (coarsest geographic key).
//   - location:    a sub-site / cage within a site. This is how pop2's two
//     physically distinct cages (old/new) are told apart under one NetBox
//     site — the case site alone can't resolve.
//   - rack:        a rack within a location (NetBox rack name, e.g. "0205").
//   - u:           the device's U-position within its rack (finest key).
//   - device_type: the chassis model (topology-family key where geography
//     can't split co-resident families — e.g. pop1's L3 kworkers vs L2
//     legacy hosts at one location).
//   - hostname:    a regex over the device name. Brittle as a SOLE/primary
//     key (a rename silently breaks it), so it's used as an additional
//     defense-in-depth AND-constraint on top of a fact key, not alone.
//
// Exported so the API server validates uploads against the same vocabulary
// the matcher uses.
var NetworkScriptFields = map[string]struct{}{
	"site":        {},
	"location":    {},
	"rack":        {},
	"u":           {},
	"device_type": {},
	"hostname":    {},
}

// ParseNetworkScript parses a network-config script's headers against the
// network field vocabulary. The API server uses it to validate + extract
// priority at upload time, mirroring storage.ParseScript.
func ParseNetworkScript(name string, content []byte) (*configscript.Script, error) {
	return configscript.Parse(name, content, NetworkScriptFields)
}

// parseNetworkScripts parses the raw network scripts from the global store
// once per snapshot build. An unparseable script is logged and excluded —
// one bad script must not break snapshot build for a whole site, matching
// the storage-script posture in Build.
func parseNetworkScripts(scripts []model.NetworkScript) []configscript.Script {
	parsed := make([]configscript.Script, 0, len(scripts))
	for _, s := range scripts {
		p, err := configscript.Parse(s.Name, []byte(s.Content), NetworkScriptFields)
		if err != nil {
			slog.Warn("network script has unparseable headers, skipping",
				"name", s.Name, "err", err)
			continue
		}
		parsed = append(parsed, *p)
	}
	return parsed
}

// matchNetworkScript runs a device's NetBox identity through the parsed
// scripts and returns the snapshot-wire-shape script, or nil if no script
// matched (agent falls back to its built-in DHCP netplan). The resolver
// reads site/location/rack/U/device_type and the device name off the Device
// record Build already holds — all are populated by the default NetBox device
// serializer, so zero new NetBox queries.
func matchNetworkScript(scripts []configscript.Script, dev netbox.Device) *model.SnapshotNetworkScript {
	if len(scripts) == 0 {
		return nil
	}
	resolve := func(field string) string {
		switch field {
		case "site":
			if dev.Site != nil {
				return dev.Site.Slug
			}
		case "location":
			if dev.Location != nil {
				return dev.Location.Slug
			}
		case "rack":
			// Racks are identified by name (e.g. "0205"), not a slug —
			// NestedRef.Slug is empty for racks in NetBox's device serializer.
			if dev.Rack != nil {
				return dev.Rack.Name
			}
		case "u":
			// The device's U-position. Format with the minimal decimal
			// representation so a whole-U slot 12.0 matches "u=12" while a
			// half-U slot 12.5 round-trips as "u=12.5". A device with no
			// position (nil) resolves to "" and so never matches a u= rule.
			if dev.Position != nil {
				return strconv.FormatFloat(*dev.Position, 'f', -1, 64)
			}
		case "device_type":
			if dev.DeviceType != nil {
				return dev.DeviceType.Slug
			}
		case "hostname":
			return dev.Name
		}
		return ""
	}
	matched, err := configscript.Match(scripts, resolve)
	if err != nil || matched == nil {
		return nil
	}
	return &model.SnapshotNetworkScript{
		Name:    matched.Path,
		Content: string(matched.Content),
	}
}
