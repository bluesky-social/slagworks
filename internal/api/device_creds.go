package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// DeviceCreds is the response shape for GET /api/v1/devices/:ident/creds.
// It is a generic, kind-agnostic proxy to the pop's bmc-creds-admin AWS
// secret: given any NetBox device (a server, a switch, a PDU — registered
// as a slagworks machine or not), it returns whatever credential entry the
// secret holds for that device's serial, plus the device's mgmt IP from
// NetBox.
//
// This is the modern, generic replacement for the old `machine bmc-info`
// command. It exists so operators can fetch OOB credentials WITHOUT
// holding direct read access to the AWS secret — slagworksd global owns the
// single privileged fetch path (the IRSA role + admin auth gate), and hands
// the resolved cred to an authenticated caller. It is the one choke point;
// no operator laptop and no other tool should grow a second secret-read path.
//
// Kind is returned verbatim rather than filtered. Documented values are
// "bmc", "amt", "pdu", "serial", and "serial-server"; the secret entry's own kind
// IS the answer. To get a switch's upstream PDU creds, name the PDU device
// directly (it has its own kind=pdu entry); there is no power-topology walk.
//
// This endpoint hands out plaintext credentials. It is gated on the operator
// bearer like the rest of the API and, when SLAGWORKS_OIDC_CREDENTIAL_GROUPS
// is set, on a credential group as well (credentialAuth). Any change to that
// gating must update TestGetDeviceCreds_AuthRequired and
// TestDeviceCreds_CredentialGroups.
type DeviceCreds struct {
	Device      string         `json:"device"` // NetBox device name
	NetBoxID    int            `json:"netbox_id"`
	Serial      string         `json:"serial"`
	Pop         string         `json:"pop"`     // site slug; selects which secret
	Kind        string         `json:"kind"`    // "bmc" | "amt" | "pdu" | "serial" | "serial-server"
	MgmtIP      string         `json:"mgmt_ip"` // mgmt_only interface IP from NetBox (kind=amt: the device oob_ip)
	Username    string         `json:"username"`
	Password    string         `json:"password"`
	ExtraConfig map[string]any `json:"extra_config"`
}

// getDeviceCreds handles GET /api/v1/devices/:ident/creds. See DeviceCreds.
func (s *Server) getDeviceCreds(c *echo.Context) error {
	if s.netbox == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"device cred lookup requires NetBox integration")
	}
	if s.bmcSecrets == nil {
		// Unlike the bmc_runtime soft-fail path, a human asking for a
		// specific credential gets a hard error when the secret store
		// isn't wired — an empty cred is never a useful answer here.
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"device cred lookup requires the admin secret store to be configured")
	}

	ctx := c.Request().Context()
	dev, err := s.resolveDevice(ctx, c.Param("ident"))
	if err != nil {
		return err // already an *echo.HTTPError
	}

	creds, err := s.deviceCredsFor(ctx, dev)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, creds)
}

// resolveDevice resolves a device identifier to a NetBox device, with no
// dependency on a slagworks machine row. The identifier is disambiguated by
// shape:
//
//   - all-digits        -> NetBox device id (falls through on miss)
//   - otherwise          -> tried as a name first, then as a serial
//
// Resolution is fall-through, not exclusive: an all-digit identifier that
// doesn't match a device id is retried as a name and a serial, since some
// vendors ship all-numeric serials. The name-then-serial step covers dotted
// names ("leaf-3.pop1"), dotless names ("pdu-0102-left"), and bare serials
// uniformly. A name or serial matching more than one device is surfaced as
// 409 rather than guessed: NetBox only enforces name uniqueness within a
// site, and a shared serial is a NetBox data error.
func (s *Server) resolveDevice(ctx context.Context, ident string) (*netbox.Device, error) {
	ident = strings.TrimSpace(ident)
	if ident == "" {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "device identifier is required")
	}

	// All-digits => try NetBox device id first. Fall through to
	// name/serial on a miss rather than 404-ing: some vendors ship
	// all-numeric serials, and a bare number could legitimately be
	// either an id or a serial. ID wins when it resolves; otherwise the
	// name/serial lookups below get a shot.
	if id, err := strconv.Atoi(ident); err == nil {
		dev, err := s.netbox.GetDeviceByID(ctx, id)
		if err != nil {
			return nil, echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("netbox lookup: %v", err))
		}
		if dev != nil {
			return dev, nil
		}
	}

	// Exact name match across all sites (siteID 0). A name several sites
	// share, such as a rack-position PDU name, is refused, not guessed.
	dev, err := s.netbox.FindDeviceByName(ctx, ident, 0)
	var ambig *netbox.AmbiguousNameError
	if errors.As(err, &ambig) {
		return nil, echo.NewHTTPError(http.StatusConflict,
			ambig.Error()+"; pass the NetBox device id or serial instead")
	}
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("netbox lookup by name: %v", err))
	}
	if dev != nil {
		return dev, nil
	}

	// Fall through to serial.
	hits, err := s.netbox.GetDevicesBySerial(ctx, ident)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("netbox lookup by serial: %v", err))
	}
	switch len(hits) {
	case 0:
		return nil, echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("no NetBox device matches %q (tried name and serial)", ident))
	case 1:
		return &hits[0], nil
	default:
		names := make([]string, len(hits))
		for i, h := range hits {
			names[i] = h.Name
		}
		return nil, echo.NewHTTPError(http.StatusConflict,
			fmt.Sprintf("serial %q matches %d devices: %s (data error in NetBox; one serial should map to one device)",
				ident, len(hits), strings.Join(names, ", ")))
	}
}

// deviceCredsFor resolves the credential entry + mgmt IP for an already-
// resolved NetBox device. Every missing piece is a hard error: the caller
// named a specific device and wants its specific credential.
func (s *Server) deviceCredsFor(ctx context.Context, dev *netbox.Device) (DeviceCreds, error) {
	if dev.Serial == "" {
		return DeviceCreds{}, echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("device %q (id %d) has no serial in NetBox; the admin secret is keyed by serial, so its credential can't be resolved",
				dev.Name, dev.ID))
	}
	if dev.Site == nil || dev.Site.Slug == "" {
		return DeviceCreds{}, echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("device %q (id %d) has no site in NetBox; can't determine which pop's admin secret to read",
				dev.Name, dev.ID))
	}
	pop := dev.Site.Slug

	secret, err := s.bmcSecrets.GetAdmin(ctx, pop)
	if err != nil {
		return DeviceCreds{}, echo.NewHTTPError(http.StatusBadGateway,
			fmt.Sprintf("fetch admin secret for pop %s: %v", pop, err))
	}
	host := secret.Lookup(dev.Serial)
	if host == nil {
		return DeviceCreds{}, echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("no admin-secret entry for device %q (serial %q) in pop %s", dev.Name, dev.Serial, pop))
	}

	var mgmtIP string
	if host.Kind == awssecrets.KindAMT {
		// AMT shares a host NIC that is not mgmt_only (plan D2); its
		// endpoint is the device's oob_ip, the same address --via amt dials.
		if dev.OOBIP != nil {
			mgmtIP, _, _ = strings.Cut(dev.OOBIP.Address, "/")
		}
	} else {
		mgmtIP, err = s.netboxMgmtIPForDevice(ctx, dev.ID)
		if err != nil {
			return DeviceCreds{}, echo.NewHTTPError(http.StatusInternalServerError,
				fmt.Sprintf("lookup mgmt IP for device %d: %v", dev.ID, err))
		}
	}

	extra := host.ExtraConfig
	if extra == nil {
		extra = map[string]any{}
	}
	return DeviceCreds{
		Device:      dev.Name,
		NetBoxID:    dev.ID,
		Serial:      dev.Serial,
		Pop:         pop,
		Kind:        host.Kind,
		MgmtIP:      mgmtIP,
		Username:    host.Username,
		Password:    host.Password,
		ExtraConfig: extra,
	}, nil
}
