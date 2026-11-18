package worker

import (
	"log/slog"

	"github.com/bluesky-social/slagworks/internal/agent"
	"github.com/bluesky-social/slagworks/internal/agent/execkit"
	"github.com/bluesky-social/slagworks/internal/agent/inventory"
)

// ReadSelfSerial reads the site worker host's hardware serial from SMBIOS
// (dmidecode -s system-serial-number, sysfs fallback) and normalizes it:
// well-known placeholder/default values (e.g. "To Be Filled By O.E.M.",
// "Default string", all-zeros, empty) come back as "" (UNVERIFIED). The
// normalization + placeholder set live in inventory.SystemSerial so the
// commission inventory path and this self-identity handshake share one source
// of truth.
//
// A "" result is NOT a startup failure — the worker starts and attempts to
// connect; GLOBAL decides. Under serial enforcement (production) a hello with
// an empty serial is refused outright, so an unverified worker fails loudly at
// connect (fix dmidecode / the board serial / NetBox, then reconnect) rather
// than running in a half-trusted state. Only a global without a serial
// resolver (dev/test) accepts it. The serial is reported in HelloMsg.Serial
// and joined by global to the NetBox device.
func ReadSelfSerial(log *slog.Logger) string {
	var runner agent.Runner = &execkit.Exec{Log: log}
	serial := inventory.SystemSerial(runner)
	if serial == "" {
		log.Warn("no verified hardware serial from SMBIOS; " +
			"worker self-identity is unverified (dmidecode absent or serial is a placeholder)")
	} else {
		log.Info("resolved hardware serial for self-identity", "serial", serial)
	}
	return serial
}
