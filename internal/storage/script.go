package storage

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bluesky-social/slagworks/internal/configscript"
)

// Script and MatchRule are re-exported from configscript so existing storage
// callers keep their type names; the rule grammar and selection live in the
// shared engine. Storage adds only its field vocabulary, the DMI resolver, and
// the kernel_params rejection.
type Script = configscript.Script

// validFields are the fields a storage script may match on. Two kinds:
//
//   - DMI/commission fields (system_manufacturer, system_product) — what the
//     hardware physically IS, discovered by probing. These are the PRIMARY key:
//     a storage layout is chosen for a chassis model.
//   - NetBox-identity fields (hostname) — WHO the host is. Storage layout is
//     model-driven, but the chassis model is NOT role-unique: e.g. the
//     Supermicro AS-Tx TNR chassis is shared by kworker/hot-spare (which we
//     image) AND nova/algo/relay/router/etc (which we do NOT). device_type and
//     role don't separate them either (all role=server, models overlap) — only
//     the hostname does. So a storage script can AND a hostname constraint as a
//     defense-in-depth scope, exactly like the network scripts: it bounds WHICH
//     of the model-sharing hosts the script is allowed to claim. The hostname
//     is sourced from the NetBox device name by the snapshot builder (the same
//     value the network matcher uses), NOT from unreliable commission-time
//     os.Hostname().
var validFields = map[string]struct{}{
	"system_manufacturer": {},
	"system_product":      {},
	"hostname":            {},
}

// ParseScript parses the slagworks metadata headers from a storage script.
// It wraps configscript.Parse with storage's field vocabulary and rejects the
// retired slagworks:kernel_params header before delegating.
func ParseScript(path string, content []byte) (*Script, error) {
	if err := rejectKernelParams(path, content); err != nil {
		return nil, err
	}
	return configscript.Parse(path, content, validFields)
}

// rejectKernelParams fails any storage script still carrying the retired
// slagworks:kernel_params header. It scans the same top-of-file comment block
// configscript.Parse does and stops at the first non-comment line.
//
// kernel_params used to live on storage scripts as a per-hardware
// "console=ttyS1,..." override. That coupled two unrelated concerns (storage
// layout and serial console shape) and silently broke when the hardcoded
// trailer in httpboot won the cmdline last-token race. Kernel params now come
// from the NetBox device-type custom field; reject any storage script still
// carrying the header so we notice and migrate it.
func rejectKernelParams(path string, content []byte) error {
	for line := range strings.SplitSeq(string(content), "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(line, "\r"))
		if trimmed == "" || strings.HasPrefix(trimmed, "#!") {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			break
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
		if strings.HasPrefix(rest, "slagworks:kernel_params") {
			return fmt.Errorf("%s: slagworks:kernel_params is no longer accepted on storage scripts; set NetBox device_type.custom_fields.slagworks_kernel_params instead", path)
		}
	}
	return nil
}

// commissionSummary holds the commission data fields used for script matching.
type commissionSummary struct {
	SystemManufacturer string
	SystemProduct      string
}

func extractMatchFields(data json.RawMessage) (commissionSummary, error) {
	var parsed struct {
		System struct {
			Manufacturer string `json:"manufacturer"`
			ProductName  string `json:"product_name"`
		} `json:"system"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return commissionSummary{}, fmt.Errorf("parse commission data: %w", err)
	}
	return commissionSummary{
		SystemManufacturer: parsed.System.Manufacturer,
		SystemProduct:      parsed.System.ProductName,
	}, nil
}

// Match returns the highest-priority script whose rules all match. The DMI
// fields (system_product/system_manufacturer) come from commissionData; the
// identity fields (hostname) come from the identity map the caller supplies
// (the snapshot builder fills it from the NetBox device — storage stays
// netbox-free). A nil/empty identity map means an identity field resolves to ""
// and so never matches a rule referencing it. Returns nil if no script matches;
// scripts with no rules never match. The input slice is not modified.
func Match(scripts []Script, commissionData json.RawMessage, identity map[string]string) (*Script, error) {
	summary, err := extractMatchFields(commissionData)
	if err != nil {
		return nil, err
	}
	resolve := func(field string) string {
		switch field {
		case "system_product":
			return summary.SystemProduct
		case "system_manufacturer":
			return summary.SystemManufacturer
		default:
			// hostname (and any future identity field) comes from the
			// caller-supplied map.
			return identity[field]
		}
	}
	return configscript.Match(scripts, resolve)
}
