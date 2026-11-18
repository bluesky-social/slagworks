package storage

// DefaultLayout returns the built-in fallback layout for a single disk:
// 512MB ESP + remaining ext4 root. Used when no storage script matches.
func DefaultLayout(diskName string) Layout {
	p1 := partName(diskName, 1)
	p2 := partName(diskName, 2)
	return Layout{
		Devices: map[string]Device{
			diskName: {
				Type:   "disk",
				PTable: "gpt",
				Boot:   true,
				Partitions: []Partition{
					{Name: p1, FS: "vfat", Size: "512M", Bootable: true},
					{Name: p2, FS: "ext4", Size: "rest"},
				},
			},
		},
		Mounts: map[string]Mount{
			"/":         {Device: p2},
			"/boot/efi": {Device: p1},
		},
	}
}
