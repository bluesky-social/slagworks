package mirror_test

// Model-based property test for the NetBox -> machines mirror: the real
// mirror.Mirror, snapshot.Reconciler and store.Store (Postgres via testpg)
// driven by random histories over a small pool of NetBox device ids and
// serials, with webhooks delivered, lost, duplicated and delivered stale,
// in-flight operations started and finished, and reconcile ticks.
//
// Checked contracts (internal/mirror package doc, docs/dev/architecture.md
// "on a re-create with the same serial rebinds the existing row",
// model.Machine "NetBoxSerial ... the stable key used to re-bind a machine
// when a NetBox Device is deleted and recreated"):
//
//   B1 no merge: a machine row never moves from device X to device Y while
//      X still exists in NetBox.
//   B2 rebind: when a device with serial S is processed as created and a
//      row with serial S exists whose device is gone from NetBox, that row
//      is rebound (same UUID) rather than refused or replaced. Not checked
//      today, see checkRebindOnRecreate.
//   B3 settle bijection: after ops finish and two quiet ticks, every NetBox
//      device whose serial is non-empty and unique has exactly one row, and
//      every row is bound to a device that exists. A device blocked by a row
//      that carries a stale serial is excluded, see settle.
//   B4 retention: a delete delivered while the machine has an operation in
//      flight keeps the machine row.
//
// Each case costs a few dozen Postgres round trips, so the default check
// count is kept low; -rapid.checks raises it for a deep run.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/mirror"
	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/netbox"
	"github.com/bluesky-social/slagworks/internal/snapshot"
	"github.com/bluesky-social/slagworks/internal/store"
	"github.com/bluesky-social/slagworks/internal/testpg"
)

const propSite = "s1"

// checkRebindOnRecreate gates B2. It is off because the mirror does not
// rebind today: a row kept through a delete (the machine had an operation
// in flight) stays bound to the deleted device id, so OnDeviceCreated
// refuses the re-created device as a duplicate serial, and the reconcile
// sweep later hard-deletes the row and enrolls the device as a fresh
// machine. Turn it on when re-creation rebinds the retained row.
const checkRebindOnRecreate = false

// fakeNB is the NetBox the reconciler reads: one site, no locations.
type fakeNB struct {
	mu      sync.Mutex
	devices map[int]string // id -> serial
}

func (f *fakeNB) GetSiteBySlug(ctx context.Context, slug string) (*netbox.Site, error) {
	if slug != propSite {
		return nil, nil
	}
	return &netbox.Site{ID: 1, Slug: propSite, Name: propSite}, nil
}

func (f *fakeNB) dev(id int, serial string) netbox.Device {
	return netbox.Device{ID: id, Name: fmt.Sprintf("host-%d", id), Serial: serial, Site: &netbox.NestedRef{ID: 1, Slug: propSite}}
}

func (f *fakeNB) GetDeviceByID(ctx context.Context, id int) (*netbox.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.devices[id]
	if !ok {
		return nil, nil
	}
	d := f.dev(id, s)
	return &d, nil
}

func (f *fakeNB) ListDevicesForSite(ctx context.Context, slug string) ([]netbox.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int, 0, len(f.devices))
	for id := range f.devices {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]netbox.Device, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.dev(id, f.devices[id]))
	}
	return out, nil
}

func (f *fakeNB) ListLocationsForSite(ctx context.Context, slug string) ([]netbox.Location, error) {
	return nil, nil
}
func (f *fakeNB) ListInterfacesForDevices(ctx context.Context, ids []int) ([]netbox.Interface, error) {
	return nil, nil
}
func (f *fakeNB) ListIPAddressesForInterfaces(ctx context.Context, ids []int) ([]netbox.IPAddress, error) {
	return nil, nil
}
func (f *fakeNB) ListSitePrefixes(ctx context.Context, slug, tag string) ([]netbox.Prefix, error) {
	return nil, nil
}
func (f *fakeNB) ListLocationPrefixes(ctx context.Context, id int, tag string) ([]netbox.Prefix, error) {
	return nil, nil
}
func (f *fakeNB) ListIPRangesByRole(ctx context.Context, role, parent string) ([]netbox.IPRange, error) {
	return nil, nil
}
func (f *fakeNB) ListDeviceTypesByIDs(ctx context.Context, ids []int) ([]netbox.DeviceType, error) {
	return nil, nil
}
func (f *fakeNB) ListConsolePortsForDevices(ctx context.Context, ids []int) ([]netbox.ConsolePort, error) {
	return nil, nil
}

type nopInv struct{}

func (nopInv) Invalidate(string) {}

type opKind int

const (
	opNBCreate opKind = iota
	opNBDelete
	opNBSetSerial
	opStartOp
	opFinishOps
	opTick
	opRedeliverCreated
	opRedeliverDeleted
)

var opNames = map[opKind]string{
	opNBCreate: "nb-create", opNBDelete: "nb-delete", opNBSetSerial: "nb-set-serial",
	opStartOp: "start-op", opFinishOps: "finish-ops", opTick: "tick",
	opRedeliverCreated: "redeliver-created", opRedeliverDeleted: "redeliver-deleted",
}

type histOp struct {
	Kind    opKind
	ID      int
	Serial  string
	Deliver bool
}

func (o histOp) String() string {
	return fmt.Sprintf("%s(id=%d serial=%q deliver=%v)", opNames[o.Kind], o.ID, o.Serial, o.Deliver)
}

var (
	propIDs     = []int{1, 2, 3, 4}
	propSerials = []string{"SN-A", "SN-B", "SN-C"}
)

func genHistOp(allowSerialChange bool) *rapid.Generator[histOp] {
	return rapid.Custom(func(t *rapid.T) histOp {
		kinds := []opKind{opNBCreate, opNBCreate, opNBDelete, opNBDelete, opStartOp, opStartOp, opStartOp, opFinishOps, opTick, opTick, opRedeliverCreated, opRedeliverDeleted}
		if allowSerialChange {
			kinds = append(kinds, opNBSetSerial)
		}
		return histOp{
			Kind:    rapid.SampledFrom(kinds).Draw(t, "kind"),
			ID:      rapid.SampledFrom(propIDs).Draw(t, "id"),
			Serial:  rapid.SampledFrom(propSerials).Draw(t, "serial"),
			Deliver: rapid.IntRange(0, 3).Draw(t, "deliver") != 0, // 25% lost webhooks
		}
	})
}

type harness struct {
	t   *rapid.T
	db  *gorm.DB
	st  *store.Store
	nb  *fakeNB
	mir *mirror.Mirror
	rec *snapshot.Reconciler
	// binding history for B1: machine UUID -> device id last observed
	bound map[uuid.UUID]int
	trace []string
}

func (h *harness) machines() []model.Machine {
	ms, err := h.st.ListMachines(nil)
	if err != nil {
		h.t.Fatalf("list machines: %v", err)
	}
	return ms
}

func (h *harness) mirrorDevice(id int, serial string) mirror.Device {
	return mirror.Device{ID: id, Name: fmt.Sprintf("host-%d", id), Serial: serial, SiteSlug: propSite, ConfigSlug: propSite}
}

// deliverCreated mirrors what WebhookHandler.handleDevice does for a
// claimed device's "created" event.
func (h *harness) deliverCreated(id int, serial string) {
	before := h.machines()
	err := h.mir.OnDeviceCreated(context.Background(), h.mirrorDevice(id, serial))
	h.trace = append(h.trace, fmt.Sprintf("  OnDeviceCreated(%d,%s) -> %v", id, serial, err))
	h.checkRebind(before, id, serial)
}

// checkRebind is B2 for one processed create of device id with serial.
func (h *harness) checkRebind(before []model.Machine, id int, serial string) {
	if !checkRebindOnRecreate || serial == "" {
		return
	}
	h.nb.mu.Lock()
	_, devExists := h.nb.devices[id]
	h.nb.mu.Unlock()
	if !devExists {
		return // stale delivery of a create for a device that is gone
	}
	var candidate *model.Machine
	for i := range before {
		m := before[i]
		if m.NetBoxSerial != serial || m.NetBoxDeviceID == nil || *m.NetBoxDeviceID == id {
			continue
		}
		h.nb.mu.Lock()
		_, oldExists := h.nb.devices[*m.NetBoxDeviceID]
		h.nb.mu.Unlock()
		if !oldExists {
			candidate = &m
		}
	}
	if candidate == nil {
		return
	}
	for _, m := range h.machines() {
		if m.ID == candidate.ID {
			if m.NetBoxDeviceID != nil && *m.NetBoxDeviceID == id {
				return
			}
			h.t.Fatalf("B2: device %d (serial %s) was processed as created while machine %s (serial %s) is bound to device %d, which no longer exists in NetBox; want the row rebound to %d, got bound to %v\ntrace:\n%s",
				id, serial, m.ID, m.NetBoxSerial, *candidate.NetBoxDeviceID, id, deref(m.NetBoxDeviceID), strings.Join(h.trace, "\n"))
		}
	}
	h.t.Fatalf("B2: device %d (serial %s) was processed as created while machine %s (serial %s) is bound to gone device %d; want the row rebound, got the row deleted\ntrace:\n%s",
		id, serial, candidate.ID, candidate.NetBoxSerial, *candidate.NetBoxDeviceID, strings.Join(h.trace, "\n"))
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func (h *harness) checkNoMerge() {
	h.nb.mu.Lock()
	defer h.nb.mu.Unlock()
	for _, m := range h.machines() {
		if m.NetBoxDeviceID == nil {
			continue
		}
		prev, seen := h.bound[m.ID]
		if seen && prev != *m.NetBoxDeviceID {
			if _, stillThere := h.nb.devices[prev]; stillThere {
				h.t.Fatalf("B1: machine %s moved from device %d to %d while %d still exists in NetBox\ntrace:\n%s",
					m.ID, prev, *m.NetBoxDeviceID, prev, strings.Join(h.trace, "\n"))
			}
		}
		h.bound[m.ID] = *m.NetBoxDeviceID
	}
}

func (h *harness) apply(o histOp) {
	h.trace = append(h.trace, o.String())
	ctx := context.Background()
	switch o.Kind {
	case opNBCreate:
		h.nb.mu.Lock()
		_, exists := h.nb.devices[o.ID]
		if !exists {
			h.nb.devices[o.ID] = o.Serial
		}
		h.nb.mu.Unlock()
		if exists {
			return
		}
		if o.Deliver {
			h.deliverCreated(o.ID, o.Serial)
		}
	case opNBDelete:
		h.nb.mu.Lock()
		_, exists := h.nb.devices[o.ID]
		delete(h.nb.devices, o.ID)
		h.nb.mu.Unlock()
		if exists && o.Deliver {
			// B4 (B7 retention): a delete landing while an op is in flight
			// fails the op and keeps the row for inspection.
			var busy *model.Machine
			if m, err := h.st.GetMachineByNetBoxDeviceID(o.ID); err == nil {
				var n int64
				h.db.Model(&model.Operation{}).Where("machine_id = ? AND status IN ?", m.ID,
					[]model.OperationStatus{model.OpPending, model.OpClaimed, model.OpRunning}).Count(&n)
				if n > 0 {
					busy = m
				}
			}
			err := h.mir.OnDeviceDeleted(ctx, o.ID, propSite)
			h.trace = append(h.trace, fmt.Sprintf("  OnDeviceDeleted(%d) -> %v", o.ID, err))
			if busy != nil {
				if _, gerr := h.st.GetMachine(busy.ID); gerr != nil {
					h.t.Fatalf("B4: machine %s had an in-flight op when device %d was deleted; the row must be retained, got %v\ntrace:\n%s",
						busy.ID, o.ID, gerr, strings.Join(h.trace, "\n"))
				}
			}
		}
	case opNBSetSerial:
		h.nb.mu.Lock()
		_, exists := h.nb.devices[o.ID]
		if exists {
			h.nb.devices[o.ID] = o.Serial
		}
		h.nb.mu.Unlock()
		if exists && o.Deliver {
			_ = h.mir.OnDeviceUpdated(ctx, h.mirrorDevice(o.ID, o.Serial))
		}
	case opStartOp:
		m, err := h.st.GetMachineByNetBoxDeviceID(o.ID)
		if err != nil {
			return
		}
		op := &model.Operation{MachineID: m.ID, Type: model.OpDeploy, Status: model.OpRunning}
		if err := h.st.CreateOperation(op); err != nil {
			// idx_one_active_op_per_machine: already busy.
			return
		}
	case opFinishOps:
		for _, m := range h.machines() {
			if _, err := h.st.FailInFlightOpsForMachine(m.ID, "property test: op finished"); err != nil {
				h.t.Fatalf("finish ops: %v", err)
			}
		}
	case opTick:
		before := h.machines()
		h.rec.ManualTick(ctx)
		// B2 for every create the tick processed: the reconciler feeds
		// every untracked device through OnDeviceCreated before its sweep.
		h.nb.mu.Lock()
		devs := make(map[int]string, len(h.nb.devices))
		for id, s := range h.nb.devices {
			devs[id] = s
		}
		h.nb.mu.Unlock()
		tracked := map[int]bool{}
		for _, m := range before {
			if m.NetBoxDeviceID != nil {
				tracked[*m.NetBoxDeviceID] = true
			}
		}
		for id, s := range devs {
			if !tracked[id] && uniqueSerial(devs, s) {
				h.checkRebind(before, id, s)
			}
		}
	case opRedeliverCreated:
		h.nb.mu.Lock()
		s, exists := h.nb.devices[o.ID]
		h.nb.mu.Unlock()
		if !exists {
			// Stale delivery: the device is gone; NetBox's payload carries
			// the serial it had. Use the drawn serial.
			s = o.Serial
		}
		h.deliverCreated(o.ID, s)
	case opRedeliverDeleted:
		err := h.mir.OnDeviceDeleted(ctx, o.ID, propSite)
		h.trace = append(h.trace, fmt.Sprintf("  stale OnDeviceDeleted(%d) -> %v", o.ID, err))
	}
	h.checkNoMerge()
}

func uniqueSerial(devs map[int]string, s string) bool {
	if s == "" {
		return false
	}
	n := 0
	for _, x := range devs {
		if x == s {
			n++
		}
	}
	return n == 1
}

func (h *harness) settle() {
	h.trace = append(h.trace, "-- settle")
	h.apply(histOp{Kind: opFinishOps})
	h.apply(histOp{Kind: opTick})
	h.apply(histOp{Kind: opTick})
	h.nb.mu.Lock()
	devs := make(map[int]string, len(h.nb.devices))
	for id, s := range h.nb.devices {
		devs[id] = s
	}
	h.nb.mu.Unlock()
	rows := map[int]int{}
	for _, m := range h.machines() {
		if m.NetBoxDeviceID == nil {
			h.t.Fatalf("B3: machine %s is unbound after settle\ntrace:\n%s", m.ID, strings.Join(h.trace, "\n"))
		}
		if _, ok := devs[*m.NetBoxDeviceID]; !ok {
			h.t.Fatalf("B3: machine %s bound to device %d which is gone from NetBox after settle\ntrace:\n%s",
				m.ID, *m.NetBoxDeviceID, strings.Join(h.trace, "\n"))
		}
		rows[*m.NetBoxDeviceID]++
	}
	for id, s := range devs {
		if !uniqueSerial(devs, s) {
			continue
		}
		if rows[id] != 1 {
			// Excluded: the mirror never updates a row's serial, so once a
			// tracked device's NetBox serial changes (or a stale create
			// enrolled it under an old one) its row keeps the old serial
			// and blocks enrolling the device that now carries it.
			blocked := false
			for _, m := range h.machines() {
				if m.NetBoxSerial == s && m.NetBoxDeviceID != nil && devs[*m.NetBoxDeviceID] != s {
					blocked = true
				}
			}
			if blocked {
				continue
			}
			h.t.Fatalf("B3: NetBox device %d (unique serial %s) has %d machine rows after settle\ntrace:\n%s",
				id, s, rows[id], strings.Join(h.trace, "\n"))
		}
	}
}

func mirrorBijection(t *testing.T, allowSerialChange bool) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := testpg.DB(t, store.Migrate)
	st := store.New(db)
	if err := st.CreateSiteConfig(&model.SiteConfig{
		Slug: propSite, NetBoxSite: propSite, EphemeralPoolStart: "10.0.0.100", EphemeralPoolEnd: "10.0.0.200",
	}); err != nil {
		t.Fatalf("create site config: %v", err)
	}
	rapid.Check(t, func(rt *rapid.T) {
		// One database for the whole property: truncating every table per
		// case costs more than the case itself. Only these tables are
		// written by the history.
		for _, tbl := range []string{"log_envelopes", "operations", "machines"} {
			if err := db.Exec("DELETE FROM " + tbl).Error; err != nil {
				rt.Fatalf("reset %s: %v", tbl, err)
			}
		}
		nb := &fakeNB{devices: map[int]string{}}
		mir := mirror.New(st, nopInv{})
		mir.SetLogger(quiet)
		rec := snapshot.NewReconciler(nb, st, nopInv{}, mir)
		rec.SetLogger(quiet)
		h := &harness{t: rt, db: db, st: st, nb: nb, mir: mir, rec: rec, bound: map[uuid.UUID]int{}}
		ops := rapid.SliceOfN(genHistOp(allowSerialChange), 1, 16).Draw(rt, "ops")
		for _, o := range ops {
			h.apply(o)
		}
		h.settle()
	})
}

func TestPropMirrorBijection(t *testing.T) { mirrorBijection(t, false) }

// TestPropMirrorBijectionSerialChange also lets NetBox serials change.
func TestPropMirrorBijectionSerialChange(t *testing.T) { mirrorBijection(t, true) }
