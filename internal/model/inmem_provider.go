package model

import (
	"fmt"
	"sync"

	"github.com/google/uuid"
)

type machineStatus int

const (
	statusUnknown machineStatus = iota
	statusCommissioning
	statusCommissioned
	statusDeployed
	statusVerified
)

// InMemoryProvider is a test-only MachineProvider. Production uses
// worker.Cache.
type InMemoryProvider struct {
	mu              sync.RWMutex
	macToID         map[string]uuid.UUID
	states          map[uuid.UUID]machineStatus
	systemUUIDs     map[uuid.UUID]string
	storageScripts  map[uuid.UUID]*SnapshotStorageScript
	kernelParams    map[uuid.UUID]string
	networkScripts  map[uuid.UUID]*SnapshotNetworkScript
	networkFacts    map[uuid.UUID][]SnapshotInterface
	fqdn            map[uuid.UUID]string
	amt             map[uuid.UUID]bool
	bmcUser         map[uuid.UUID]string
	deployOps       map[uuid.UUID]*Operation
	commissionOps   map[uuid.UUID]*Operation
	SSHKeys         []string
	ProgressReports []ProgressReport
}

func NewInMemoryProvider() *InMemoryProvider {
	return &InMemoryProvider{
		macToID:        make(map[string]uuid.UUID),
		states:         make(map[uuid.UUID]machineStatus),
		systemUUIDs:    make(map[uuid.UUID]string),
		storageScripts: make(map[uuid.UUID]*SnapshotStorageScript),
		kernelParams:   make(map[uuid.UUID]string),
		networkScripts: make(map[uuid.UUID]*SnapshotNetworkScript),
		networkFacts:   make(map[uuid.UUID][]SnapshotInterface),
		fqdn:           make(map[uuid.UUID]string),
		deployOps:      make(map[uuid.UUID]*Operation),
		commissionOps:  make(map[uuid.UUID]*Operation),
	}
}

func (p *InMemoryProvider) BootActionForMAC(mac string) BootAction {
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.macToID[mac]
	if !ok {
		return BootLocal
	}
	state := p.states[id]
	if state >= statusDeployed {
		return BootLocal
	}
	if state == statusCommissioning {
		return BootCommission
	}
	return BootDeploy
}

func (p *InMemoryProvider) MachineIDForMAC(mac string) (uuid.UUID, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.macToID[mac]
	return id, ok
}

// MachineByMAC assembles a MachineSnapshot for the MAC from the
// per-field test maps, or nil for an unknown MAC. Mirrors
// worker.Cache.MachineByMAC's field-access contract so the httpboot
// handler reads SystemUUID / StorageScript / KernelParams /
// NetworkScript / Interfaces / FQDN off the returned snapshot.
func (p *InMemoryProvider) MachineByMAC(mac string) *MachineSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.macToID[mac]
	if !ok {
		return nil
	}
	return p.machineSnapshotLocked(id)
}

// MachineByID assembles a MachineSnapshot by ID, or nil when the ID is
// unknown. An ID is "known" once it has been Register'd (present in
// states); a bare ID with no seeded fields yields a zero-valued snapshot.
func (p *InMemoryProvider) MachineByID(id uuid.UUID) *MachineSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if _, ok := p.states[id]; !ok {
		return nil
	}
	return p.machineSnapshotLocked(id)
}

// machineSnapshotLocked builds a snapshot from the per-field maps.
// Caller holds p.mu.
func (p *InMemoryProvider) machineSnapshotLocked(id uuid.UUID) *MachineSnapshot {
	m := &MachineSnapshot{
		ID:           id,
		SystemUUID:   p.systemUUIDs[id],
		KernelParams: p.kernelParams[id],
		FQDN:         p.fqdn[id],
		AMT:          p.amt[id],
		BMCUser:      p.bmcUser[id],
	}
	if s := p.storageScripts[id]; s != nil {
		cp := *s
		m.StorageScript = &cp
	}
	if s := p.networkScripts[id]; s != nil {
		cp := *s
		m.NetworkScript = &cp
	}
	if facts := p.networkFacts[id]; facts != nil {
		m.Interfaces = append([]SnapshotInterface(nil), facts...)
	}
	return m
}

// SetFQDN seeds a per-machine FQDN for tests.
func (p *InMemoryProvider) SetFQDN(machineID uuid.UUID, fqdn string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fqdn == nil {
		p.fqdn = make(map[uuid.UUID]string)
	}
	p.fqdn[machineID] = fqdn
}

// SetAMT marks a machine's OOB endpoint as Intel AMT for tests.
func (p *InMemoryProvider) SetAMT(machineID uuid.UUID, amt bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.amt == nil {
		p.amt = make(map[uuid.UUID]bool)
	}
	p.amt[machineID] = amt
}

// SetBMCUser seeds the snapshot's admin-entry BMC username for tests.
func (p *InMemoryProvider) SetBMCUser(machineID uuid.UUID, user string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bmcUser == nil {
		p.bmcUser = make(map[uuid.UUID]string)
	}
	p.bmcUser[machineID] = user
}

// SetNetworkScript seeds a per-machine network-config script for tests.
func (p *InMemoryProvider) SetNetworkScript(machineID uuid.UUID, s *SnapshotNetworkScript) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.networkScripts == nil {
		p.networkScripts = make(map[uuid.UUID]*SnapshotNetworkScript)
	}
	p.networkScripts[machineID] = s
}

// SetNetworkFacts seeds a per-machine set of NetBox interface facts for tests.
func (p *InMemoryProvider) SetNetworkFacts(machineID uuid.UUID, ifaces []SnapshotInterface) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.networkFacts == nil {
		p.networkFacts = make(map[uuid.UUID][]SnapshotInterface)
	}
	p.networkFacts[machineID] = ifaces
}

// SetKernelParams seeds a per-machine kernel cmdline string for tests.
func (p *InMemoryProvider) SetKernelParams(machineID uuid.UUID, params string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kernelParams[machineID] = params
}

// Register adds a MAC->ID mapping so the provider recognizes the MAC.
func (p *InMemoryProvider) Register(mac string, id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.macToID[mac] = id
	if _, ok := p.states[id]; !ok {
		p.states[id] = statusUnknown
	}
}

// SetCommissioning marks a machine as commissioning.
func (p *InMemoryProvider) SetCommissioning(id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states[id] = statusCommissioning
	if p.commissionOps[id] == nil {
		p.commissionOps[id] = &Operation{
			ID:        uuid.NewSHA1(uuid.NameSpaceOID, append(id[:], []byte("commission")...)),
			MachineID: id,
			TraceID:   uuid.NewSHA1(uuid.NameSpaceOID, append(id[:], []byte("commission-trace")...)),
			Type:      OpCommission,
			Status:    OpRunning,
		}
	}
}

// SetSystemUUID sets the system UUID for a machine (test helper).
func (p *InMemoryProvider) SetSystemUUID(id uuid.UUID, uuid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.systemUUIDs[id] = uuid
}

// SetStorageScript attaches a pre-chosen storage script to a machine
// (test helper).
func (p *InMemoryProvider) SetStorageScript(id uuid.UUID, s *SnapshotStorageScript) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.storageScripts[id] = s
}

func (p *InMemoryProvider) RecordDeployComplete(machineID uuid.UUID, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states[machineID] = statusDeployed
	// Test provider doesn't need to retain the BMCCreds payload —
	// HandleProgressReport stashes the full report on
	// p.ProgressReports for tests that want to inspect creds.
}

func (p *InMemoryProvider) RecordPhoneHome(machineID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states[machineID] = statusVerified
}

func (p *InMemoryProvider) RecordCommissionReport(machineID uuid.UUID, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states[machineID] = statusCommissioned
}

func (p *InMemoryProvider) RecordInventoryReport(machineID uuid.UUID, data []byte) {
	// Inventory reports are observation-only from the worker's view;
	// the test provider doesn't need to retain the body.
}

// SetCommissioned marks a machine as commissioned (test helper).
func (p *InMemoryProvider) SetCommissioned(id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.states[id] = statusCommissioned
}

func (p *InMemoryProvider) GetSSHKeys() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, len(p.SSHKeys))
	copy(out, p.SSHKeys)
	return out
}

func (p *InMemoryProvider) DeployOpForMAC(mac string) *Operation {
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.macToID[mac]
	if !ok {
		return nil
	}
	if op, ok := p.deployOps[id]; ok {
		cp := *op
		return &cp
	}
	return nil
}

// SetDeployOp sets the active deploy operation for a machine (test helper).
func (p *InMemoryProvider) SetDeployOp(machineID uuid.UUID, op *Operation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if op == nil {
		delete(p.deployOps, machineID)
		return
	}
	p.deployOps[machineID] = op
}

func (p *InMemoryProvider) CommissionOpForMAC(mac string) *Operation {
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.macToID[mac]
	if !ok {
		return nil
	}
	if op, ok := p.commissionOps[id]; ok {
		cp := *op
		return &cp
	}
	return nil
}

// SetCommissionOp sets the active commission operation for a machine (test helper).
func (p *InMemoryProvider) SetCommissionOp(machineID uuid.UUID, op *Operation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if op == nil {
		delete(p.commissionOps, machineID)
		return
	}
	p.commissionOps[machineID] = op
}

func (p *InMemoryProvider) HandleProgressReport(report *ProgressReport) error {
	mid, err := uuid.Parse(report.MachineID)
	if err != nil {
		return err
	}
	oid, err := uuid.Parse(report.OperationID)
	if err != nil {
		return err
	}
	p.mu.RLock()
	var bound *Operation
	for _, op := range p.deployOps {
		if op != nil && op.ID == oid {
			bound = op
			break
		}
	}
	if bound == nil {
		for _, op := range p.commissionOps {
			if op != nil && op.ID == oid {
				bound = op
				break
			}
		}
	}
	if bound == nil || bound.MachineID != mid || bound.Status == OpCompleted || bound.Status == OpFailed {
		p.mu.RUnlock()
		return fmt.Errorf("report is not bound to an active operation")
	}
	if report.Event == "commission_report" && bound.Type != OpCommission {
		p.mu.RUnlock()
		return fmt.Errorf("commission_report requires commission operation")
	}
	if report.Event == "deploy_complete" && bound.Type != OpDeploy {
		p.mu.RUnlock()
		return fmt.Errorf("deploy_complete requires deploy operation")
	}
	p.mu.RUnlock()

	p.mu.Lock()
	p.ProgressReports = append(p.ProgressReports, *report)
	p.mu.Unlock()

	switch report.Event {
	case "deploy_complete":
		p.RecordDeployComplete(mid, nil)
	case "commission_report":
		p.RecordCommissionReport(mid, report.CommissionData)
	case "phone_home":
		p.RecordPhoneHome(mid)
	}
	return nil
}

// IsCommissioned returns true if the machine has completed commissioning.
func (p *InMemoryProvider) IsCommissioned(id uuid.UUID) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.states[id] >= statusCommissioned
}

// IsDeployed returns true if the machine has completed deployment.
func (p *InMemoryProvider) IsDeployed(id uuid.UUID) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.states[id] >= statusDeployed
}

// IsVerified returns true if the machine has phoned home post-boot.
func (p *InMemoryProvider) IsVerified(id uuid.UUID) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.states[id] >= statusVerified
}
