package winfsp

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

// ioReparseTagMountPoint is IO_REPARSE_TAG_MOUNT_POINT, the tag of a junction.
const ioReparseTagMountPoint = 0xA0000003

// mountPointReparseTarget returns the substitute name of a mount-point
// (junction) reparse buffer as returned by FSCTL_GET_REPARSE_POINT.
func mountPointReparseTarget(buf []byte) (string, error) {
	if len(buf) < 16 {
		return "", fmt.Errorf("reparse buffer is %d bytes, shorter than a mount point header", len(buf))
	}
	if tag := binary.LittleEndian.Uint32(buf[:4]); tag != ioReparseTagMountPoint {
		return "", fmt.Errorf("reparse tag %#x is not a mount point", tag)
	}
	off := int(binary.LittleEndian.Uint16(buf[8:10]))
	size := int(binary.LittleEndian.Uint16(buf[10:12]))
	if off%2 != 0 || size%2 != 0 || 16+off+size > len(buf) {
		return "", fmt.Errorf("invalid mount point substitute name offset %d size %d in %d bytes", off, size, len(buf))
	}
	name := make([]uint16, size/2)
	for i := range name {
		name[i] = binary.LittleEndian.Uint16(buf[16+off+2*i:])
	}
	return string(utf16.Decode(name)), nil
}

// mountPointReparseBuffer builds an FSCTL_SET_REPARSE_POINT buffer for a
// junction whose substitute and print names are both target.
func mountPointReparseBuffer(target string) []byte {
	u := append(utf16.Encode([]rune(target)), 0)
	nameBytes := len(u) * 2
	data := make([]byte, 16+2*nameBytes)
	binary.LittleEndian.PutUint32(data[0:4], ioReparseTagMountPoint)
	binary.LittleEndian.PutUint16(data[4:6], uint16(len(data)-8))
	// Substitute name at offset 0, print name right after its terminator.
	binary.LittleEndian.PutUint16(data[8:10], 0)
	binary.LittleEndian.PutUint16(data[10:12], uint16(nameBytes-2))
	binary.LittleEndian.PutUint16(data[12:14], uint16(nameBytes))
	binary.LittleEndian.PutUint16(data[14:16], uint16(nameBytes-2))
	for i, c := range u {
		binary.LittleEndian.PutUint16(data[16+2*i:], c)
		binary.LittleEndian.PutUint16(data[16+nameBytes+2*i:], c)
	}
	return data
}
