package storage

// Layout is the top-level JSON structure produced by storage scripts.
// The format matches the MAAS custom storage layout schema.
type Layout struct {
	Devices map[string]Device `json:"layout"`
	Mounts  map[string]Mount  `json:"mounts"`
}

// Device is a physical disk to write, a disk to retain without writes, a RAID
// array to create, or a RAID array to preserve (assemble, not create).
type Device struct {
	Type       string      `json:"type"`                 // "disk", "retain", "raid", or "preserve"
	PTable     string      `json:"ptable,omitempty"`     // "gpt" (disks only)
	Boot       bool        `json:"boot,omitempty"`       // disk gets ESP (disks only)
	Partitions []Partition `json:"partitions,omitempty"` // disks only
	Level      int         `json:"level,omitempty"`      // RAID level (raid only; documentation-only for preserve)
	Members    []string    `json:"members,omitempty"`    // partition/disk names (raid, preserve)
	FS         string      `json:"fs,omitempty"`         // filesystem for the device itself
	// ArrayUUID pins the md array UUID. For "raid" it is optional: when
	// set, planRAID passes --uuid to mdadm --create so the created array
	// has a script-known UUID instead of a random one. For "preserve" it
	// is REQUIRED: it is the identity boundary the agent assembles by and
	// verifies against — member names are a discovery hint, not identity.
	ArrayUUID string `json:"array_uuid,omitempty"`
	// FSUUID is an optional expected filesystem UUID for a preserve
	// device; if set, the agent verifies it (via blkid) before mount.
	FSUUID string `json:"fs_uuid,omitempty"`
}

// Partition describes a GPT partition on a disk.
type Partition struct {
	Name     string `json:"name"`
	Size     string `json:"size"`               // "512M", "100G", "rest"
	FS       string `json:"fs,omitempty"`       // "vfat", "ext4", "xfs", "swap"
	Bootable bool   `json:"bootable,omitempty"` // true for ESP partitions (GPT type EF00)
	// BIOSGrub marks a GPT BIOS boot partition (type EF02): a tiny
	// (~1M), unformatted, unmounted partition that holds GRUB's core.img
	// for legacy/BIOS boot on a GPT disk. The agent's legacy bootloader
	// path (cmd/slagworks-agent installGRUBBIOS) installs GRUB onto every
	// disk that carries one. Mutually exclusive with FS/Bootable.
	BIOSGrub bool `json:"bios_grub,omitempty"`
}

// Mount maps a mount point to a device/partition name from the layout.
type Mount struct {
	Device string `json:"device"`
}
