package storage

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// partitionSizeUnitBytes is what every fixed partition size must be a whole
// multiple of: 4096, the largest logical sector size. sgdisk rounds a size
// down to whole sectors, so on a 4Kn disk a size under 4096 bytes is a
// zero-length partition and any other size that is not a multiple of it is a
// smaller partition than the same size gives on a disk with 512-byte sectors
// ("6K" is 6 KiB there and 4 KiB on 4Kn), with sgdisk exiting 0.
const partitionSizeUnitBytes = 4096

// sizeBytes returns the bytes a fixed partition size declares: a whole
// number with a K, M, G or T suffix, in binary units and either case ("512M"
// is 512 MiB), that is a positive multiple of partitionSizeUnitBytes. planDisk
// passes the size to sgdisk unchanged, as the end of "-n N:0:+<size>", so
// anything sgdisk would read differently is refused here: a bare number
// (sgdisk reads it as sectors, a size that changes with the disk's sector
// size), a fraction (sgdisk drops it), zero, a negative number or a size
// under one sector (sgdisk fails, after --zap-all has run), and a size that
// is not a whole number of 4096-byte sectors (sgdisk rounds it down on a 4Kn
// disk only). The last rule does not depend on the sector size of the disk
// in hand: a layout must mean the same on every disk it may land on. Only a
// K size can break it; every M, G and T size is a multiple of 4K.
func sizeBytes(size string) (int64, error) {
	bad := fmt.Errorf("partition size %q is neither \"rest\" nor a positive whole number "+
		"with a K, M, G or T suffix (as in \"512M\" or \"100G\")", size)
	n := len(size)
	if n == 0 {
		return 0, bad
	}
	i := strings.Index("KMGT", strings.ToUpper(size[n-1:]))
	if i < 0 {
		return 0, bad
	}
	digits, unit := size[:n-1], int64(1)<<(10*(i+1))
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, bad
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || v == 0 || v > math.MaxInt64/unit {
		return 0, bad
	}
	if v*unit < partitionSizeUnitBytes {
		return 0, fmt.Errorf("partition size %q is under 4K: sgdisk rounds sizes down to whole "+
			"logical sectors, which are 4K on a 4Kn disk; make it at least 1M", size)
	}
	if rem := v * unit % partitionSizeUnitBytes; rem != 0 {
		down := (v*unit - rem) / 1024
		return 0, fmt.Errorf("partition size %q is not a multiple of 4K: sgdisk rounds sizes down to "+
			"whole logical sectors, so on a 4Kn disk (4K sectors) it would create %dK instead; "+
			"use %dK or %dK, or a size in M", size, down, down, down+partitionSizeUnitBytes/1024)
	}
	return v * unit, nil
}

// checkFits refuses a disk whose partitions cannot all be created on it.
// sgdisk starts the first partition 1 MiB into the disk and each later one
// on the next 1 MiB boundary, and keeps the end of the disk for the backup
// GPT, so the partitions need at least 1 MiB, plus each fixed size rounded up
// to 1 MiB, plus the backup GPT (counted as 1 MiB; it is 16.5 KiB at 512-byte
// sectors and 20 KiB at 4096). A "rest" partition, which validateDisk allows
// only last, is counted as 1 MiB, the least it may get. Without this check a
// layout that does not fit fails at its first partition past the end of the
// disk, after sgdisk --zap-all has erased the disk's partition table.
//
// The accounting is the one restPartitionBytes uses and overstates what
// sgdisk needs by under 2 MiB, so a layout this accepts always fits.
func checkFits(name string, dev Device, bd BlockDevice) error {
	diskMiB := bd.SizeBytes / mib
	needMiB := int64(2)
	for _, p := range dev.Partitions {
		partMiB := int64(1)
		if p.Size != "rest" {
			b, err := sizeBytes(p.Size)
			if err != nil {
				return fmt.Errorf("disk %s partition %s: %w", name, p.Name, err)
			}
			// Rounded up without ceilDiv, which overflows for a b within
			// 1 MiB of MaxInt64.
			partMiB = b / mib
			if b%mib != 0 {
				partMiB++
			}
		}
		// Returning at the first partition that does not fit also keeps
		// needMiB from overflowing.
		needMiB += partMiB
		if needMiB > diskMiB {
			return fmt.Errorf("disk %s: partition %s does not fit: with sgdisk's 1M alignment "+
				"and the GPT, the partitions up to and including it need at least %dM, and the "+
				"inventory gives the disk %dM (%d bytes); make the layout's partitions smaller",
				name, p.Name, needMiB, diskMiB, bd.SizeBytes)
		}
	}
	return nil
}
