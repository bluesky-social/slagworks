package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"

	"github.com/bluesky-social/slagworks/internal/awssecrets"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
)

// preflightTTL is how long a preflight token remains valid. Short enough
// that a stale confirmation after a topology change is unlikely to slip
// through the re-validation on POST; long enough for a human operator to
// read the warning and type a confirmation (or use --yes).
const preflightTTL = 5 * time.Minute

// OutletResolution is the response shape for GET
// /api/v1/pdus/:ident/outlet/:n/power/resolve. It is a pure read:
// no op is created, no state is changed. The CLI uses it to show the
// operator the server-resolved facts (downstream device, feed
// classification, warning) before prompting for confirmation.
type OutletResolution struct {
	// PDU identity.
	PDUName   string `json:"pdu_name"`
	PDUMgmtIP string `json:"pdu_mgmt_ip"`
	PDUSerial string `json:"pdu_serial"`

	// Outlet index (1-based, as on the APC CLI).
	Outlet int `json:"outlet"`

	// Connected is false when the outlet has no cabled downstream device
	// in NetBox. An unconnected outlet is not safe to actuate in prod
	// (the self-power interlock can't identify what it cuts).
	Connected bool `json:"connected"`

	// Downstream is populated when Connected is true.
	DownstreamDevice string `json:"downstream_device,omitempty"`
	DownstreamSerial string `json:"downstream_serial,omitempty"`
	DownstreamPort   string `json:"downstream_port,omitempty"` // power-port name on the downstream device

	// Classification is the feed-topology verdict:
	//   "unconnected"          - no cabled downstream device
	//   "sole"                 - exactly one modeled feed (or independence unverifiable)
	//   "redundant_unverified" - >=2 distinct-PDU feeds modeled, live health unverified
	Classification string `json:"classification"`

	// Warning is a human-readable string the CLI echoes before confirmation.
	Warning string `json:"warning"`

	// PreflightToken is a short-lived HMAC token binding
	// (operator, verb, pdu device id, outlet index, downstream serial,
	// classification). The POST endpoint validates it before creating the op.
	// Empty when Connected is false (no token is issued for unconnected outlets
	// — the POST will refuse anyway, so the CLI never reaches a confirm).
	PreflightToken string `json:"preflight_token,omitempty"`
}

// outletResolution is the internal, fully-resolved state used by both
// handler paths. It augments OutletResolution with data only the server needs
// (credentials, machine UUID) that we don't want to expose on the wire.
type outletResolution struct {
	OutletResolution
	creds       DeviceCreds
	pduDeviceID int
	machineID   uuid.UUID
	// pduDev is the resolved PDU NetBox device, retained so the POST handler
	// can route the op-creation-time eligible-partner check to the PDU's
	// owning site-config slug (same routing the claim uses).
	pduDev *netbox.Device
}

// preflightClaims is the set of fields bound into the token. Any mismatch
// between the token's claims and the re-resolved state on POST causes a
// 409 — resolution changed since preflight.
type preflightClaims struct {
	Operator         string `json:"op"`
	Verb             string `json:"verb"`
	PDUDeviceID      int    `json:"pdu_id"`
	Outlet           int    `json:"n"`
	DownstreamSerial string `json:"ds"`
	Classification   string `json:"cls"`
	ExpiresAt        int64  `json:"exp"` // unix seconds
}

// mintPreflightToken serialises claims and returns an HMAC-SHA256 token
// of the form "<hex-claims>.<hex-sig>". Claims are plaintext JSON hex-encoded
// (no secret in the claims themselves — the sig is the authenticity proof).
func mintPreflightToken(key []byte, c preflightClaims) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("marshal preflight claims: %w", err)
	}
	encoded := hex.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	sig := hex.EncodeToString(mac.Sum(nil))
	return encoded + "." + sig, nil
}

// verifyPreflightToken parses and authenticates a token produced by
// mintPreflightToken, returning the embedded claims. Returns an error if
// the token is malformed, the signature doesn't verify, or the token has
// expired.
//
// Token format: "<hex-claims>.<hex-HMAC-SHA256-sig>"
// The HMAC hex is always 64 characters (SHA256 → 32 bytes → 64 hex chars).
// We find the separator by locating the last '.', which is safe because the
// claims portion is also hex-encoded (no dots).
func verifyPreflightToken(key []byte, token string) (preflightClaims, error) {
	// The hex-encoded HMAC-SHA256 is always exactly 64 chars; the separator
	// is at position len(token)-65 (64 sig chars + 1 dot).
	if len(token) < 66 { // minimum: 1 claims char + '.' + 64 sig chars
		return preflightClaims{}, fmt.Errorf("malformed preflight token")
	}
	dot := len(token) - 65
	if token[dot] != '.' {
		return preflightClaims{}, fmt.Errorf("malformed preflight token (separator not found)")
	}
	encodedClaims := token[:dot]
	gotSig := token[dot+1:]

	raw, err := hex.DecodeString(encodedClaims)
	if err != nil {
		return preflightClaims{}, fmt.Errorf("malformed preflight token (decode claims): %w", err)
	}

	mac := hmac.New(sha256.New, key)
	mac.Write(raw)
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(gotSig), []byte(expectedSig)) {
		return preflightClaims{}, fmt.Errorf("preflight token signature invalid")
	}

	var c preflightClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return preflightClaims{}, fmt.Errorf("decode preflight claims: %w", err)
	}
	if time.Now().Unix() > c.ExpiresAt {
		return preflightClaims{}, fmt.Errorf("preflight token expired")
	}
	return c, nil
}

// resolveOutlet performs the core reverse-topology lookup for a PDU + outlet
// number: it resolves the PDU device from an ident, verifies it is a PDU
// (kind check), finds the Outlet N row, traces its cable to the downstream
// device, and classifies the downstream's feed set.
//
// Callers must ensure s.netbox != nil before calling. s.bmcSecrets may be
// nil (dev/test): in that case the kind check and credential resolution are
// skipped, and any device is accepted as a PDU.
func (s *Server) resolveOutlet(ctx context.Context, ident string, outletN int) (outletResolution, error) {
	// Step 1: resolve PDU device from the ident.
	pduDev, err := s.resolveDevice(ctx, ident)
	if err != nil {
		return outletResolution{}, err // already *echo.HTTPError
	}

	// Step 2: resolve the PDU's creds (mgmt IP, serial, kind).
	var creds DeviceCreds
	if s.bmcSecrets == nil {
		// No secret store (dev/test): deviceCredsFor reads the store, so
		// resolve the mgmt IP and serial from NetBox alone and leave the
		// kind and credential fields empty.
		mgmtIP, ipErr := s.netboxMgmtIPForDevice(ctx, pduDev.ID)
		if ipErr != nil {
			return outletResolution{}, echo.NewHTTPError(http.StatusInternalServerError,
				fmt.Sprintf("lookup mgmt IP for PDU %q (id %d): %v", pduDev.Name, pduDev.ID, ipErr))
		}
		creds = DeviceCreds{Device: pduDev.Name, NetBoxID: pduDev.ID, Serial: pduDev.Serial, MgmtIP: mgmtIP}
	} else {
		creds, err = s.deviceCredsFor(ctx, pduDev)
		if err != nil {
			return outletResolution{}, err
		}
		// Kind guard: only PDU devices may be addressed here.
		if creds.Kind != awssecrets.KindPDU {
			return outletResolution{}, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("device %q (kind %q) is not a PDU; only PDU devices can be addressed via this endpoint",
					pduDev.Name, creds.Kind))
		}
	}

	// Step 3: PDU must have a Machine row (enrolled via a site-config).
	machine, err := s.store.GetMachineByNetBoxDeviceID(pduDev.ID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return outletResolution{}, echo.NewHTTPError(http.StatusUnprocessableEntity,
				fmt.Sprintf("PDU %q is not a managed machine; cannot drive it through the op queue "+
					"(enroll it by adding the pop's site-config)", pduDev.Name))
		}
		return outletResolution{}, echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("lookup PDU machine: %v", err))
	}

	// Step 4: find the named outlet on the PDU.
	outlets, err := s.netbox.ListPowerOutletsForDevice(ctx, pduDev.ID)
	if err != nil {
		return outletResolution{}, echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("list power-outlets for PDU %q: %v", pduDev.Name, err))
	}
	expectedName := fmt.Sprintf("Outlet %d", outletN)
	var found *netbox.PowerOutlet
	for i := range outlets {
		if outlets[i].Name == expectedName {
			found = &outlets[i]
			break
		}
	}
	if found == nil {
		return outletResolution{}, echo.NewHTTPError(http.StatusNotFound,
			fmt.Sprintf("outlet %d not found on PDU %q (checked %d outlets)",
				outletN, pduDev.Name, len(outlets)))
	}

	// Populate the invariant fields (present regardless of connected status).
	r := outletResolution{
		OutletResolution: OutletResolution{
			PDUName:   pduDev.Name,
			PDUMgmtIP: creds.MgmtIP,
			PDUSerial: pduDev.Serial,
			Outlet:    outletN,
		},
		creds:       creds,
		pduDeviceID: pduDev.ID,
		machineID:   machine.ID,
		pduDev:      pduDev,
	}

	// Step 5: trace the outlet's cable to the downstream device.
	if !found.ConnectedEndpointsReachable || len(found.ConnectedEndpoints) == 0 {
		r.Connected = false
		r.Classification = "unconnected"
		r.Warning = fmt.Sprintf(
			"Outlet %d on %s has no cabled downstream device in NetBox; "+
				"refusing because the self-power interlock cannot determine what it feeds — "+
				"fix the cabling in NetBox, or (non-prod only) pass the unsafe-override.",
			outletN, pduDev.Name)
		return r, nil
	}

	downstreamPort := found.ConnectedEndpoints[0]
	downstreamDev := downstreamPort.Device
	if downstreamDev == nil {
		// The cable traces to a port with no parent device — a modeling gap.
		// Treat as unconnected (fail-closed).
		r.Connected = false
		r.Classification = "unconnected"
		r.Warning = fmt.Sprintf(
			"Outlet %d on %s: downstream power-port %q has no parent device in NetBox; "+
				"cannot determine downstream identity.",
			outletN, pduDev.Name, downstreamPort.Name)
		return r, nil
	}

	r.Connected = true
	r.DownstreamDevice = downstreamDev.Name
	r.DownstreamPort = downstreamPort.Name

	// Fetch the downstream device's full record to get its serial. A failure
	// here is non-fatal for the topology read, but the serial IS required for
	// the token and for the P4 interlock — we'll surface the error.
	downstreamFull, fetchErr := s.netbox.GetDevice(ctx, downstreamDev.ID)
	if fetchErr != nil {
		return outletResolution{}, echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("fetch downstream device %q (id %d): %v",
				downstreamDev.Name, downstreamDev.ID, fetchErr))
	}
	if downstreamFull != nil {
		r.DownstreamSerial = downstreamFull.Serial
	}
	// Fail-closed: the design doc (P2 constraint, consumed by P4 interlock)
	// requires that an outlet op with no resolved downstream serial is refused.
	// A downstream device with no NetBox serial means the interlock can't
	// identify what it cuts — same as the "unconnected" case, and just as unsafe.
	if r.DownstreamSerial == "" {
		return outletResolution{}, echo.NewHTTPError(http.StatusUnprocessableEntity,
			fmt.Sprintf("downstream device %q (id %d) has no serial in NetBox; "+
				"cannot create a self-power-safe outlet op (the interlock requires a serial to identify the downstream device — add a serial in NetBox)",
				downstreamDev.Name, downstreamDev.ID))
	}

	// Step 6: classify the downstream's feeds.
	//
	// Count how many of the downstream device's power-ports have reachable
	// connections, and how many distinct parent PDU devices they connect to.
	//
	// >=2 distinct-PDU parent devices => "redundant_unverified"
	//    (topology suggests redundancy, but sibling feed live-health is NOT
	//    verified in P2; treat as possibly-sole for safety)
	// < 2 distinct-PDU parents, or no reachable feeds => "sole"
	//    (the named outlet may be the only effective feed)
	feeds, _ := s.netbox.ListPowerPortsForDevice(ctx, downstreamDev.ID)
	// A fetch error here is non-fatal: fall back to "sole" (the stronger,
	// safer warning) rather than refusing the whole resolution.

	distinctPDUs := make(map[int]struct{})
	for _, feed := range feeds {
		if !feed.ConnectedEndpointsReachable || len(feed.ConnectedEndpoints) == 0 {
			continue
		}
		if outlet := feed.ConnectedEndpoints[0]; outlet.Device != nil {
			distinctPDUs[outlet.Device.ID] = struct{}{}
		}
	}

	if len(distinctPDUs) >= 2 {
		r.Classification = "redundant_unverified"
		r.Warning = fmt.Sprintf(
			"Outlet %d feeds %s, which has %d feeds on distinct PDUs in topology; "+
				"cycling this ONE outlet is expected to drop one feed, NOT reboot %s. "+
				"(Feed health is unverified — treat as possibly-sole if sibling feed status is unknown.)",
			outletN, r.DownstreamDevice, len(distinctPDUs), r.DownstreamDevice)
	} else {
		r.Classification = "sole"
		r.Warning = fmt.Sprintf(
			"Outlet %d may be the only effective power feed for %s; "+
				"cycling it may hard-power-off %s.",
			outletN, r.DownstreamDevice, r.DownstreamDevice)
	}

	return r, nil
}

// resolvePDUOutletPower handles GET /api/v1/pdus/:ident/outlet/:n/power/resolve.
// Non-mutating. Resolves the PDU outlet, traces its cable, classifies the
// downstream's feeds, and returns a signed preflight token the POST endpoint
// will validate.
func (s *Server) resolvePDUOutletPower(c *echo.Context) error {
	if s.netbox == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"PDU outlet resolution requires NetBox integration")
	}

	outletN, err := parseOutletParam(c)
	if err != nil {
		return err
	}

	verb := c.QueryParam("verb")
	switch verb {
	case "cycle", "on", "off":
	default:
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("query param verb=%q must be cycle/on/off", verb))
	}

	ctx := c.Request().Context()
	r, err := s.resolveOutlet(ctx, c.Param("ident"), outletN)
	if err != nil {
		return err
	}

	// Only issue a token for connected outlets. An unconnected outlet is
	// refused at POST anyway, so there's no confirm path to protect.
	if r.Connected {
		operator := ""
		if claims := operatorFromContext(c); claims != nil {
			operator = claims.Subject
		}
		claims := preflightClaims{
			Operator:         operator,
			Verb:             verb,
			PDUDeviceID:      r.pduDeviceID,
			Outlet:           outletN,
			DownstreamSerial: r.DownstreamSerial,
			Classification:   r.Classification,
			ExpiresAt:        time.Now().Add(preflightTTL).Unix(),
		}
		token, mintErr := mintPreflightToken(s.preflightKey, claims)
		if mintErr != nil {
			return echo.NewHTTPError(http.StatusInternalServerError,
				"mint preflight token: "+mintErr.Error())
		}
		r.PreflightToken = token
	}

	return c.JSON(http.StatusOK, r.OutletResolution)
}

// powerPDUOutlet handles POST /api/v1/pdus/:ident/outlet/:n/power.
// Validates the preflight token, re-resolves current topology (refusing if
// anything changed since the token was minted), and creates the verb-matched
// power op anchored on the PDU's Machine row.
func (s *Server) powerPDUOutlet(c *echo.Context) error {
	if s.netbox == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable,
			"PDU outlet power requires NetBox integration")
	}

	outletN, err := parseOutletParam(c)
	if err != nil {
		return err
	}

	var body struct {
		Verb           string `json:"verb"`
		PreflightToken string `json:"preflight_token"`
		// AllowSelf is the INV-2 override (`slagctl ... --allow-self`): let a
		// single-worker site cycle/on its OWN feed. Honored only for cycle/on
		// — never off (INV-1). Refused below when set with verb=off.
		AllowSelf bool `json:"allow_self"`
	}
	if err := c.Bind(&body); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	switch body.Verb {
	case "cycle", "on", "off":
	default:
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("verb %q must be cycle/on/off", body.Verb))
	}
	// INV-1: --allow-self never applies to off (no serving-worker-off path in
	// this command). Reject loudly rather than silently ignoring the flag.
	if body.AllowSelf && body.Verb == "off" {
		return echo.NewHTTPError(http.StatusBadRequest,
			"--allow-self is not valid for `off` (there is no serving-worker-off path; INV-1)")
	}
	if body.PreflightToken == "" {
		return echo.NewHTTPError(http.StatusBadRequest,
			"preflight_token is required; call GET .../resolve?verb=<verb> first")
	}

	// Validate the preflight token before doing any NetBox work.
	tokenClaims, tokenErr := verifyPreflightToken(s.preflightKey, body.PreflightToken)
	if tokenErr != nil {
		return echo.NewHTTPError(http.StatusConflict,
			"preflight token invalid or expired: "+tokenErr.Error()+"; re-run the resolve step")
	}
	if tokenClaims.Verb != body.Verb {
		return echo.NewHTTPError(http.StatusConflict,
			fmt.Sprintf("preflight token was issued for verb %q but request uses %q; re-run the resolve step",
				tokenClaims.Verb, body.Verb))
	}
	// Operator binding: the same authenticated operator that requested the
	// resolve must post the mutation. Empty token operator means dev/test
	// (no secret store, no OIDC) — skip the check in that case.
	if tokenClaims.Operator != "" {
		operator := ""
		if claims := operatorFromContext(c); claims != nil {
			operator = claims.Subject
		}
		if operator != tokenClaims.Operator {
			return echo.NewHTTPError(http.StatusConflict,
				"preflight token was issued for a different operator; re-run the resolve step")
		}
	}

	// Re-resolve (don't trust client-supplied topology — the token binding
	// proves the token but not that topology is unchanged).
	ctx := c.Request().Context()
	r, resolveErr := s.resolveOutlet(ctx, c.Param("ident"), outletN)
	if resolveErr != nil {
		return resolveErr
	}

	// Re-validate all token-bound fields against the current resolution.
	// Any mismatch means topology changed since the token was minted.
	if r.pduDeviceID != tokenClaims.PDUDeviceID ||
		r.Outlet != tokenClaims.Outlet ||
		r.DownstreamSerial != tokenClaims.DownstreamSerial ||
		r.Classification != tokenClaims.Classification {
		return echo.NewHTTPError(http.StatusConflict,
			"resolution changed since preflight; re-run the resolve step")
	}

	// Refuse unconnected outlets (fail-closed for the interlock).
	if !r.Connected {
		return echo.NewHTTPError(http.StatusUnprocessableEntity,
			fmt.Sprintf("outlet %d on %s has no cabled downstream device; cannot create outlet op",
				outletN, r.PDUName))
	}

	// Resolve the apc credential from the secret store and build the
	// single-outlet PDURuntime envelope.
	outlet := model.PDUOutlet{
		PDUHost: r.creds.MgmtIP,
		Outlet:  outletN,
		Serial:  r.creds.Serial,
	}
	if s.bmcSecrets != nil {
		secret, secErr := s.bmcSecrets.GetAdmin(ctx, r.creds.Pop)
		if secErr != nil {
			return echo.NewHTTPError(http.StatusBadGateway,
				fmt.Sprintf("fetch admin secret for pop %s: %v", r.creds.Pop, secErr))
		}
		h := secret.Lookup(r.creds.Serial)
		switch {
		case r.creds.Serial == "":
			return echo.NewHTTPError(http.StatusUnprocessableEntity,
				fmt.Sprintf("PDU %q has no serial; cannot resolve credential", r.PDUName))
		case h == nil:
			return echo.NewHTTPError(http.StatusNotFound,
				fmt.Sprintf("no admin-secret entry for PDU serial %q (%s)", r.creds.Serial, r.PDUName))
		case h.Kind != awssecrets.KindPDU:
			return echo.NewHTTPError(http.StatusUnprocessableEntity,
				fmt.Sprintf("admin-secret entry for serial %q is kind %q, expected pdu", r.creds.Serial, h.Kind))
		case h.Username == "" || h.Password == "":
			return echo.NewHTTPError(http.StatusUnprocessableEntity,
				fmt.Sprintf("admin-secret entry for PDU serial %q has incomplete credential", r.creds.Serial))
		}
		outlet.Username = h.Username
		outlet.Password = h.Password
	}

	// P4b interlock creation-time enforcement. The claim-time gate is the
	// authoritative safety enforcement; these creation-time checks turn a
	// would-be silent timeout (or a refused claim) into an immediate operator
	// error and keep an un-servable / unsafe op out of the queue entirely.
	//
	// A resolved outlet op ALWAYS has a downstream serial here (resolveOutlet
	// 422s on an empty one), but re-assert it: an outlet op with no downstream
	// serial is invalid, never claimable (fail-closed input to the interlock).
	if r.DownstreamSerial == "" {
		return echo.NewHTTPError(http.StatusUnprocessableEntity,
			"outlet op has no resolved downstream serial; cannot create a self-power-safe op")
	}
	// Is the downstream device itself a registered serving worker? (INV-1 /
	// self-feed classification, resolved from the persisted registered set.)
	downstreamWorkers, dwErr := s.store.ListSiteWorkersBySerial(r.DownstreamSerial)
	if dwErr != nil {
		return echo.NewHTTPError(http.StatusInternalServerError,
			fmt.Sprintf("resolve downstream worker-ness: %v", dwErr))
	}
	downstreamIsWorker := len(downstreamWorkers) > 0
	// INV-1: refuse `off` on a serving-worker downstream at creation (the
	// claim path refuses it too; this is the belt-and-suspenders that gives
	// the operator an immediate error). Never overridable.
	if body.Verb == "off" && downstreamIsWorker {
		return echo.NewHTTPError(http.StatusUnprocessableEntity,
			fmt.Sprintf("refusing to power off outlet feeding %q: it is a serving site worker (INV-1: no serving-worker-off path in this command)", r.DownstreamDevice))
	}
	// Eligible-partner check for a non-AllowSelf self-feed cycle/on: if the
	// downstream is a serving worker AND the operator did NOT pass
	// --allow-self, some connected, claim-eligible NON-SELF worker must exist
	// to serve it (the self-worker will decline its own claim via INV-2). An
	// eligible partner is a connection whose verified serial != "" AND does
	// not match DownstreamSerial. Without one, the op could only ever time
	// out — refuse creation with an actionable message. AllowSelf skips this (the operator
	// accepted the self-cycle).
	if (body.Verb == "cycle" || body.Verb == "on") && downstreamIsWorker && !body.AllowSelf {
		if !s.eligiblePartnerConnected(ctx, r, body.Verb) {
			return echo.NewHTTPError(http.StatusUnprocessableEntity,
				fmt.Sprintf("no eligible partner worker is connected to serve this outlet op (it feeds serving worker %q, which cannot cut its own feed); use --allow-self if this is a single-worker site and you accept the self-%s",
					r.DownstreamDevice, body.Verb))
		}
	}

	rt := model.PDURuntime{
		Outlets:          []model.PDUOutlet{outlet},
		DownstreamSerial: r.DownstreamSerial, // P4b interlock consumes this
		Outlet:           true,               // explicit outlet-op marker (interlock applicability)
		AllowSelf:        body.AllowSelf,     // INV-2 override (cycle/on only; rejected on off above)
	}
	params := model.OpParams{PDURuntime: &rt}
	rawParams, marshalErr := json.Marshal(params)
	if marshalErr != nil {
		return echo.NewHTTPError(http.StatusInternalServerError,
			"marshal op params: "+marshalErr.Error())
	}

	// Map verb to op type and create the op anchored on the PDU's machine.
	var opType model.OperationType
	switch body.Verb {
	case "cycle":
		opType = model.OpPowerCycle
	case "on":
		opType = model.OpPowerOn
	case "off":
		opType = model.OpPowerOff
	}

	op, createErr := s.store.CreatePowerOp(r.machineID, opType, rawParams)
	if createErr != nil {
		return echo.NewHTTPError(http.StatusConflict, createErr.Error())
	}

	s.log.Info("PDU outlet power op created",
		"op_id", op.ID,
		"type", op.Type,
		"machine_id", op.MachineID,
		"pdu", r.PDUName,
		"outlet", outletN,
		"downstream_serial", r.DownstreamSerial,
	)
	s.publishOpDelta(*op, "")
	return c.JSON(http.StatusCreated, op)
}

// eligiblePartnerConnected reports whether some connected, claim-eligible
// NON-SELF worker exists to serve a self-feed cycle/on outlet op. "Eligible"
// mirrors the claim-time predicates: a connection whose verified serial is
// non-empty AND does not match the downstream serial under
// model.SameHardwareSerial, the case-insensitive relation the hello gate
// verified it with (a bare connected worker with an empty serial is
// claim-ineligible, so counting it would reproduce the very timeout this
// check prevents). Routing uses the PDU device's owning
// site-config slug — the same slug the claim will arrive under.
//
// When no hub is wired (powerStatusRPC nil — tests / a global without the WS
// hub), partner connectivity can't be observed, so this returns false: the op
// is refused at creation rather than accepted to time out. Safety is enforced
// at claim time regardless; this is the advisory UX gate.
func (s *Server) eligiblePartnerConnected(ctx context.Context, r outletResolution, _ string) bool {
	if s.powerStatusRPC == nil || r.pduDev == nil {
		return false
	}
	slug, err := s.routeSlugForDevice(ctx, "outlet power", r.pduDev)
	if err != nil {
		// Can't resolve the owning slug -> can't prove a partner is reachable.
		return false
	}
	for _, serial := range s.powerStatusRPC.ConnectedWorkerSerials(slug) {
		if serial != "" && !model.SameHardwareSerial(serial, r.DownstreamSerial) {
			return true
		}
	}
	return false
}

// parseOutletParam extracts and validates the :n route parameter as a
// 1-based outlet index. Returns an *echo.HTTPError on any parse failure.
func parseOutletParam(c *echo.Context) (int, error) {
	nStr := c.Param("n")
	n, err := strconv.Atoi(nStr)
	if err != nil {
		return 0, echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("outlet %q is not a valid integer", nStr))
	}
	if n < 1 {
		return 0, echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("outlet index must be >= 1, got %d", n))
	}
	return n, nil
}
