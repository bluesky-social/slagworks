//go:build integration

package qemutest

import (
	"bytes"
	"fmt"
	"strings"
)

// WriteCpioNewc writes a single regular-file entry plus the
// terminating TRAILER!!! marker into a newc-format cpio archive in
// memory. Linux initramfs concatenates multiple cpio archives by
// design (see Documentation/filesystems/ramfs-rootfs-initramfs.rst);
// later entries shadow earlier ones. The integration tests use this
// to overlay an ephemeral test pubkey onto the canonical initrd
// without rebuilding it.
//
// Layout per newc spec: 110-byte ASCII header, name (NUL-terminated),
// 4-byte alignment pad, file data, 4-byte alignment pad. Each header
// field is exactly 8 hex characters representing a uint32.
func WriteCpioNewc(path string, mode uint32, data []byte) []byte {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// cpio paths are stored without the leading slash.
	name := strings.TrimPrefix(path, "/")
	return writeCpioEntries([]cpioEntry{{
		name: name,
		mode: 0o100000 | (mode & 0o7777), // S_IFREG | mode
		data: data,
	}})
}

type cpioEntry struct {
	name string
	mode uint32
	data []byte
}

func writeCpioEntries(entries []cpioEntry) []byte {
	var buf bytes.Buffer
	for _, e := range entries {
		// Name is NUL-terminated in the archive.
		nameBytes := append([]byte(e.name), 0)
		writeNewcHeader(&buf, e.mode, uint32(len(e.data)), uint32(len(nameBytes)))
		buf.Write(nameBytes)
		padTo(&buf, 4)
		buf.Write(e.data)
		padTo(&buf, 4)
	}
	// Trailer.
	trailer := append([]byte("TRAILER!!!"), 0)
	writeNewcHeader(&buf, 0, 0, uint32(len(trailer)))
	buf.Write(trailer)
	padTo(&buf, 4)
	return buf.Bytes()
}

func writeNewcHeader(buf *bytes.Buffer, mode, fileSize, nameLen uint32) {
	// All 13 fields are 8-hex-char uint32. ino, uid, gid, nlink,
	// mtime, devmajor, devminor, rdevmajor, rdevminor, check are all
	// zero in our minimal archives; the kernel doesn't care for
	// initramfs purposes. nlink=1 is required for regular files
	// (kernel rejects nlink=0 on reg files).
	// nlink=1 for everything incl. the trailer, per newc spec.
	nlink := uint32(1)
	fmt.Fprintf(buf, "070701") // newc magic
	for _, v := range []uint32{
		0,        // ino
		mode,     // mode
		0,        // uid
		0,        // gid
		nlink,    // nlink
		0,        // mtime
		fileSize, // filesize
		0,        // devmajor
		0,        // devminor
		0,        // rdevmajor
		0,        // rdevminor
		nameLen,  // namesize (includes trailing NUL)
		0,        // check
	} {
		fmt.Fprintf(buf, "%08x", v)
	}
}

func padTo(buf *bytes.Buffer, alignment int) {
	if pad := (alignment - buf.Len()%alignment) % alignment; pad > 0 {
		buf.Write(make([]byte, pad))
	}
}
