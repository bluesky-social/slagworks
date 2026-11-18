package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/snapshot"
)

// MatchResult reports which config scripts a machine resolves to — the
// network-config script and the storage script the owning worker would select
// for it. It answers the operator question "what would this host get?" without
// a deploy, by reading the SAME authoritative match the boot path uses (the
// built snapshot), so it cannot drift from production.
//
// A nil *MatchedScript means no script of that kind matched (the agent falls
// back to DHCP netplan / default layout respectively) — NOT an error.
type MatchResult struct {
	Machine       uuid.UUID      `json:"machine"`
	Hostname      string         `json:"hostname,omitempty"`
	NetworkScript *MatchedScript `json:"network_script,omitempty"`
	StorageScript *MatchedScript `json:"storage_script,omitempty"`
}

// MatchedScript names the script that won the match. Content is omitted by
// default (the operator wants the name, not the body); callers wanting the body
// fetch it from the script list.
type MatchedScript struct {
	Name string `json:"name"`
}

// matchMachine resolves which config scripts a machine matches by building the
// owning worker's snapshot (the same read-only path preflight uses) and reading
// the matched scripts off the resulting MachineSnapshot. This reuses the
// authoritative match — it does NOT re-run the matchers — so the answer is
// exactly what the boot path would select.
func (s *Server) matchMachine(ctx context.Context, machineID uuid.UUID) (MatchResult, error) {
	res := MatchResult{Machine: machineID}

	if s.snapshotBuilder == nil || s.netbox == nil {
		return res, fmt.Errorf("match: requires NetBox + snapshot builder; this server is configured without them")
	}

	m, err := s.store.GetMachine(machineID)
	if err != nil {
		return res, fmt.Errorf("match: get machine %s: %w", machineID, err)
	}
	if m.NetBoxDeviceID == nil {
		return res, fmt.Errorf("match: machine %s is not bound to a NetBox device, so it has no identity to match scripts against", machineID)
	}

	dev, err := s.netbox.GetDevice(ctx, *m.NetBoxDeviceID)
	if err != nil {
		return res, fmt.Errorf("match: netbox get device %d: %w", *m.NetBoxDeviceID, err)
	}
	if dev == nil {
		return res, fmt.Errorf("match: netbox device %d not found", *m.NetBoxDeviceID)
	}

	slug, err := snapshot.ResolveOwningConfigSlug(ctx, s.netbox, s.store, *dev)
	if err != nil {
		if errors.Is(err, snapshot.ErrNoOwningConfig) {
			return res, fmt.Errorf("match: no site_config claims device %q (site %s); no worker would boot it, so no script applies", dev.Name, siteSlug(dev))
		}
		return res, fmt.Errorf("match: resolve owning config for device %d: %w", dev.ID, err)
	}

	snap, err := s.snapshotBuilder.BuildSnapshot(ctx, slug)
	if err != nil {
		return res, fmt.Errorf("match: build snapshot for site %s: %w", slug, err)
	}

	var ms *model.MachineSnapshot
	for i := range snap.Machines {
		if snap.Machines[i].ID == machineID {
			ms = &snap.Machines[i]
			break
		}
	}
	if ms == nil {
		return res, fmt.Errorf("match: machine %s resolved to config %q but did not appear in its built snapshot (it dropped out of scope between resolve and build)", machineID, slug)
	}

	return matchResultFromSnapshot(machineID, ms), nil
}

// matchResultFromSnapshot projects a machine's built MachineSnapshot into the
// MatchResult wire shape — the only logic in the match path that isn't a reuse
// of the (pg/NetBox-backed) snapshot-build chain, so it's factored out to be
// unit-testable on its own. A nil *Snapshot{Network,Storage}Script means no
// script of that kind matched (agent fallback), reported as a nil MatchedScript.
func matchResultFromSnapshot(machineID uuid.UUID, ms *model.MachineSnapshot) MatchResult {
	res := MatchResult{Machine: machineID}
	res.Hostname = ms.FQDN
	if res.Hostname == "" {
		res.Hostname = ms.Hostname
	}
	if ms.NetworkScript != nil {
		res.NetworkScript = &MatchedScript{Name: ms.NetworkScript.Name}
	}
	if ms.StorageScript != nil {
		res.StorageScript = &MatchedScript{Name: ms.StorageScript.Name}
	}
	return res
}

// matchMachineHandler serves GET /machines/:id/match. Read-only: it builds the
// owning worker's snapshot and reports the matched scripts, creating no op.
func (s *Server) matchMachineHandler(c *echo.Context) error {
	id, err := s.resolveMachineParam(c)
	if err != nil {
		return err
	}
	res, err := s.matchMachine(c.Request().Context(), id)
	if err != nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, err.Error())
	}
	return c.JSON(http.StatusOK, res)
}
