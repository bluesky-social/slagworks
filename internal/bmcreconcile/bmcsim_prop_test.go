package bmcreconcile

// A stateful IPMI user-table simulator used as a fake `ipmitool` by the
// property tests in reconcile_prop_test.go and admingate_prop_test.go.
//
// The simulator lives in the test process. The `ipmitool` Reconcile executes
// is a few lines of bash that forward argv and IPMI_PASSWORD to it over a
// loopback socket and relay its output and exit code, so one invocation costs
// a bash start rather than an interpreter start or a state-file round trip.
// The model is deliberately independent of reconcile.go: it renders `user
// list 1` exactly as ipmitool 1.8.19 prints it (lib/ipmi_user.c
// dump_user_access), and implements the Set User Password / test password
// completion codes as ipmitool reports them.

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type simSlot struct {
	Name     string
	Priv     int // 0 = NO ACCESS, 1..4
	Enabled  bool
	Password string
	PwSize   int // 16 or 20; 0 = never set
	History  []string
	LAN      bool
}

// Fault kinds applied to the call with a given index (0-based, counting
// every ipmitool invocation of one Reconcile run).
const (
	faultBefore = "before" // command not applied, exit 1, session error text
	faultAfter  = "after"  // command applied, then exit 1 (response lost)
	faultDown   = "down"   // this and every later call fail before applying (BMC unreachable)
)

type simState struct {
	Slots []simSlot // index i is slot i+1

	AdminPassword string // checked for lanplus sessions

	// Firmware quirks.
	MegaRAC     bool // refuse set password to the current or a historical password (0x19)
	NoEmptyName bool // Supermicro: refuse `user set name <slot> ""`
	Tabs        bool // real ipmitool header bytes (tabs) vs terminal-expanded
	// DisableShowsNoAccess: `user list` shows NO ACCESS for a disabled
	// user. Real ipmitool prints the channel privilege limit only, so false
	// is the faithful default; true models firmware that zeroes the limit.
	DisableShowsNoAccess bool

	Faults map[int]string
	Calls  int
	Log    []string
	Down   bool
}

func (s *simState) clone() *simState {
	c := *s
	c.Slots = make([]simSlot, len(s.Slots))
	for i, sl := range s.Slots {
		sl.History = slices.Clone(sl.History)
		c.Slots[i] = sl
	}
	c.Log = slices.Clone(s.Log)
	c.Faults = map[int]string{}
	for k, v := range s.Faults {
		c.Faults[k] = v
	}
	return &c
}

// bmcSim serves one simulated BMC to the fake ipmitool at bin.
type bmcSim struct {
	bin string

	mu    sync.Mutex
	state *simState
}

const simIPMIToolScript = `#!/usr/bin/env bash
exec 3<>/dev/tcp/127.0.0.1/%d || exit 98
printf '%%s\n' "${IPMI_PASSWORD-}" "$#" "$@" >&3
IFS= read -r code <&3 || exit 97
while IFS= read -r line <&3; do printf '%%s\n' "$line"; done
exit "$code"
`

// newBMCSim starts the simulator and writes the fake ipmitool. Arguments and
// output travel line by line: an argument must not contain a newline, and
// every line of output ends in one.
func newBMCSim(t *testing.T) *bmcSim {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	b := &bmcSim{bin: filepath.Join(t.TempDir(), "ipmitool"), state: &simState{}}
	script := fmt.Sprintf(simIPMIToolScript, ln.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(b.bin, []byte(script), 0o755); err != nil { //nolint:gosec // an executable test fake
		t.Fatalf("write fake ipmitool: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(c)
		}
	}()
	return b
}

func (b *bmcSim) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	line := func() (string, bool) {
		s, err := r.ReadString('\n')
		return strings.TrimSuffix(s, "\n"), err == nil
	}
	password, ok := line()
	if !ok {
		return
	}
	countLine, ok := line()
	if !ok {
		return
	}
	n, err := strconv.Atoi(countLine)
	if err != nil {
		return
	}
	argv := make([]string, 0, n)
	for range n {
		a, ok := line()
		if !ok {
			return
		}
		argv = append(argv, a)
	}
	b.mu.Lock()
	out, code := b.state.invoke(argv, password)
	b.mu.Unlock()
	fmt.Fprintf(c, "%d\n%s", code, out)
}

// snapshot returns a copy of the simulated BMC.
func (b *bmcSim) snapshot() *simState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.clone()
}

// set replaces the simulated BMC.
func (b *bmcSim) set(s *simState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = s.clone()
}

var simPrivNames = map[int]string{1: "CALLBACK", 2: "USER", 3: "OPERATOR", 4: "ADMINISTRATOR"}

func (s *simState) userList() string {
	var b strings.Builder
	if s.Tabs {
		b.WriteString("ID  Name\t     Callin  Link Auth\tIPMI Msg   Channel Priv Limit\n")
	} else {
		b.WriteString("ID  Name             Callin  Link Auth  IPMI Msg   Channel Priv Limit\n")
	}
	for i, sl := range s.Slots {
		priv := "NO ACCESS"
		if p, ok := simPrivNames[sl.Priv]; ok && (sl.Enabled || !s.DisableShowsNoAccess) {
			priv = p
		}
		link := "false"
		if sl.LAN {
			link = "true "
		}
		fmt.Fprintf(&b, "%-4d%-17s%-8s%-11s%-11s%-s\n", i+1, sl.Name, "true ", link, link, priv)
	}
	return b.String()
}

// invoke runs one fake ipmitool invocation and returns its output and exit
// code.
func (s *simState) invoke(argv []string, sessionPassword string) (string, int) {
	idx := s.Calls
	s.Calls++
	// Strip the transport prefix: -I <iface> [-H h -U u -C c -E].
	args := argv
	iface := ""
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch {
		case args[0] == "-E":
			args = args[1:]
		case len(args) < 2:
			args = nil
		case args[0] == "-I":
			iface = args[1]
			args = args[2:]
		default:
			args = args[2:]
		}
	}
	s.Log = append(s.Log, strings.Join(args, " "))
	fault := s.Faults[idx]
	if fault == faultDown {
		s.Down = true
	}
	const sessionError = "Error: Unable to establish IPMI v2 / RMCP+ session\n"
	switch {
	case s.Down:
		return sessionError, 1
	case iface == "lanplus" && sessionPassword != s.AdminPassword:
		return sessionError, 1
	case fault == faultBefore:
		return sessionError, 1
	}
	out, code := s.exec(args)
	if fault == faultAfter {
		return "Unknown error\n", 1
	}
	return out, code
}

func (s *simState) slot(arg string) (*simSlot, bool) {
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > len(s.Slots) {
		return nil, false
	}
	return &s.Slots[n-1], true
}

func (s *simState) exec(a []string) (string, int) {
	j := strings.Join(a, " ")
	switch {
	case j == "user list 1":
		return s.userList(), 0
	case len(a) >= 4 && a[0] == "user" && a[1] == "set" && a[2] == "name":
		sl, ok := s.slot(a[3])
		if !ok {
			return "Invalid user id\n", 1
		}
		name := ""
		if len(a) >= 5 {
			name = a[4]
		}
		if name == "" && s.NoEmptyName {
			return "Set User Name command failed (user " + a[3] + "): Invalid data field in request\n", 1
		}
		if len(name) > 16 {
			return "Username is too long (> 16 bytes)\n", 1
		}
		sl.Name = name
		return "", 0
	case len(a) >= 5 && a[0] == "user" && a[1] == "set" && a[2] == "password":
		sl, ok := s.slot(a[3])
		if !ok {
			return "Invalid user id\n", 1
		}
		pw := a[4]
		size := 16
		if len(a) >= 6 && a[5] == "20" {
			size = 20
		}
		if len(pw) > size {
			return fmt.Sprintf("Password is too long (> %d bytes)\n", size), 1
		}
		if s.MegaRAC && (pw == sl.Password || slices.Contains(sl.History, pw)) {
			return "Set User Password command failed (user " + a[3] + "): Unknown (0x19)\n", 1
		}
		if sl.Password != "" {
			sl.History = append(sl.History, sl.Password)
			if len(sl.History) > 5 {
				sl.History = sl.History[len(sl.History)-5:]
			}
		}
		sl.Password, sl.PwSize = pw, size
		return "Set User Password command successful (user " + a[3] + ")\n", 0
	case len(a) >= 5 && a[0] == "user" && a[1] == "test":
		sl, ok := s.slot(a[2])
		if !ok {
			return "Invalid user id\n", 1
		}
		size, _ := strconv.Atoi(a[3])
		pw := a[4]
		if len(pw) > size {
			pw = pw[:size]
		}
		stored := sl.PwSize
		if stored == 0 {
			stored = 16 // a never-set slot holds the 16-byte null password
		}
		if stored != size {
			return "Failure: wrong password size\n", 1
		}
		if sl.Password == pw {
			return "Success\n", 0
		}
		return "Failure: password incorrect\n", 1
	case len(a) >= 4 && a[0] == "user" && a[1] == "priv":
		sl, ok := s.slot(a[2])
		if !ok {
			return "Invalid user id\n", 1
		}
		p, _ := strconv.Atoi(a[3])
		sl.Priv = p
		return "Set Privilege Level command successful (user " + a[2] + ")\n", 0
	case len(a) >= 4 && a[0] == "channel" && a[1] == "setaccess" && a[2] == "1":
		sl, ok := s.slot(a[3])
		if !ok {
			return "Invalid user id\n", 1
		}
		for _, kv := range a[4:] {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "link", "ipmi":
				sl.LAN = v == "on"
			case "privilege":
				p, _ := strconv.Atoi(v)
				sl.Priv = p
			}
		}
		return "Set User Access (channel 1 id " + a[3] + ") successful.\n", 0
	case len(a) == 3 && a[0] == "user" && (a[1] == "enable" || a[1] == "disable"):
		sl, ok := s.slot(a[2])
		if !ok {
			return "Invalid user id\n", 1
		}
		sl.Enabled = a[1] == "enable"
		return "", 0
	case strings.HasPrefix(j, "chassis bootparam set bootflag force_pxe"):
		return "Set Boot Device to force_pxe\n", 0
	default:
		return "sim: unsupported command: " + j + "\n", 1
	}
}
