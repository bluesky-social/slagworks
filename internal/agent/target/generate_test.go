package target

import (
	"strings"
	"testing"
)

func TestGenerateFstab(t *testing.T) {
	// Multi-mount input: root (ext4, pass 1), ESP (vfat/umask, pass 1),
	// and a data mount (xfs, defaults, pass 2). Order is the caller's
	// responsibility; GenerateFstab renders it verbatim.
	entries := []fstabEntry{
		{Mountpoint: "/", UUID: "aaaa-bbbb-cccc", FSType: "ext4", Opts: "defaults", Passno: 1},
		{Mountpoint: "/boot/efi", UUID: "DDDD-EEEE", FSType: "vfat", Opts: "umask=0077", Passno: 1},
		{Mountpoint: "/data", UUID: "1111-2222", FSType: "xfs", Opts: "defaults", Passno: 2},
	}
	fstab := GenerateFstab(entries)

	if !strings.Contains(fstab, "UUID=aaaa-bbbb-cccc\t/\text4\tdefaults\t0\t1\n") {
		t.Fatalf("missing/wrong root entry, got:\n%s", fstab)
	}
	if !strings.Contains(fstab, "UUID=DDDD-EEEE\t/boot/efi\tvfat\tumask=0077\t0\t1\n") {
		t.Fatalf("missing/wrong ESP entry, got:\n%s", fstab)
	}
	if !strings.Contains(fstab, "UUID=1111-2222\t/data\txfs\tdefaults\t0\t2\n") {
		t.Fatalf("missing/wrong data entry, got:\n%s", fstab)
	}
	if !strings.HasSuffix(fstab, "\n") {
		t.Fatal("fstab should end with newline")
	}

	// Ordering must be preserved: / then /boot/efi then /data.
	rootIdx := strings.Index(fstab, "\t/\t")
	espIdx := strings.Index(fstab, "/boot/efi")
	dataIdx := strings.Index(fstab, "/data")
	if rootIdx >= espIdx || espIdx >= dataIdx {
		t.Fatalf("entries out of order (want / < /boot/efi < /data), got:\n%s", fstab)
	}
}

// TestSortMountPaths pins the fstab ordering: root first (shallowest), then
// by depth, ties broken lexically -- matching planMounts so fstab and the
// mount sequence are trivially diffable.
func TestSortMountPaths(t *testing.T) {
	got := sortMountPaths(map[string]string{
		"/data":     "/dev/md1",
		"/":         "/dev/md0",
		"/boot/efi": "/dev/nvme0n1p1",
		"/var/log":  "/dev/sdb1",
	})
	want := []string{"/", "/data", "/boot/efi", "/var/log"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortMountPaths order = %v, want %v", got, want)
		}
	}
}

// TestFstabOpts pins the per-mountpoint options/passno rules.
func TestFstabOpts(t *testing.T) {
	cases := []struct {
		mp     string
		opts   string
		passno int
	}{
		{"/", "defaults", 1},
		{"/boot/efi", "umask=0077", 1},
		{"/data", "defaults", 2},
		{"/var/lib/whatever", "defaults", 2},
	}
	for _, c := range cases {
		opts, passno := fstabOpts(c.mp)
		if opts != c.opts || passno != c.passno {
			t.Errorf("fstabOpts(%q) = (%q, %d), want (%q, %d)", c.mp, opts, passno, c.opts, c.passno)
		}
	}
}

func TestGeneratePhoneHomeUnit(t *testing.T) {
	unit := GeneratePhoneHomeUnit("10.0.0.1:8080", "aaaa-bbbb-cccc-dddd", "eeee-ffff-0000-1111")

	if !strings.Contains(unit, "[Unit]") {
		t.Fatal("missing [Unit] section")
	}
	if !strings.Contains(unit, "[Service]") {
		t.Fatal("missing [Service] section")
	}
	if !strings.Contains(unit, "[Install]") {
		t.Fatal("missing [Install] section")
	}
	if !strings.Contains(unit, `"machine_id":"aaaa-bbbb-cccc-dddd"`) {
		t.Fatalf("missing machine_id in JSON payload, got:\n%s", unit)
	}
	if !strings.Contains(unit, `"op_id":"eeee-ffff-0000-1111"`) {
		t.Fatalf("missing op_id in JSON payload, got:\n%s", unit)
	}
	if !strings.Contains(unit, `"event":"phone_home"`) {
		t.Fatalf("missing event in JSON payload, got:\n%s", unit)
	}
	if !strings.Contains(unit, "http://10.0.0.1:8080/api/v1/report") {
		t.Fatalf("missing correct URL, got:\n%s", unit)
	}
	if !strings.Contains(unit, "ConditionPathExists=!/var/lib/slagworks/.phoned-home") {
		t.Fatal("missing sentinel condition")
	}
	if !strings.Contains(unit, "After=network-online.target") {
		t.Fatal("missing network dependency")
	}
	if !strings.Contains(unit, "WantedBy=multi-user.target") {
		t.Fatal("missing install target")
	}

	// The unit must RETRY until success and only write the
	// .phoned-home marker on an HTTP 2xx, so a slow-converging LACP bond (or
	// any transient POST failure) can't permanently strand the host in
	// awaiting_phone_home.
	if !strings.Contains(unit, "until wget") {
		t.Fatalf("phone-home must retry until success (missing `until wget` loop):\n%s", unit)
	}
	if !strings.Contains(unit, "sleep 5") {
		t.Fatalf("retry loop must back off between attempts:\n%s", unit)
	}
	// The marker write must come AFTER the until-loop (success-only), not as an
	// unconditional ExecStartPost.
	loopIdx := strings.Index(unit, "until wget")
	markerIdx := strings.Index(unit, "touch /var/lib/slagworks/.phoned-home")
	if markerIdx < 0 || loopIdx < 0 || markerIdx < loopIdx {
		t.Fatalf("marker must be written only after the success loop:\n%s", unit)
	}
	if strings.Contains(unit, "ExecStartPost") {
		t.Fatalf("must not unconditionally touch the marker via ExecStartPost (the bug):\n%s", unit)
	}
	// Each attempt must be time-bounded so a black-holed route can't wedge the
	// loop on a single try.
	if !strings.Contains(unit, "-T 10") {
		t.Fatalf("each wget attempt must have a timeout (-T):\n%s", unit)
	}
	// The oneshot unit MUST disable TimeoutStartSec, or systemd's default 90s
	// would SIGTERM the retry loop and (with no Restart=) strand the host until
	// reboot — the exact window this fix closes.
	if !strings.Contains(unit, "TimeoutStartSec=infinity") {
		t.Fatalf("oneshot retry loop must disable the start timeout (TimeoutStartSec=infinity):\n%s", unit)
	}
}

// TestGeneratePhoneHomeUnit_ShellQuotingSafe asserts the ExecStart sh -c
// payload is single-quoted as one token and the inner JSON's single-quote-free
// payload survives — a regression guard on shellSingleQuote.
func TestGeneratePhoneHomeUnit_ShellQuotingSafe(t *testing.T) {
	unit := GeneratePhoneHomeUnit("10.0.0.1:8080", "aaaa-bbbb-cccc-dddd", "eeee-ffff-0000-1111")
	if !strings.Contains(unit, "ExecStart=/bin/sh -c '") {
		t.Fatalf("ExecStart must invoke sh -c with a single-quoted script:\n%s", unit)
	}
	// The JSON payload uses double quotes, so the single-quoted wrapper stays
	// balanced — exactly one opening and matching close around the script.
	if !strings.Contains(unit, `"machine_id":"aaaa-bbbb-cccc-dddd"`) {
		t.Fatalf("JSON payload mangled by quoting:\n%s", unit)
	}
}

func TestGeneratePhoneHomeUnit_SpecialCharsInServer(t *testing.T) {
	unit := GeneratePhoneHomeUnit("192.168.1.100:9090", "aaaa-bbbb-cccc-dddd", "eeee-ffff-0000-1111")
	if !strings.Contains(unit, "http://192.168.1.100:9090/api/v1/report") {
		t.Fatalf("URL not correctly formed, got:\n%s", unit)
	}
}
