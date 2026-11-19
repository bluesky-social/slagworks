package model

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"
)

// Property coverage for SiteSnapshot.ContentHash, the dispatcher's
// push-decision key. Two directions matter and they fail differently:
//
//   - equal content must hash equal regardless of slice order (a violation
//     is a spurious push + SeqNum bump every rebuild);
//   - ANY change to a field the worker consumes must change the hash (a
//     violation is a missed dispatch: the worker keeps serving a stale boot
//     decision until some unrelated field changes).
//
// The sensitivity check walks the struct by reflection instead of listing
// fields, so a field added to the snapshot later is covered without touching
// this file — and a field that is silently excluded from the hash (json:"-",
// unexported, or dropped by a custom marshaller) fails the property.

var propPool = struct {
	macs, names, ips, strs []string
}{
	macs:  []string{"", "aa:00:00:00:00:01", "aa:00:00:00:00:02", "aa:00:00:00:00:03"},
	names: []string{"eth0", "eth1", "bond0", "ipmi0", "lo"},
	ips:   []string{"", "10.0.0.1/24", "10.0.0.2/24", "10.0.1.9/31"},
	strs:  []string{"", "a", "b", "c"},
}

func propUUID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", n))
}

func genPropSnapshot(t *rapid.T) SiteSnapshot {
	str := rapid.SampledFrom(propPool.strs)
	s := SiteSnapshot{
		SeqNum: rapid.Int64Range(0, 5).Draw(t, "seq"),
		Site:   SiteInfo{Name: str.Draw(t, "siteName"), Slug: str.Draw(t, "siteSlug")},
		DHCP: DHCPConfig{
			Subnet: str.Draw(t, "subnet"), Gateway: str.Draw(t, "gw"),
			EphemeralPoolStart: str.Draw(t, "eps"), EphemeralPoolEnd: str.Draw(t, "epe"),
			Promiscuous:  rapid.Bool().Draw(t, "promisc"),
			LeaseSeconds: rapid.IntRange(0, 2).Draw(t, "lease"),
			DNS:          rapid.SliceOfN(str, 0, 2).Draw(t, "dns"),
		},
		OOB: OOBConfig{
			Subnet: str.Draw(t, "oobSubnet"), Gateway: str.Draw(t, "oobGw"),
			DNS: rapid.SliceOfN(str, 0, 2).Draw(t, "oobDNS"), NTP: rapid.SliceOfN(str, 0, 2).Draw(t, "oobNTP"),
			LeaseSeconds: rapid.IntRange(0, 2).Draw(t, "oobLease"),
		},
		SSHKeys: rapid.SliceOfN(str, 0, 3).Draw(t, "keys"),
	}
	for i := range rapid.IntRange(0, 2).Draw(t, "nRelay") {
		s.DHCP.RelayPools = append(s.DHCP.RelayPools, RelayPool{
			Subnet: fmt.Sprintf("10.9.%d.0/31", i), RangeStart: str.Draw(t, "rs"), RangeEnd: str.Draw(t, "re"), Gateway: str.Draw(t, "rg"),
		})
	}
	// Dynamic slices: Build rejects duplicate owners, so owners are unique.
	for i := range rapid.IntRange(0, 3).Draw(t, "nSlices") {
		s.OOB.DynamicSlices = append(s.OOB.DynamicSlices, OOBDynamicSlice{
			Start: str.Draw(t, "ss"), End: str.Draw(t, "se"), Owner: fmt.Sprintf("10.8.0.%d", i),
		})
	}
	for i := range rapid.IntRange(0, 3).Draw(t, "nRes") {
		s.Reservations = append(s.Reservations, Reservation{
			MAC: rapid.SampledFrom(propPool.macs).Draw(t, "resMAC"), IP: fmt.Sprintf("10.7.0.%d", i),
		})
	}
	for i := range rapid.IntRange(0, 3).Draw(t, "nImages") {
		s.Images = append(s.Images, Image{
			ID: propUUID(100 + i), Name: rapid.SampledFrom([]string{"img-a", "img-b"}).Draw(t, "imgName"),
			Version: str.Draw(t, "imgVer"), Arch: "amd64", SHA256: str.Draw(t, "imgSHA"), URL: str.Draw(t, "imgURL"),
			CreatedAt: time.Unix(int64(1700000000+rapid.IntRange(0, 2).Draw(t, "imgT")), 0).UTC(),
		})
	}
	for i := range rapid.IntRange(0, 4).Draw(t, "nMachines") {
		m := MachineSnapshot{
			ID:           propUUID(i + 1),
			NetBoxSerial: str.Draw(t, "serial"), Hostname: str.Draw(t, "host"), FQDN: str.Draw(t, "fqdn"),
			BMCIP:     str.Draw(t, "bmcip"),
			State:     MachineState(rapid.SampledFrom([]string{"new", "ready", "deployed", "failed"}).Draw(t, "state")),
			DesiredOS: str.Draw(t, "dos"), CurrentOS: str.Draw(t, "cos"),
			Locked: rapid.Bool().Draw(t, "locked"), PXEEligible: rapid.Bool().Draw(t, "pxe"),
			BMCUser: str.Draw(t, "bu"), BMCPass: str.Draw(t, "bp"), AMT: rapid.Bool().Draw(t, "amt"),
			SystemSerial: str.Draw(t, "ss"), ChassisSerial: str.Draw(t, "cs"), SystemUUID: str.Draw(t, "su"),
			Manufacturer: str.Draw(t, "mf"), Model: str.Draw(t, "model"), KernelParams: str.Draw(t, "kp"),
		}
		if rapid.Bool().Draw(t, "hasDev") {
			id := rapid.IntRange(1, 9).Draw(t, "devID")
			m.NetBoxDeviceID = &id
		}
		// Interface names are unique per device in NetBox; MACs and IPs are
		// free to collide (bond members share a MAC).
		nIf := rapid.IntRange(0, len(propPool.names)).Draw(t, "nIf")
		for j := range nIf {
			m.Interfaces = append(m.Interfaces, SnapshotInterface{
				NetBoxID: 10*i + j, Name: propPool.names[j],
				MAC:  rapid.SampledFrom(propPool.macs).Draw(t, "mac"),
				Type: rapid.SampledFrom([]string{"physical", "ipmi", "lag"}).Draw(t, "ifType"),
				IP:   rapid.SampledFrom(propPool.ips).Draw(t, "ip"),
				LAG:  rapid.SampledFrom([]string{"", "bond0"}).Draw(t, "lag"),
				VLAN: rapid.IntRange(0, 2).Draw(t, "vlan"), Gateway: str.Draw(t, "ifgw"), MTU: rapid.IntRange(0, 1).Draw(t, "mtu") * 9000,
			})
		}
		for range rapid.IntRange(0, 2).Draw(t, "nDisks") {
			m.Disks = append(m.Disks, SnapshotDiskIdentity{Serial: str.Draw(t, "ds"), WWN: str.Draw(t, "dw")})
		}
		if rapid.Bool().Draw(t, "hasSS") {
			m.StorageScript = &SnapshotStorageScript{Name: str.Draw(t, "ssn"), Content: str.Draw(t, "ssc")}
		}
		if rapid.Bool().Draw(t, "hasNS") {
			m.NetworkScript = &SnapshotNetworkScript{Name: str.Draw(t, "nsn"), Content: str.Draw(t, "nsc")}
		}
		if rapid.Bool().Draw(t, "hasCon") {
			m.Console = &SnapshotConsole{Server: str.Draw(t, "csrv"), ServerSerial: str.Draw(t, "cser"), Port: str.Draw(t, "cport"), Baud: 115200}
		}
		s.Machines = append(s.Machines, m)
	}
	return s
}

// propDeepCopy returns a snapshot sharing no backing arrays or pointers with
// the input, built by reflection so it cannot drift from the struct shape.
func propDeepCopy(s SiteSnapshot) SiteSnapshot {
	out := reflect.New(reflect.TypeOf(s)).Elem()
	propCopyValue(out, reflect.ValueOf(s))
	return out.Interface().(SiteSnapshot)
}

func propCopyValue(dst, src reflect.Value) {
	switch src.Kind() {
	case reflect.Pointer:
		if src.IsNil() {
			return
		}
		dst.Set(reflect.New(src.Type().Elem()))
		propCopyValue(dst.Elem(), src.Elem())
	case reflect.Slice:
		if src.IsNil() {
			return
		}
		dst.Set(reflect.MakeSlice(src.Type(), src.Len(), src.Len()))
		for i := range src.Len() {
			propCopyValue(dst.Index(i), src.Index(i))
		}
	case reflect.Struct:
		if src.Type() == reflect.TypeOf(time.Time{}) {
			dst.Set(src)
			return
		}
		for i := range src.NumField() {
			propCopyValue(dst.Field(i), src.Field(i))
		}
	default:
		dst.Set(src)
	}
}

func propShuffle(s *SiteSnapshot, seed int64) {
	r := rand.New(rand.NewSource(seed))
	r.Shuffle(len(s.Machines), func(i, j int) { s.Machines[i], s.Machines[j] = s.Machines[j], s.Machines[i] })
	for k := range s.Machines {
		ifs := s.Machines[k].Interfaces
		r.Shuffle(len(ifs), func(i, j int) { ifs[i], ifs[j] = ifs[j], ifs[i] })
	}
	r.Shuffle(len(s.Images), func(i, j int) { s.Images[i], s.Images[j] = s.Images[j], s.Images[i] })
	r.Shuffle(len(s.SSHKeys), func(i, j int) { s.SSHKeys[i], s.SSHKeys[j] = s.SSHKeys[j], s.SSHKeys[i] })
	r.Shuffle(len(s.Reservations), func(i, j int) {
		s.Reservations[i], s.Reservations[j] = s.Reservations[j], s.Reservations[i]
	})
	r.Shuffle(len(s.OOB.DynamicSlices), func(i, j int) {
		s.OOB.DynamicSlices[i], s.OOB.DynamicSlices[j] = s.OOB.DynamicSlices[j], s.OOB.DynamicSlices[i]
	})
}

// TestPropContentHashOrderInvariant: the collections ContentHash documents
// as order-free hash identically under any permutation, and hashing never
// mutates the receiver.
func TestPropContentHashOrderInvariant(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := genPropSnapshot(t)
		b := propDeepCopy(a)
		propShuffle(&b, rapid.Int64().Draw(t, "shuffleSeed"))
		before := propDeepCopy(b)
		ha, err := a.ContentHash()
		if err != nil {
			t.Fatal(err)
		}
		hb, err := b.ContentHash()
		if err != nil {
			t.Fatal(err)
		}
		if ha != hb {
			t.Fatalf("hash depends on slice order:\n a=%+v\n b=%+v", a, b)
		}
		if !reflect.DeepEqual(b, before) {
			t.Fatalf("ContentHash mutated its receiver")
		}
		// SeqNum is the only excluded field.
		b.SeqNum = a.SeqNum + 7
		if hb2, _ := b.ContentHash(); hb2 != ha {
			t.Fatalf("hash depends on SeqNum")
		}
	})
}

// propLeaf is one mutable leaf reachable from a snapshot.
type propLeaf struct {
	path string
	v    reflect.Value
}

func propLeaves(path string, v reflect.Value, out *[]propLeaf) {
	switch v.Kind() {
	case reflect.Pointer:
		// The pointer itself (nil <-> set) is a leaf; so is everything under it.
		*out = append(*out, propLeaf{path + "(ptr)", v})
		if !v.IsNil() {
			propLeaves(path, v.Elem(), out)
		}
	case reflect.Slice:
		*out = append(*out, propLeaf{path + "(len)", v})
		for i := range v.Len() {
			propLeaves(fmt.Sprintf("%s[%d]", path, i), v.Index(i), out)
		}
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			*out = append(*out, propLeaf{path, v})
			return
		}
		for i := range v.NumField() {
			propLeaves(path+"."+v.Type().Field(i).Name, v.Field(i), out)
		}
	case reflect.Array:
		*out = append(*out, propLeaf{path, v}) // uuid.UUID
	default:
		*out = append(*out, propLeaf{path, v})
	}
}

// propMutate changes one leaf to a different value of the same type.
func propMutate(l propLeaf) {
	v := l.v
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "~")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Array:
		b := v.Index(0)
		b.SetUint(b.Uint() ^ 0x80)
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		} else {
			v.Set(reflect.Zero(v.Type()))
		}
	case reflect.Slice:
		// Append one zero element (a non-empty element where the zero
		// element would be indistinguishable is handled by element leaves).
		el := reflect.New(v.Type().Elem()).Elem()
		if el.Kind() == reflect.String {
			el.SetString("~new")
		}
		v.Set(reflect.Append(v, el))
	case reflect.Struct: // time.Time
		v.Set(reflect.ValueOf(v.Interface().(time.Time).Add(time.Second)))
	default:
		panic("unhandled leaf kind " + v.Kind().String() + " at " + l.path)
	}
}

// TestPropContentHashSensitiveToEveryField: mutating any single leaf of a
// snapshot other than SeqNum changes the hash. A nil->empty pointer flip on
// an omitempty pointer-to-struct is still a JSON-visible change ("{}" vs
// absent), so it is included.
func TestPropContentHashSensitiveToEveryField(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		base := genPropSnapshot(t)
		h0, err := base.ContentHash()
		if err != nil {
			t.Fatal(err)
		}
		mut := propDeepCopy(base)
		var leaves []propLeaf
		propLeaves("snap", reflect.ValueOf(&mut).Elem(), &leaves)
		l := leaves[rapid.IntRange(0, len(leaves)-1).Draw(t, "leaf")]
		if l.path == "snap.SeqNum" {
			t.Skip("SeqNum is excluded by contract")
		}
		// Appending a zero-valued element to an omitempty-free slice is a
		// visible change; appending "" to a []string is too (JSON [""]).
		propMutate(l)
		h1, err := mut.ContentHash()
		if err != nil {
			t.Fatal(err)
		}
		if h0 == h1 {
			t.Fatalf("mutating %s did not change the content hash (missed dispatch)\n base=%+v\n mut =%+v", l.path, base, mut)
		}
	})
}
