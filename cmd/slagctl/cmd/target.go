package cmd

import (
	"github.com/spf13/cobra"
)

// bindMachineArg attaches the single positional <machine> argument
// that every machine-targeted slagctl command takes. The arg is
// disambiguated by shape (UUID / serial / hostname / unique
// substring) at run time by resolveMachine — see resolve.go.
//
// The shape was simpler when slagctl had `--host` and `--machine-id`
// flags, but operators on call don't carry UUIDs in their head. The
// flag pair was removed 2026-05-25; arg-shape detection covers every
// historical caller (UUIDs, NetBox serials, FQDN hostnames, short
// hostnames, hostname prefixes).
//
// Caller responsibilities: append the positional to Use, set
// cobra.ExactArgs(1), and pass args[0] to resolveMachine inside RunE.
func bindMachineArg(cmd *cobra.Command) {
	cmd.Use += " <machine>"
	cmd.Args = cobra.ExactArgs(1)
}
