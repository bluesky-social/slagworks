package bootloader

import "testing"

func TestSplitDiskPart(t *testing.T) {
	cases := []struct {
		in      string
		disk    string
		part    int
		wantErr bool
	}{
		{"/dev/nvme0n1p1", "/dev/nvme0n1", 1, false},
		{"/dev/nvme10n1p2", "/dev/nvme10n1", 2, false},
		{"/dev/sda1", "/dev/sda", 1, false},
		{"/dev/sdb12", "/dev/sdb", 12, false},
		{"/dev/vda3", "/dev/vda", 3, false},
		{"/dev/mmcblk0p1", "/dev/mmcblk0", 1, false},
		{"/dev/nvme0n1", "", 0, true},
		{"/dev/sda", "", 0, true},
		{"", "", 0, true},
		{"/dev/foo", "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			disk, part, err := SplitDiskPart(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("SplitDiskPart(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if disk != c.disk || part != c.part {
				t.Errorf("SplitDiskPart(%q) = (%q, %d), want (%q, %d)",
					c.in, disk, part, c.disk, c.part)
			}
		})
	}
}

func TestParseSlagworksBootNum(t *testing.T) {
	// Real-shape output (from kworker-l34-2, 2026-06-01): BootNNNN* label\tdevpath.
	// Note Boot0008/0009 "UEFI OS" device paths reference \EFI\BOOT\BOOTX64.EFI
	// (same loader file as Slagworks) — the parser must NOT match on the path,
	// only on the label field being exactly "Slagworks".
	const realOut = `BootCurrent: 0003
Timeout: 1 seconds
BootOrder: 0007,0002,0003,0004,0005,0001,0000,0006,0008,0009
Boot0000  Ubuntu	HD(1,GPT,23b77a5f,0x800,0xf4000)/File(\EFI\ubuntu\shimx64.efi)
Boot0002* UEFI PXE IPv4 Mellanox	PciRoot(0x1)/MAC(7cc255be9174,1)
Boot0006  Ubuntu	HD(1,GPT,102da6b5,0x800,0xf4000)/File(\EFI\ubuntu\shimx64.efi)
Boot0008  UEFI OS	HD(1,GPT,200bd215,0x800,0x100000)/File(\EFI\BOOT\BOOTX64.EFI)
Boot0009  UEFI OS	HD(1,GPT,764e175e,0x800,0x100000)/File(\EFI\BOOT\BOOTX64.EFI)
Boot0007* Slagworks	HD(1,GPT,85b0a4b0,0x800,0x100000)/File(\EFI\BOOT\BOOTX64.EFI)
`
	id, err := parseSlagworksBootNum(realOut)
	if err != nil {
		t.Fatalf("real output: unexpected error: %v", err)
	}
	if id != "0007" {
		t.Errorf("real output: got %q, want 0007", id)
	}

	// Zero matches -> error (don't guess a BootNext target).
	if _, err := parseSlagworksBootNum("Boot0002* UEFI PXE IPv4\tPciRoot(0x1)\n"); err == nil {
		t.Error("no Slagworks entry: expected error, got nil")
	}

	// Two matches -> ambiguous error (the drop-then-create should make
	// this impossible, but the parser must refuse rather than pick one).
	const dup = "Boot0007* Slagworks\tHD(a)\nBoot0008* Slagworks\tHD(b)\n"
	if _, err := parseSlagworksBootNum(dup); err == nil {
		t.Error("duplicate Slagworks entries: expected ambiguous error, got nil")
	}

	// A "Slagworks" substring in a device path must NOT match (label guard).
	const pathOnly = "Boot0003* Network\tHD(x)/File(\\Slagworks\\boot.efi)\n"
	if _, err := parseSlagworksBootNum(pathOnly); err == nil {
		t.Error("Slagworks only in device path: should not match the label")
	}
}
