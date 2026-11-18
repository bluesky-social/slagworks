package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/internal/model"
)

// powerOpTypes are the operation types that drive a physical power
// transition. Deploy and commission belong here too because their
// executors power-cycle (or power-on) the host as part of the
// lifecycle — a machine's power picture is incomplete without them.
var powerOpTypes = map[model.OperationType]bool{
	model.OpPowerOn:    true,
	model.OpPowerOff:   true,
	model.OpPowerCycle: true,
	model.OpDeploy:     true,
	model.OpCommission: true,
}

// deployOpTypes are the deploy/commission lifecycle op types.
var deployOpTypes = map[model.OperationType]bool{
	model.OpDeploy:     true,
	model.OpCommission: true,
}

// machineOpsCmd is `slagctl machine ops <machine>` — list the operations
// run against one machine. It is sugar over the machine-scoped operation
// list and shares its fetch + render path with `operation list --machine`,
// so the two surfaces stay column-identical. Filter flags:
//
//	--power   power-actuating ops (power on/off/cycle + deploy/commission,
//	          whose executors power-cycle the host into PXE)
//	--deploy  deploy/commission lifecycle ops only
//
// Default (neither flag) lists every op for the machine. --power and
// --deploy are mutually exclusive.
//
// This replaces the old `machine power history`: power events are ops,
// covered by `machine ops --power`.
func machineOpsCmd() *cobra.Command {
	var power, deploy bool
	cmd := &cobra.Command{
		Use:   "ops",
		Short: "list operations run against a machine",
		Long: `List the operations recorded against a machine, newest activity at the
bottom. Columns match "operation list".

By default every op is shown. --power narrows to power-actuating ops
(explicit power on/off/cycle plus deploys and commissions, whose
executors power-cycle the host into PXE); --deploy narrows to deploy
and commission lifecycle ops. The two flags are mutually exclusive.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if power && deploy {
				return fmt.Errorf("--power and --deploy are mutually exclusive")
			}
			r, err := resolveMachine(ctx(cmd), apiClient, args[0])
			if err != nil {
				return err
			}
			ops, err := fetchOperations(ctx(cmd), r.Slot())
			if err != nil {
				return fmt.Errorf("list operations for %s: %w", r.Label, err)
			}
			switch {
			case power:
				ops = filterOpTypes(ops, powerOpTypes)
			case deploy:
				ops = filterOpTypes(ops, deployOpTypes)
			}
			return renderOperations(cmd.OutOrStdout(), ops)
		},
	}
	bindMachineArg(cmd)
	cmd.Flags().BoolVar(&power, "power", false, "show only power-actuating ops (power_on/off/cycle + deploy/commission)")
	cmd.Flags().BoolVar(&deploy, "deploy", false, "show only deploy/commission lifecycle ops")
	return watchable(cmd)
}

// filterOpTypes returns the ops whose Type is in keep, preserving order.
func filterOpTypes(ops []model.Operation, keep map[model.OperationType]bool) []model.Operation {
	out := ops[:0]
	for _, op := range ops {
		if keep[op.Type] {
			out = append(out, op)
		}
	}
	return out
}
