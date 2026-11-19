package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/google/uuid"

	slclient "github.com/bluesky-social/slagworks/cmd/slagctl/client"
	"github.com/bluesky-social/slagworks/internal/model"
)

// ResolvedMachine is the result of resolving a single positional
// machine argument: a canonical UUID for use in URL slots and a
// human-readable label for stdout messages. Exported so cmd_test
// can drive resolveMachine directly through ResolveMachine.
type ResolvedMachine struct {
	ID    uuid.UUID
	Label string
}

// Slot returns the URL-slot string used in /api/v1/machines/<slot>
// paths — always the canonical UUID, never a hostname. Even when the
// operator typed a hostname, the resolver pre-resolves to UUID before
// any per-machine endpoint is called.
func (r ResolvedMachine) Slot() string { return r.ID.String() }

// resolveMachine resolves a single positional argument into a
// ResolvedMachine. The arg is disambiguated by shape:
//
//  1. UUID         → returned as-is, no API call.
//  2. exact serial → GET /api/v1/machines?serial=<arg> with one hit.
//  3. exact host   → GET /api/v1/machines?hostname=<arg> with exactly
//     one hit whose name equals arg (NetBox `name__ic` is substring, so
//     the equality check happens client-side). Two or more such hits
//     (NetBox names are unique only per site) are ambiguous and error
//     with their UUIDs.
//  4. unique substring → same call, no exact hit, exactly one match.
//  5. ambiguous → error listing the candidate hostnames.
//  6. miss → "no machine matches".
//
// The Label is "hostname (uuid-prefix)" when a hostname is known,
// else the serial, else the bare UUID.
//
// This replaces the old --host / --machine-id flag pair. Every
// machine-targeted slagctl command takes a single positional arg
// and feeds it through here.
func resolveMachine(ctx context.Context, c *slclient.Client, arg string) (ResolvedMachine, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ResolvedMachine{}, fmt.Errorf("machine argument is required")
	}

	if id, err := uuid.Parse(arg); err == nil {
		return ResolvedMachine{ID: id, Label: id.String()}, nil
	}

	// Exact serial: cheap DB lookup, no NetBox round-trip.
	{
		var hits []model.Machine
		q := url.Values{"serial": {arg}}
		if err := c.Get(ctx, "/api/v1/machines", q, &hits); err != nil {
			return ResolvedMachine{}, fmt.Errorf("lookup by serial: %w", err)
		}
		if len(hits) == 1 {
			return ResolvedMachine{ID: hits[0].ID, Label: serialLabel(hits[0])}, nil
		}
		// Two machines sharing a serial is a NetBox-side data error
		// rather than a slagctl input error; surface it with the
		// candidate UUIDs rather than silently picking one.
		if len(hits) > 1 {
			ids := make([]string, len(hits))
			for i, m := range hits {
				ids[i] = m.ID.String()
			}
			sort.Strings(ids)
			return ResolvedMachine{}, fmt.Errorf(
				"serial %q matches %d machines: %s (data error in NetBox; one serial should map to one device)",
				arg, len(hits), strings.Join(ids, ", "))
		}
	}

	// Hostname: substring match server-side via NetBox. The response
	// includes Hostname on each row so we can disambiguate without a
	// second round-trip.
	var hits []model.Machine
	q := url.Values{"hostname": {arg}}
	if err := c.Get(ctx, "/api/v1/machines", q, &hits); err != nil {
		// 503 from server when NetBox isn't configured — surface as-is
		// so the operator knows to fall back to UUID/serial.
		return ResolvedMachine{}, fmt.Errorf("lookup by hostname: %w", err)
	}
	if len(hits) == 0 {
		return ResolvedMachine{}, fmt.Errorf("no machine matches %q", arg)
	}
	// Exact-name win: a row whose Hostname equals arg (case-
	// insensitive — NetBox device names are case-insensitive at the
	// API layer via `name__ic`, so a typed `CPU1.pop1` should match a
	// stored `cpu1.pop1` exactly even when other substring hits would
	// otherwise force an ambiguous error). NetBox names are unique only
	// within a site (and tenant), so two machines can share one; that
	// is ambiguous too, never a pick of whichever row came first.
	var exact []model.Machine
	for _, m := range hits {
		if strings.EqualFold(m.Hostname, arg) {
			exact = append(exact, m)
		}
	}
	if len(exact) == 1 {
		return ResolvedMachine{ID: exact[0].ID, Label: hostLabel(exact[0])}, nil
	}
	if len(exact) > 1 {
		// The hostnames collide, so list full UUIDs: the one shape
		// that still tells the candidates apart and resolves as-is.
		cands := make([]string, len(exact))
		for i, m := range exact {
			cands[i] = fmt.Sprintf("%s (%s)", m.Hostname, m.ID)
		}
		sort.Strings(cands)
		return ResolvedMachine{}, fmt.Errorf(
			"ambiguous: %q is the hostname of %d machines: %s; pass the machine UUID instead",
			arg, len(exact), strings.Join(cands, ", "))
	}
	if len(hits) == 1 {
		return ResolvedMachine{ID: hits[0].ID, Label: hostLabel(hits[0])}, nil
	}
	names := make([]string, 0, len(hits))
	for _, m := range hits {
		if m.Hostname != "" {
			names = append(names, m.Hostname)
		} else {
			// Should not happen — server populates Hostname when
			// ?hostname= is set. Fall back to UUID so the operator
			// can copy-paste.
			names = append(names, m.ID.String())
		}
	}
	sort.Strings(names)
	return ResolvedMachine{}, fmt.Errorf(
		"ambiguous: %q matches %d machines: %s",
		arg, len(hits), strings.Join(names, ", "))
}

// resolveDeviceIdent maps a positional arg to a NetBox-resolvable device
// identifier for the /devices/:ident/creds endpoint. Unlike the per-machine
// endpoints (which take the canonical UUID from resolveMachine), that endpoint
// resolves against NetBox by name / numeric id / serial and has NO knowledge
// of slagworks machine UUIDs — so a UUID passed straight through 404s.
//
// A UUID arg is unambiguously a slagworks machine reference, so we translate
// it to the machine's NetBox serial (one /machines/<uuid> lookup; the serial
// is the key the admin secret is stored under anyway). Every other shape —
// NetBox name, numeric id, serial, including non-machine devices like PDUs and
// BMC-less switches that have no machine row — is passed through verbatim for
// the server's resolveDevice to handle. That preserves `machine creds
// pdu-0102-left`, which is the whole reason creds doesn't use resolveMachine.
//
// We deliberately do NOT extend resolveMachine to surface the serial: its
// UUID branch is a guaranteed no-network short-circuit (TestResolveMachine_
// UUIDShortCircuits), and translating a UUID to a serial requires an API call.
func resolveDeviceIdent(ctx context.Context, c *slclient.Client, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", fmt.Errorf("device argument is required")
	}
	id, isUUID := parseUUID(arg)
	if !isUUID {
		// Not a UUID — name / id / serial, resolved server-side.
		return arg, nil
	}
	// A UUID-shaped arg that doesn't name a registered machine might still
	// be a NetBox identifier the server can resolve (e.g. a device whose
	// serial is itself UUID-shaped). On a 404 fall back to the verbatim arg
	// so the non-machine path is preserved; any other error (network, 5xx)
	// is real and propagates.
	m, found, err := lookupMachineByID(ctx, c, id)
	if err != nil {
		return "", fmt.Errorf("look up machine %s: %w", id, err)
	}
	if !found {
		return arg, nil
	}
	if m.NetBoxSerial == "" {
		return "", fmt.Errorf(
			"machine %s has no NetBox serial; its device creds can't be resolved (the admin secret is keyed by serial)", id)
	}
	return m.NetBoxSerial, nil
}

// lookupMachineByID fetches a machine by canonical UUID. A 404 is reported as
// found=false with a nil error (the UUID is well-formed but unregistered),
// keeping the "not a machine" signal out of the error channel so callers can
// branch on a plain bool. Any other transport error propagates.
func lookupMachineByID(ctx context.Context, c *slclient.Client, id uuid.UUID) (model.Machine, bool, error) {
	var m model.Machine
	if err := c.Get(ctx, "/api/v1/machines/"+id.String(), nil, &m); err != nil {
		if isNotFound(err) {
			return model.Machine{}, false, nil
		}
		return model.Machine{}, false, err
	}
	return m, true, nil
}

// ResolveDeviceIdent is an exported alias of resolveDeviceIdent for cmd_test.
func ResolveDeviceIdent(ctx context.Context, c *slclient.Client, arg string) (string, error) {
	return resolveDeviceIdent(ctx, c, arg)
}

// isNotFound reports whether err is an API error carrying HTTP 404.
func isNotFound(err error) bool {
	var apiErr *slclient.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// parseUUID reports whether arg is a well-formed UUID, returning the parsed
// value when so. A bool return (rather than the parse error) keeps the
// "not a UUID" signal out of the error channel at the call site.
func parseUUID(arg string) (uuid.UUID, bool) {
	id, err := uuid.Parse(arg)
	return id, err == nil
}

func hostLabel(m model.Machine) string {
	if m.Hostname != "" {
		return fmt.Sprintf("%s (%s)", m.Hostname, shortUUID(m.ID))
	}
	return m.ID.String()
}

func serialLabel(m model.Machine) string {
	if m.NetBoxSerial != "" {
		return fmt.Sprintf("%s (%s)", m.NetBoxSerial, shortUUID(m.ID))
	}
	return m.ID.String()
}

func shortUUID(id uuid.UUID) string {
	s := id.String()
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// ResolveMachine is an alias of resolveMachine for use from external
// test packages (cmd_test). Tests drive the resolver directly so each
// shape branch has its own table entry without going through cobra.
func ResolveMachine(ctx context.Context, c *slclient.Client, arg string) (ResolvedMachine, error) {
	return resolveMachine(ctx, c, arg)
}
