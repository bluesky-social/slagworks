package inventory

import (
	"testing"

	"github.com/bluesky-social/slagworks/internal/agent"
)

// ipmitoolTrap is a fixture runner that reports ipmitool present and fails
// the test if anything actually invokes it.
type ipmitoolTrap struct {
	*fixtureRunner
	t *testing.T
}

func (r ipmitoolTrap) Available(name string) bool {
	return name == "ipmitool" || r.fixtureRunner.Available(name)
}

func (r ipmitoolTrap) Output(name string, args ...string) (string, error) {
	if name == "ipmitool" {
		r.t.Errorf("ipmitool invoked on an AMT host: %v", args)
	}
	return r.fixtureRunner.Output(name, args...)
}

func (r ipmitoolTrap) Run(name string, args ...string) error {
	_, err := r.Output(name, args...)
	return err
}

// TestCollect_SkipBMCForAMT: with SkipBMC (boot cmdline slagworks.oob=amt)
// both collectors record bmc_ipmitool as skipped, never run ipmitool, and
// report no BMC, so an AMT host's commission never posts IPMI BMC data.
func TestCollect_SkipBMCForAMT(t *testing.T) {
	for name, collect := range map[string]func(agent.Runner, Options) *CommissionResult{
		"Collect":         Collect,
		"CollectGraceful": CollectGraceful,
	} {
		t.Run(name, func(t *testing.T) {
			r := ipmitoolTrap{fixtureRunner: newFixtureRunner("testdata/ubuntu-2404", "dmidecode", "lsblk", "ip", "lspci"), t: t}
			res := collect(r, Options{SkipBMC: true})
			if res.BMC != nil {
				t.Errorf("BMC = %+v, want nil on an AMT host", res.BMC)
			}
			for _, s := range res.Steps {
				if s.Name == "bmc_ipmitool" {
					if s.Status != StepStatusSkipped {
						t.Errorf("bmc_ipmitool status = %q, want skipped", s.Status)
					}
					return
				}
			}
			t.Error("bmc_ipmitool step missing; it must still be recorded (as skipped)")
		})
	}
}
