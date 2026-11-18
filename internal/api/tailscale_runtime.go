package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/tailscale"
)

// TailscaleMinter mints single-use tailnet auth keys. Satisfied by
// *tailscale.Minter in production; tests inject a fake. nil disables
// deploy-time enrollment (the minter is optional — dev/test and any
// deployment without the OAuth client configured run without it).
type TailscaleMinter interface {
	Mint(ctx context.Context, kr tailscale.KeyRequest) (tailscale.MintedKey, error)
}

// tailscaleKeyExpirySeconds bounds the minted key's validity. Generous
// relative to a deploy (which runs in minutes) so a slow PXE/queue path
// doesn't outlast the key, but bounded so a never-consumed key (deploy
// abandoned before boot) doesn't linger. One-off keys are also revoked the
// instant a node registers, so this only matters for the unused case.
// 2h covers any realistic deploy + retry window (plan OQ1).
const tailscaleKeyExpirySeconds = 2 * 60 * 60

// resolveTailscaleRuntime mints a single-use tailnet key for a deploy op
// and stamps a TailscaleRuntime block onto params (params.TailscaleRuntime).
// It mirrors resolveBMCRuntime's structure but with SOFT failure semantics:
// tailnet enrollment is an enhancement, not a deploy precondition, so a
// missing minter or a mint error logs a warning and leaves params unchanged
// (no tailscale_runtime block) rather than blocking the deploy. The agent
// treats an absent auth key as "skip enrollment".
//
// Enrollment is skipped (params left unchanged) when:
//   - no minter is configured (dev/test, or the OAuth client secret absent);
//   - NetBox is off or the machine isn't bound to a device (no pop/hostname);
//   - the device has no site slug (can't derive the tag);
//   - minting fails (API error, tag not owned, etc.) — logged, not fatal.
func (s *Server) resolveTailscaleRuntime(ctx context.Context, machineID uuid.UUID, params *model.OpParams) error {
	if s.tailscaleMinter == nil {
		return nil
	}
	if s.netbox == nil {
		return nil
	}

	m, err := s.store.GetMachine(machineID)
	if err != nil {
		return fmt.Errorf("resolve tailscale runtime: get machine %s: %w", machineID, err)
	}
	if m.NetBoxDeviceID == nil {
		s.log.Warn("tailscale enrollment skipped: machine not bound to a NetBox device",
			"machine_id", machineID)
		return nil
	}
	dev, err := s.netbox.GetDevice(ctx, *m.NetBoxDeviceID)
	if err != nil {
		return fmt.Errorf("resolve tailscale runtime: netbox get device %d: %w", *m.NetBoxDeviceID, err)
	}
	if dev == nil || dev.Site == nil || dev.Site.Slug == "" {
		s.log.Warn("tailscale enrollment skipped: device has no site slug",
			"machine_id", machineID)
		return nil
	}

	pop := dev.Site.Slug
	tag := "tag:" + pop
	hostname := tailnetHostname(dev.Name, pop)

	minted, err := s.tailscaleMinter.Mint(ctx, tailscale.KeyRequest{
		Tags:          []string{tag},
		ExpirySeconds: tailscaleKeyExpirySeconds,
		Description:   "slagworks deploy enroll " + hostname,
	})
	if err != nil {
		// Soft fail: the host still deploys, just without tailnet
		// enrollment (ansible will enroll it on converge as before).
		s.log.Warn("tailscale enrollment skipped: mint failed",
			"machine_id", machineID, "pop", pop, "err", err)
		return nil
	}

	params.TailscaleRuntime = &model.TailscaleRuntime{
		AuthKey:  minted.Key,
		Hostname: hostname,
		Tags:     []string{tag},
	}
	s.log.Info("tailscale enrollment key minted",
		"machine_id", machineID, "pop", pop, "hostname", hostname, "key_id", minted.ID)
	return nil
}

// tailnetHostname derives the tailnet node name from a NetBox device name and
// its pop slug, matching ansible's tailscale_hostname = <short>-<pop> scheme
// (group_vars/all). NetBox names these hosts <short>.<pop> (e.g.
// "oob-gw-3.pop2"); strip a trailing ".<pop>" if present, then join with "-".
// A device name that doesn't carry the ".<pop>" suffix (or already lacks it)
// is used as-is for the short part.
func tailnetHostname(devName, pop string) string {
	short := strings.TrimSuffix(devName, "."+pop)
	return short + "-" + pop
}
