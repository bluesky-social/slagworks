package target

import (
	"testing"

	"gopkg.in/yaml.v2"
)

// parsedCloudInitSeed decodes the seed the way cloud-init does: PyYAML reads
// YAML 1.1, and so does yaml.v2, so an unquoted `off` or `no` comes back as a
// bool here exactly as it would on the host.
type parsedCloudInitSeed struct {
	DatasourceList   []string `yaml:"datasource_list"`
	PreserveHostname any      `yaml:"preserve_hostname"`
	Network          struct {
		Config any `yaml:"config"`
	} `yaml:"network"`
	Growpart struct {
		Mode any `yaml:"mode"`
	} `yaml:"growpart"`
	ResizeRootfs any `yaml:"resize_rootfs"`
	Locale       any `yaml:"locale"`
}

func parseCloudInitSeed(t *testing.T) parsedCloudInitSeed {
	t.Helper()
	var seed parsedCloudInitSeed
	if err := yaml.Unmarshal([]byte(cloudInitSeed), &seed); err != nil {
		t.Fatalf("cloud-init seed is not valid YAML: %v\n%s", err, cloudInitSeed)
	}
	return seed
}

// TestCloudInitSeed_KeepsThePlannedLayout pins that cloud-init leaves the
// storage the agent laid out alone. Ubuntu's stock cloud.cfg runs growpart
// and resizefs on every boot: with a fixed-size root partition followed by
// free space (a 5G root on an 8G disk), the first boot grew the root
// partition to the end of the disk and ran xfs_growfs, so the installed
// system did not match the storage script.
func TestCloudInitSeed_KeepsThePlannedLayout(t *testing.T) {
	seed := parseCloudInitSeed(t)

	// The string "off", not YAML 1.1's bool false that an unquoted off
	// decodes to: cloud-init accepts false but logs it as deprecated.
	if mode, ok := seed.Growpart.Mode.(string); !ok || mode != "off" {
		t.Errorf("growpart.mode = %#v, want the string \"off\"\n%s", seed.Growpart.Mode, cloudInitSeed)
	}
	if rr, ok := seed.ResizeRootfs.(bool); !ok || rr {
		t.Errorf("resize_rootfs = %#v, want false\n%s", seed.ResizeRootfs, cloudInitSeed)
	}
}

// TestCloudInitSeed_RunsNoFirstBootApt pins that cc_locale is off. The image
// has no /etc/default/locale and no locales package, so cc_locale ran
// apt-get update and apt-get install locales on first boot to get
// update-locale; with no archive reachable it failed and
// cloud-config.service failed on every installed system.
func TestCloudInitSeed_RunsNoFirstBootApt(t *testing.T) {
	seed := parseCloudInitSeed(t)

	if l, ok := seed.Locale.(bool); !ok || l {
		t.Errorf("locale = %#v, want false\n%s", seed.Locale, cloudInitSeed)
	}
}

// TestCloudInitSeed_KeepsAgentNetworkAndHostname pins the seed's earlier
// duties so a rewrite of the seed cannot drop them: the None datasource, no
// cloud-init network rendering, and the agent-written hostname kept.
func TestCloudInitSeed_KeepsAgentNetworkAndHostname(t *testing.T) {
	seed := parseCloudInitSeed(t)

	if len(seed.DatasourceList) != 1 || seed.DatasourceList[0] != "None" {
		t.Errorf("datasource_list = %v, want [None]", seed.DatasourceList)
	}
	if ph, ok := seed.PreserveHostname.(bool); !ok || !ph {
		t.Errorf("preserve_hostname = %#v, want true", seed.PreserveHostname)
	}
	if cfg, ok := seed.Network.Config.(string); !ok || cfg != "disabled" {
		t.Errorf("network.config = %#v, want the string \"disabled\"", seed.Network.Config)
	}
}
