package model_test

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/model"
)

// ContentHash decides whether global pushes a snapshot at all
// (dispatcher.rebuild: equal hash = no SeqNum bump, no push). Two properties:
//
//   - sensitivity: changing any leaf field of a populated snapshot (other
//     than SeqNum) changes the hash; a field the hash ignores is a change a
//     worker would never receive;
//   - permutation invariance (metamorphic): reordering the collections the
//     doc says are sorted does not change the hash, else every rebuild with
//     a different NetBox iteration order would bump SeqNum and push.

func drawSnapshot(t *rapid.T) model.SiteSnapshot {
	ip := func(l string) string { return fmt.Sprintf("10.0.0.%d", rapid.IntRange(1, 4).Draw(t, l)) }
	mac := func(l string) string { return fmt.Sprintf("52:54:00:00:00:%02x", rapid.IntRange(1, 4).Draw(t, l)) }
	s := model.SiteSnapshot{
		SeqNum: rapid.Int64().Draw(t, "seq"),
		Site:   model.SiteInfo{Name: "n", Slug: "s"},
		DHCP:   model.DHCPConfig{Subnet: "10.0.0.0/24", Gateway: "10.0.0.1", EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200"},
	}
	for i := range rapid.IntRange(0, 3).Draw(t, "nMachines") {
		m := model.MachineSnapshot{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte{byte(i)}), State: model.MachineStateNew}
		// NetBox interface names are unique per device; MACs and IPs may repeat
		// (bond members share the bond MAC).
		for j := range rapid.IntRange(0, 3).Draw(t, "nIfaces") {
			m.Interfaces = append(m.Interfaces, model.SnapshotInterface{
				NetBoxID: j, Name: []string{"eth0", "eth1", "bond0"}[j%3] + strings.Repeat("x", j/3),
				MAC: mac("ifmac"), Type: rapid.SampledFrom([]string{"physical", "lag", "ipmi"}).Draw(t, "iftype"),
				IP: ip("ifip"), VLAN: rapid.IntRange(0, 2).Draw(t, "vlan"),
			})
		}
		s.Machines = append(s.Machines, m)
	}
	for i := range rapid.IntRange(0, 3).Draw(t, "nRes") {
		_ = i
		s.Reservations = append(s.Reservations, model.Reservation{MAC: mac("rmac"), IP: ip("rip")})
	}
	// ContentHash sorts slices by Owner alone; the design has one slice per
	// gateway, so owners are distinct here. With duplicate owners the hash
	// would depend on input order.
	for i := range rapid.IntRange(0, 3).Draw(t, "nSlices") {
		s.OOB.DynamicSlices = append(s.OOB.DynamicSlices, model.OOBDynamicSlice{
			Start: ip("ss"), End: ip("se"), Owner: fmt.Sprintf("10.0.1.%d", i),
		})
	}
	for i := range rapid.IntRange(0, 2).Draw(t, "nKeys") {
		s.SSHKeys = append(s.SSHKeys, fmt.Sprintf("ssh-ed25519 K%d", i%2))
	}
	return s
}

func permute[T any](t *rapid.T, xs []T, label string) []T {
	out := append([]T(nil), xs...)
	for i := len(out) - 1; i > 0; i-- {
		j := rapid.IntRange(0, i).Draw(t, label)
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func mustHash(t interface{ Fatal(...any) }, s model.SiteSnapshot) string {
	h, err := s.ContentHash()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestPropContentHashPermutationInvariant(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := drawSnapshot(t)
		want := mustHash(t, s)
		p := s
		p.Machines = permute(t, s.Machines, "pm")
		for i := range p.Machines {
			p.Machines[i].Interfaces = permute(t, p.Machines[i].Interfaces, "pi")
		}
		p.Reservations = permute(t, s.Reservations, "pr")
		p.OOB.DynamicSlices = permute(t, s.OOB.DynamicSlices, "ps")
		p.SSHKeys = permute(t, s.SSHKeys, "pk")
		if got := mustHash(t, p); got != want {
			t.Fatalf("hash changed under reordering:\norig %+v\nperm %+v", s, p)
		}
	})
}

// leafPaths appends the path (".A.B[0].C") of every exported leaf field
// reachable from v. Times, UUIDs and byte slices count as leaves.
func leafPaths(v reflect.Value, path string, out *[]string) {
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) || v.Type() == reflect.TypeOf(uuid.UUID{}) {
			*out = append(*out, path)
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			leafPaths(v.Field(i), path+"."+v.Type().Field(i).Name, out)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			*out = append(*out, path)
			return
		}
		for i := 0; i < v.Len(); i++ {
			leafPaths(v.Index(i), fmt.Sprintf("%s[%d]", path, i), out)
		}
	case reflect.Pointer:
		if !v.IsNil() {
			leafPaths(v.Elem(), path, out)
		}
	default:
		*out = append(*out, path)
	}
}

func TestContentHashSensitiveToEveryField(t *testing.T) {
	dev := 7
	base := model.SiteSnapshot{
		SeqNum: 1,
		Site:   model.SiteInfo{Name: "n", Slug: "s"},
		DHCP: model.DHCPConfig{Subnet: "a", Gateway: "b", EphemeralPoolStart: "c", EphemeralPoolEnd: "d",
			RelayPools:  []model.RelayPool{{Subnet: "e", RangeStart: "f", RangeEnd: "g", Gateway: "h"}},
			Promiscuous: true, DNS: []string{"i"}, LeaseSeconds: 5},
		Machines: []model.MachineSnapshot{{
			ID: uuid.New(), NetBoxDeviceID: &dev, NetBoxSerial: "a", Hostname: "b", FQDN: "c",
			Interfaces: []model.SnapshotInterface{{NetBoxID: 1, Name: "a", MAC: "b", Type: "c", IP: "d", LAG: "e", VLAN: 2, Gateway: "f", MTU: 9}},
			BMCIP:      "d", State: "e", DesiredOS: "f", CurrentOS: "g", Locked: true, PXEEligible: true, BMCUser: "h", BMCPass: "i", AMT: true,
			SystemSerial: "j", ChassisSerial: "k", SystemUUID: "l", Disks: []model.SnapshotDiskIdentity{{Serial: "m", WWN: "n"}},
			Manufacturer: "o", Model: "p", KernelParams: "q",
			StorageScript: &model.SnapshotStorageScript{Name: "r", Content: "s"},
			NetworkScript: &model.SnapshotNetworkScript{Name: "t", Content: "u"},
			Console:       &model.SnapshotConsole{Server: "v", ServerSerial: "w", Port: "x", Baud: 3},
		}},
		Images:       []model.Image{{ID: uuid.New(), Name: "a", Version: "b", Arch: "c", SHA256: "d", URL: "e", CreatedAt: time.Unix(5, 0)}},
		SSHKeys:      []string{"k"},
		OOB:          model.OOBConfig{Subnet: "a", Gateway: "b", DNS: []string{"c"}, NTP: []string{"d"}, DynamicSlices: []model.OOBDynamicSlice{{Start: "e", End: "f", Owner: "g"}}, LeaseSeconds: 4},
		Reservations: []model.Reservation{{MAC: "a", IP: "b"}},
	}
	var paths []string
	leafPaths(reflect.ValueOf(base), "", &paths)
	baseHash := mustHash(t, base)
	insensitive := 0
	for _, p := range paths {
		if p == ".SeqNum" {
			continue
		}
		cp := deepCopySnapshot(base)
		if !setLeaf(reflect.ValueOf(&cp).Elem(), p) {
			t.Fatalf("could not set %s", p)
		}
		if mustHash(t, cp) == baseHash {
			insensitive++
			t.Errorf("ContentHash ignores %s", p)
		}
	}
	t.Logf("leaf fields checked: %d, insensitive: %d", len(paths)-1, insensitive)
}

func deepCopySnapshot(s model.SiteSnapshot) model.SiteSnapshot {
	b := s
	b.DHCP.RelayPools = append([]model.RelayPool(nil), s.DHCP.RelayPools...)
	b.DHCP.DNS = append([]string(nil), s.DHCP.DNS...)
	b.Machines = nil
	for _, m := range s.Machines {
		mc := m
		mc.Interfaces = append([]model.SnapshotInterface(nil), m.Interfaces...)
		mc.Disks = append([]model.SnapshotDiskIdentity(nil), m.Disks...)
		if m.NetBoxDeviceID != nil {
			v := *m.NetBoxDeviceID
			mc.NetBoxDeviceID = &v
		}
		if m.StorageScript != nil {
			v := *m.StorageScript
			mc.StorageScript = &v
		}
		if m.NetworkScript != nil {
			v := *m.NetworkScript
			mc.NetworkScript = &v
		}
		if m.Console != nil {
			v := *m.Console
			mc.Console = &v
		}
		b.Machines = append(b.Machines, mc)
	}
	b.Images = append([]model.Image(nil), s.Images...)
	b.SSHKeys = append([]string(nil), s.SSHKeys...)
	b.OOB.DNS = append([]string(nil), s.OOB.DNS...)
	b.OOB.NTP = append([]string(nil), s.OOB.NTP...)
	b.OOB.DynamicSlices = append([]model.OOBDynamicSlice(nil), s.OOB.DynamicSlices...)
	b.Reservations = append([]model.Reservation(nil), s.Reservations...)
	return b
}

// setLeaf walks path (".A.B[0].C") from v and changes the leaf.
func setLeaf(v reflect.Value, path string) bool {
	if v.Kind() == reflect.Pointer {
		return setLeaf(v.Elem(), path)
	}
	if path == "" {
		switch {
		case v.Type() == reflect.TypeOf(time.Time{}):
			v.Set(reflect.ValueOf(time.Unix(999, 0)))
		case v.Type() == reflect.TypeOf(uuid.UUID{}):
			v.Set(reflect.ValueOf(uuid.New()))
		case v.Kind() == reflect.String:
			v.SetString(v.String() + "x")
		case v.Kind() == reflect.Bool:
			v.SetBool(!v.Bool())
		case v.Kind() >= reflect.Int && v.Kind() <= reflect.Int64:
			v.SetInt(v.Int() + 1)
		default:
			return false
		}
		return true
	}
	if v.Kind() == reflect.Pointer {
		return setLeaf(v.Elem(), path)
	}
	if path[0] == '.' {
		rest := path[1:]
		end := len(rest)
		for i, c := range rest {
			if c == '.' || c == '[' {
				end = i
				break
			}
		}
		return setLeaf(v.FieldByName(rest[:end]), rest[end:])
	}
	if path[0] == '[' {
		end := strings.IndexByte(path, ']')
		idx, err := strconv.Atoi(path[1:end])
		if err != nil {
			return false
		}
		return setLeaf(v.Index(idx), path[end+1:])
	}
	return false
}
