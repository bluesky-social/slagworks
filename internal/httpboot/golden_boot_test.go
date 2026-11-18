package httpboot

import (
	"encoding/json"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
)

var updateBootGolden = flag.Bool("update-boot-golden", false, "rewrite testdata/boot/*.golden")

// TestBootScriptGolden is the boot-handler contract test (fable-review
// Tier 5): the exact iPXE script emitted per (boot action, mode,
// firmware platform, console config) tuple, pinned as golden files.
// The embed.ipxe regression class — a handler change that silently
// alters what a fleet of machines boots — shows up here as a one-line
// golden diff in 50ms instead of a real-hardware chase.
//
// Golden files live in testdata/boot/. Regenerate after a DELIBERATE
// contract change with:
//
//	go test ./internal/httpboot/ -run TestBootScriptGolden -update-boot-golden
//
// and review the diff like any other wire-format change.
func TestBootScriptGolden(t *testing.T) {
	// Deterministic IDs so the emitted scripts are byte-stable.
	machineID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	opID := uuid.MustParse("66666666-7777-8888-9999-aaaaaaaaaaaa")
	traceID := uuid.MustParse("bbbbbbbb-cccc-dddd-eeee-ffffffffffff")
	const sysUUID = "12345678-1234-1234-1234-000000000042"

	type fixture struct {
		name  string
		setup func() (*model.InMemoryProvider, HandlerConfig)
		query string // extra query params after mac=
	}

	newProvider := func(configure func(p *model.InMemoryProvider, id uuid.UUID)) *model.InMemoryProvider {
		p := model.NewInMemoryProvider()
		p.Register(testMAC, machineID)
		if configure != nil {
			configure(p, machineID)
		}
		return p
	}
	deployCfg := HandlerConfig{DefaultImage: "noble-pxe", HTTPPort: 8080, DeployMode: true}

	fixtures := []fixture{
		{
			// Unknown MAC, UEFI (absent platform): sanboot local boot.
			name: "boot_local_uefi",
			setup: func() (*model.InMemoryProvider, HandlerConfig) {
				return model.NewInMemoryProvider(), HandlerConfig{}
			},
		},
		{
			// Unknown MAC, legacy BIOS: exit-to-firmware local boot.
			name: "boot_local_bios",
			setup: func() (*model.InMemoryProvider, HandlerConfig) {
				return model.NewInMemoryProvider(), HandlerConfig{}
			},
			query: "&platform=pcbios",
		},
		{
			// Known MAC, no image config: chainload-complete.
			name: "chainload_complete",
			setup: func() (*model.InMemoryProvider, HandlerConfig) {
				return newProvider(func(p *model.InMemoryProvider, id uuid.UUID) {
					p.SetKernelParams(id, "console=ttyS1,115200n8")
				}), HandlerConfig{}
			},
		},
		{
			// Commission op, configured console: commission boot script.
			name: "commission_configured",
			setup: func() (*model.InMemoryProvider, HandlerConfig) {
				return newProvider(func(p *model.InMemoryProvider, id uuid.UUID) {
					p.SetCommissioning(id)
					p.SetCommissionOp(id, &model.Operation{
						ID: opID, MachineID: id, TraceID: traceID,
						Type: model.OpCommission, Status: model.OpClaimed,
					})
					p.SetKernelParams(id, "console=ttyS1,115200n8")
				}), deployCfg
			},
		},
		{
			// Commission, UNSET console: broadcast determination boot.
			name: "commission_broadcast_determination",
			setup: func() (*model.InMemoryProvider, HandlerConfig) {
				return newProvider(func(p *model.InMemoryProvider, id uuid.UUID) {
					p.SetCommissioning(id)
					p.SetCommissionOp(id, &model.Operation{
						ID: opID, MachineID: id, TraceID: traceID,
						Type: model.OpCommission, Status: model.OpClaimed,
					})
				}), deployCfg
			},
		},
		{
			// Deploy with op + params + commissioned identity.
			name: "deploy_full",
			setup: func() (*model.InMemoryProvider, HandlerConfig) {
				return newProvider(func(p *model.InMemoryProvider, id uuid.UUID) {
					p.SetKernelParams(id, "console=ttyS1,115200n8")
					p.SetSystemUUID(id, sysUUID)
					p.SetDeployOp(id, &model.Operation{
						ID: opID, MachineID: id, TraceID: traceID,
						Type: model.OpDeploy, Status: model.OpClaimed,
						Params: json.RawMessage(`{"destructive":true}`),
					})
				}), deployCfg
			},
		},
		{
			// Deploy, console "none": no console= on the cmdline.
			name: "deploy_console_none",
			setup: func() (*model.InMemoryProvider, HandlerConfig) {
				return newProvider(func(p *model.InMemoryProvider, id uuid.UUID) {
					p.SetKernelParams(id, "none")
					p.SetSystemUUID(id, sysUUID)
					p.SetDeployOp(id, &model.Operation{
						ID: opID, MachineID: id, TraceID: traceID,
						Type: model.OpDeploy, Status: model.OpClaimed,
					})
				}), deployCfg
			},
		},
		{
			// Deploy, UNSET console: loud refusal script.
			name: "deploy_unset_console_refused",
			setup: func() (*model.InMemoryProvider, HandlerConfig) {
				return newProvider(func(p *model.InMemoryProvider, id uuid.UUID) {
					p.SetSystemUUID(id, sysUUID)
					p.SetDeployOp(id, &model.Operation{
						ID: opID, MachineID: id, TraceID: traceID,
						Type: model.OpDeploy, Status: model.OpClaimed,
					})
				}), deployCfg
			},
		},
		{
			// Deploy with an op but NO commission identity: refused.
			name: "deploy_uncommissioned_refused",
			setup: func() (*model.InMemoryProvider, HandlerConfig) {
				return newProvider(func(p *model.InMemoryProvider, id uuid.UUID) {
					p.SetKernelParams(id, "console=ttyS1,115200n8")
					p.SetDeployOp(id, &model.Operation{
						ID: opID, MachineID: id, TraceID: traceID,
						Type: model.OpDeploy, Status: model.OpClaimed,
					})
				}), deployCfg
			},
		},
	}

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			p, cfg := f.setup()
			h := NewHandler(cfg, p)
			req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC+f.query, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			got := w.Body.String()
			path := filepath.Join("testdata", "boot", f.name+".golden")
			if *updateBootGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read golden %s (run with -update-boot-golden to create): %v", path, err)
			}
			if got != string(want) {
				t.Errorf("boot script drifted from golden %s.\n--- got ---\n%s\n--- want ---\n%s",
					f.name, got, want)
			}
		})
	}
}
