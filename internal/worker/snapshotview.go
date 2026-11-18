package worker

import (
	"net"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// snapshotView is the immutable, snapshot-derived read state for one site.
// It is built once per ApplySnapshot and published behind
// Cache.view (an atomic.Pointer): pure reads (the DHCP / TFTP / HTTP-boot
// serving hot path) load it lock-free and never block on an apply — not
// even on the apply's minutes-long image sync, which runs after the view
// is already swapped in.
//
// "Immutable" means: once buildView returns and the pointer is stored, no
// field is mutated. The two pieces of state that DO change between applies
// — a machine's local State (RecordPhoneHome) and an op's Status/Phase
// (claim, phase, fail) — live outside the view, in the Cache's
// stateMu-guarded mutable layer (ops map, machineStateOverride). A machine
// snapshot carried here therefore holds the State the SERVER sent; callers
// that need the local override go through Cache accessors that merge it
// (GetMachineByID/ByMAC, MachineState, BootActionForMAC).
//
// Split out of Cache (fable-review Tier 1 #4).
type snapshotView struct {
	seqNum int64

	machines        map[uuid.UUID]*model.MachineSnapshot // keyed by machine ID
	macIndex        map[string]uuid.UUID                 // physical/non-ipmi MAC -> machine ID
	ambiguousMACs   map[string]struct{}                  // physical MACs modeled on multiple machines
	ipByMAC         map[string]string                    // physical MAC -> assigned IP (CIDR or bare)
	oobReservations map[string]string                    // MAC(lower) -> IP, full OOB set incl. non-machines
	images          map[string]*model.Image              // keyed by image name
	sshKeys         []string
	dhcpCfg         model.DHCPConfig
	oobCfg          model.OOBConfig
	siteInfo        model.SiteInfo
}

// emptyView is the zero view a Cache starts from before any snapshot or
// rehydration. All maps are non-nil so readers never nil-check.
func emptyView() *snapshotView {
	return &snapshotView{
		machines:        map[uuid.UUID]*model.MachineSnapshot{},
		macIndex:        map[string]uuid.UUID{},
		ambiguousMACs:   map[string]struct{}{},
		ipByMAC:         map[string]string{},
		oobReservations: map[string]string{},
		images:          map[string]*model.Image{},
	}
}

// machineByMAC resolves a MAC to its machine via the MAC index. Returns
// (nil, false) for an unknown MAC or a MAC whose machine is absent.
func (v *snapshotView) machineByMAC(mac string) (*model.MachineSnapshot, bool) {
	id, ok := v.macIndex[mac]
	if !ok {
		return nil, false
	}
	m, ok := v.machines[id]
	return m, ok
}

// indexMachineInterfaces populates macIndex and ipByMAC for one machine,
// applying the physical-only filter: a BMC/mgmt_only (ipmi)
// interface is never a host-OS provisioning DHCP client, so its MAC must
// not make the machine "known" to the lease filter, and it never
// contributes a host-fabric reservation. Shared by buildView and the
// NewCache rehydration path so the filter lives in exactly one place.
func (v *snapshotView) indexMachineInterfaces(id uuid.UUID, ifaces []model.SnapshotInterface) {
	for _, iface := range ifaces {
		if iface.MAC == "" || iface.Type == "ipmi" {
			continue
		}
		if _, ambiguous := v.ambiguousMACs[iface.MAC]; ambiguous {
			continue
		}
		if existingID, exists := v.macIndex[iface.MAC]; exists && existingID != id {
			// A shared MAC cannot establish machine identity or receive an
			// arbitrary machine's reservation. Remember the ambiguity so a
			// third occurrence cannot silently re-add it after deletion.
			delete(v.macIndex, iface.MAC)
			delete(v.ipByMAC, iface.MAC)
			v.ambiguousMACs[iface.MAC] = struct{}{}
			continue
		}
		v.macIndex[iface.MAC] = id
		if iface.IP != "" {
			v.ipByMAC[iface.MAC] = iface.IP
		}
	}
}

// reservedIP returns the IP reserved for a MAC, or nil if none. Machine
// interface IPs (ipByMAC) win over the bulk OOB reservation set on
// conflict; the host portion is extracted from CIDR form.
func (v *snapshotView) reservedIP(mac string) net.IP {
	addr, ok := v.ipByMAC[mac]
	if !ok || addr == "" {
		addr, ok = v.oobReservations[mac]
	}
	if !ok || addr == "" {
		return nil
	}
	if ip, _, err := net.ParseCIDR(addr); err == nil {
		return ip
	}
	return net.ParseIP(addr)
}
