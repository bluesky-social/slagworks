package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/snapshot"
)

type DiffMissing string

const (
	MissingNetBox    DiffMissing = "netbox"
	MissingSlagworks DiffMissing = "slagworks"

	diffNone        = "(none)"
	diffUnavailable = "(unavailable)"
	diffNetBoxSSoT  = "(not stored; NetBox is source of truth)"
	diffNotInNetBox = "(not stored in NetBox)"
)

// NetBoxDiff is the server-produced comparison used by slagctl. Only Fields
// with Comparable=true contribute to Matched. The remaining rows are useful
// one-sided context and deliberately do not pretend that Slagworks has an
// independent copy of NetBox-owned identity.
type NetBoxDiff struct {
	MachineID string      `json:"machine_id"`
	Missing   DiffMissing `json:"missing,omitempty"`
	Message   string      `json:"message,omitempty"`
	Matched   bool        `json:"matched"`
	Fields    []DiffField `json:"fields"`
}

type DiffField struct {
	Name       string `json:"name"`
	Slagworks  string `json:"slagworks"`
	NetBox     string `json:"netbox"`
	Comparable bool   `json:"comparable"`
	Match      *bool  `json:"match,omitempty"`
}

// netboxDiffMachine compares independently persisted bindings. Hostname,
// BMC IP, interface MACs, and site are NetBox-owned and appear only as
// context; rebuilding a snapshot from live NetBox and comparing it back to
// NetBox would manufacture a meaningless always-matching "diff".
func (s *Server) netboxDiffMachine(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "machine id must be a UUID")
	}
	if s.netbox == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"netbox diff requires NetBox integration")
	}

	ctx := c.Request().Context()
	machine, err := s.store.GetMachine(id)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		machine = nil
	}

	// Resolve by NetBox's slagworks_id first. If that custom field has not
	// been stamped yet, fall back to the device ID persisted in Slagworks.
	// This ordering exposes a stale/recreated binding instead of hiding it by
	// fetching only the device Slagworks already points at.
	device, err := s.netbox.FindDeviceBySlagworksID(ctx, id.String())
	if err != nil {
		return echo.NewHTTPError(http.StatusBadGateway,
			fmt.Sprintf("netbox lookup by slagworks_id: %v", err))
	}
	if device == nil && machine != nil && machine.NetBoxDeviceID != nil {
		device, err = s.netbox.GetDevice(ctx, *machine.NetBoxDeviceID)
		if err != nil {
			var apiErr *netbox.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
				return echo.NewHTTPError(http.StatusBadGateway,
					fmt.Sprintf("netbox get device %d: %v", *machine.NetBoxDeviceID, err))
			}
			device = nil
		}
	}

	if machine == nil && device == nil {
		return echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("machine %s exists in neither Slagworks nor NetBox", id))
	}

	var missing DiffMissing
	switch {
	case machine == nil:
		missing = MissingSlagworks
	case device == nil:
		missing = MissingNetBox
	}

	var ifaces []netbox.Interface
	if device != nil {
		ifaces, err = s.netbox.ListInterfacesForDevices(ctx, []int{device.ID})
		if err != nil {
			return echo.NewHTTPError(http.StatusBadGateway,
				fmt.Sprintf("netbox list interfaces for device %d: %v", device.ID, err))
		}
	}

	snapshotSeq := s.diffSnapshotSeq(ctx, device)
	return c.JSON(http.StatusOK, buildNetBoxDiff(id, machine, device, ifaces, snapshotSeq, missing))
}

// diffSnapshotSeq is best-effort context. Failure to map a device to a
// worker config must not hide the identity comparison operators asked for.
func (s *Server) diffSnapshotSeq(ctx context.Context, device *netbox.Device) string {
	if device == nil {
		return diffUnavailable
	}
	slug, err := snapshot.ResolveOwningConfigSlug(ctx, s.netbox, s.store, *device)
	if err != nil {
		return diffUnavailable
	}
	cfg, err := s.store.GetSiteConfig(slug)
	if err != nil {
		return diffUnavailable
	}
	return strconv.FormatInt(cfg.LastSnapshotSeq, 10)
}

func buildNetBoxDiff(id uuid.UUID, machine *model.Machine, device *netbox.Device, ifaces []netbox.Interface, snapshotSeq string, missing DiffMissing) NetBoxDiff {
	res := NetBoxDiff{MachineID: id.String(), Missing: missing}
	switch missing {
	case MissingNetBox:
		res.Message = "machine exists in Slagworks but no corresponding NetBox device was found"
	case MissingSlagworks:
		res.Message = "NetBox device exists but the Slagworks machine row is missing"
	}

	fields := make([]DiffField, 0, 11)
	compare := func(name, sw, nb string) {
		matched := sw == nb
		fields = append(fields, DiffField{
			Name: name, Slagworks: sw, NetBox: nb,
			Comparable: true, Match: &matched,
		})
	}
	contextField := func(name, sw, nb string) {
		fields = append(fields, DiffField{Name: name, Slagworks: sw, NetBox: nb})
	}

	swID, swDeviceID, swSerial := diffUnavailable, diffUnavailable, diffUnavailable
	if machine != nil {
		swID = machine.ID.String()
		swDeviceID = diffNone
		if machine.NetBoxDeviceID != nil {
			swDeviceID = strconv.Itoa(*machine.NetBoxDeviceID)
		}
		swSerial = renderDiffValue(machine.NetBoxSerial)
	}

	nbSlagworksID, nbDeviceID, nbSerial := diffUnavailable, diffUnavailable, diffUnavailable
	if device != nil {
		nbSlagworksID = renderDiffValue(deviceSlagworksID(device))
		nbDeviceID = strconv.Itoa(device.ID)
		nbSerial = renderDiffValue(device.Serial)
	}
	compare("slagworks_id", swID, nbSlagworksID)
	compare("netbox_device_id", swDeviceID, nbDeviceID)
	compare("serial", swSerial, nbSerial)

	nbHostname, nbSite, nbBMC, nbMACs := diffUnavailable, diffUnavailable, diffUnavailable, diffUnavailable
	nbStatus := diffUnavailable
	if device != nil {
		nbHostname = renderDiffValue(device.Name)
		if device.Site != nil {
			nbSite = renderDiffValue(device.Site.Slug)
		} else {
			nbSite = diffNone
		}
		if device.OOBIP != nil {
			nbBMC = renderDiffValue(stripCIDR(device.OOBIP.Address))
		} else {
			nbBMC = diffNone
		}
		nbMACs = renderDiffValue(strings.Join(netboxMACs(ifaces), ", "))
		if device.Status != nil {
			nbStatus = renderDiffValue(device.Status.Value)
		} else {
			nbStatus = diffNone
		}
	}
	contextField("hostname", diffNetBoxSSoT, nbHostname)
	contextField("site", diffNetBoxSSoT, nbSite)
	contextField("bmc_ip", diffNetBoxSSoT, nbBMC)
	contextField("macs", diffNetBoxSSoT, nbMACs)

	swState, commissionedAt := diffUnavailable, diffUnavailable
	if machine != nil {
		swState = renderDiffValue(string(machine.State))
		commissionedAt = diffNone
		if machine.CommissionedAt != nil {
			commissionedAt = machine.CommissionedAt.UTC().Format(time.RFC3339)
		}
	}
	contextField("slagworks_state", swState, diffNotInNetBox)
	contextField("netbox_status", diffNetBoxSSoT, nbStatus)
	contextField("commissioned_at", commissionedAt, diffNotInNetBox)
	contextField("snapshot_seq", snapshotSeq, diffNotInNetBox)

	res.Fields = fields
	res.Matched = missing == ""
	for _, field := range fields {
		if field.Comparable && (field.Match == nil || !*field.Match) {
			res.Matched = false
		}
	}
	return res
}

func deviceSlagworksID(device *netbox.Device) string {
	if device == nil || len(device.CustomFields) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(device.CustomFields, &fields); err != nil {
		return ""
	}
	var id string
	if err := json.Unmarshal(fields["slagworks_id"], &id); err != nil {
		return ""
	}
	return id
}

func renderDiffValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return diffNone
	}
	return value
}

func stripCIDR(address string) string {
	return strings.SplitN(strings.TrimSpace(address), "/", 2)[0]
}

func netboxMACs(ifaces []netbox.Interface) []string {
	seen := make(map[string]struct{})
	for _, iface := range ifaces {
		mac := strings.ToLower(strings.TrimSpace(iface.GetMAC()))
		if mac != "" {
			seen[mac] = struct{}{}
		}
	}
	macs := make([]string, 0, len(seen))
	for mac := range seen {
		macs = append(macs, mac)
	}
	sort.Strings(macs)
	return macs
}
