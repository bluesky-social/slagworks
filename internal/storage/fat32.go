package storage

import "fmt"

// fat32MinClusters is the fewest clusters a FAT32 filesystem may have.
// Readers that follow Microsoft's FAT specification, UEFI firmware among
// them, tell FAT12, FAT16 and FAT32 apart by cluster count alone, so to them
// a "FAT32" filesystem with fewer clusters is a corrupt FAT16 one. mkfs.fat
// -F 32 formats such a filesystem anyway, with a warning and exit status 0.
const fat32MinClusters = 65525

const mib = 1 << 20

// fat32MinBytes returns the smallest partition, in whole MiB, that
// `mkfs.fat -F 32` formats with at least fat32MinClusters clusters on a disk
// with the given logical sector size: 33 MiB at 512-byte sectors, 257 MiB at
// 4096-byte (4Kn) sectors. It follows how dosfstools 4.2 lays out FAT32 at
// that size: one sector per cluster, 32 reserved sectors, two FATs holding
// a four-byte entry per cluster plus two reserved entries, and the
// filesystem shortened to a whole number of tracks (up to 62 sectors lost).
// Rounding up to whole MiB, the unit sgdisk aligns partitions to, also
// covers mkfs.fat sizing its FATs from an estimate a few clusters high.
// dosfstools 4.2 formats 32M (512-byte sectors) and 256M (4096-byte sectors)
// with the warning and 33M and 257M without it.
func fat32MinBytes(sectorBytes int64) int64 {
	fatSectors := ceilDiv((fat32MinClusters+2)*4, sectorBytes)
	sectors := 32 + 2*fatSectors + fat32MinClusters + 62
	return ceilDiv(sectors*sectorBytes, mib) * mib
}

// checkVFATSizes refuses a vfat partition on disk name that is too small for
// FAT32 at the disk's logical sector size. planDisk formats every vfat
// partition with mkfs.fat -F 32, which formats an undersized one without
// failing; UEFI firmware cannot read the result, so an undersized ESP
// installs cleanly and then does not boot. Refusing here, in the pure plan,
// stops the deploy before any disk is touched.
func checkVFATSizes(name string, dev Device, bd BlockDevice) error {
	for i, p := range dev.Partitions {
		if p.FS != "vfat" {
			continue
		}
		sector := bd.LogicalSectorBytes
		if sector <= 0 {
			return fmt.Errorf("disk %s: the inventory has no logical sector size for it, "+
				"which checking vfat partition %s against the FAT32 minimum needs; "+
				"the agent reads it from lsblk's LOG-SEC column", name, p.Name)
		}
		var size int64
		var what string
		if p.Size == "rest" {
			var err error
			size, err = restPartitionBytes(dev, i, bd)
			if err != nil {
				return fmt.Errorf("disk %s: sizing vfat partition %s: %w", name, p.Name, err)
			}
			what = fmt.Sprintf("the rest of the disk (about %dM)", size/mib)
		} else {
			var err error
			size, err = partitionBytes(p.Size, sector)
			if err != nil {
				return fmt.Errorf("disk %s partition %s: %w", name, p.Name, err)
			}
			what = p.Size
		}
		if minBytes := fat32MinBytes(sector); size < minBytes {
			return fmt.Errorf("disk %s partition %s: %s is too small for FAT32 on a disk with "+
				"%d-byte logical sectors: mkfs.fat -F 32 would format it with fewer than %d "+
				"clusters, which UEFI firmware cannot read; make the partition at least %dM",
				name, p.Name, what, sector, fat32MinClusters, minBytes/mib)
		}
	}
	return nil
}

// restPartitionBytes returns a lower bound on the size sgdisk gives the
// "rest" partition at index rest: the disk less every other partition, each
// rounded up to the 1 MiB sgdisk aligns partition starts to, less the 1 MiB
// before the first partition and up to 1 MiB for the backup GPT at the end.
func restPartitionBytes(dev Device, rest int, bd BlockDevice) (int64, error) {
	left := bd.SizeBytes - 2*mib
	for i, p := range dev.Partitions {
		if i == rest {
			continue
		}
		b, err := partitionBytes(p.Size, bd.LogicalSectorBytes)
		if err != nil {
			return 0, fmt.Errorf("partition %s: %w", p.Name, err)
		}
		left -= ceilDiv(b, mib) * mib
	}
	return left, nil
}

// partitionBytes returns the size sgdisk gives a partition declared with a
// fixed size (see sizeBytes): the declared bytes rounded down to a whole
// number of logical sectors.
func partitionBytes(size string, sectorBytes int64) (int64, error) {
	b, err := sizeBytes(size)
	if err != nil {
		return 0, err
	}
	return b - b%sectorBytes, nil
}

func ceilDiv(a, b int64) int64 {
	return (a + b - 1) / b
}
