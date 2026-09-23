package winfsp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// mappedImportSlot locates one named kernel32 IAT slot in a mapped PE64 image.
// It is test-only: callers must independently verify the loaded module identity
// and restore the slot after injection. No process-wide API patching is needed.
func mappedImportSlot(image []byte, symbol string) (uint32, error) {
	bad := func() (uint32, error) { return 0, fmt.Errorf("invalid or ambiguous PE64 import %q", symbol) }
	span := func(off, n uint32) bool { return uint64(off)+uint64(n) <= uint64(len(image)) }
	if len(image) < 64 || string(image[:2]) != "MZ" {
		return bad()
	}
	pe := binary.LittleEndian.Uint32(image[60:64])
	if !span(pe, 24+128) || string(image[pe:pe+4]) != "PE\x00\x00" || binary.LittleEndian.Uint16(image[pe+4:]) != 0x8664 {
		return bad()
	}
	optional := pe + 24
	if binary.LittleEndian.Uint16(image[pe+20:]) < 128 || binary.LittleEndian.Uint16(image[optional:]) != 0x20b || binary.LittleEndian.Uint32(image[optional+108:]) < 2 {
		return bad()
	}
	start, size := binary.LittleEndian.Uint32(image[optional+120:]), binary.LittleEndian.Uint32(image[optional+124:])
	if start == 0 || size < 20 || !span(start, size) {
		return bad()
	}
	nameAt := func(off uint32) (string, bool) {
		if !span(off, 1) {
			return "", false
		}
		end := bytes.IndexByte(image[off:], 0)
		if end < 0 || end > 256 {
			return "", false
		}
		return string(image[off : uint64(off)+uint64(end)]), true
	}
	var found uint32
	terminated := false
	for d := uint64(start); d+20 <= uint64(start)+uint64(size); d += 20 {
		descriptor := image[d : d+20]
		if bytes.Equal(descriptor, make([]byte, 20)) {
			terminated = true
			break
		}
		dll, ok := nameAt(binary.LittleEndian.Uint32(descriptor[12:]))
		if !ok {
			return bad()
		}
		if !strings.EqualFold(dll, "kernel32.dll") {
			continue
		}
		lookup, iat := binary.LittleEndian.Uint32(descriptor), binary.LittleEndian.Uint32(descriptor[16:])
		if lookup == 0 || iat == 0 || iat%8 != 0 {
			return bad()
		}
		for index := uint64(0); ; index++ {
			l, slot := uint64(lookup)+index*8, uint64(iat)+index*8
			if l+8 > uint64(len(image)) || slot+8 > uint64(len(image)) {
				return bad()
			}
			entry := binary.LittleEndian.Uint64(image[l : l+8])
			if entry == 0 {
				break
			}
			if entry>>63 != 0 {
				continue
			}
			if entry > uint64(len(image)) || entry+2 >= uint64(len(image)) {
				return bad()
			}
			name, ok := nameAt(uint32(entry + 2))
			if !ok {
				return bad()
			}
			if name == symbol {
				if found != 0 {
					return bad()
				}
				found = uint32(slot)
			}
		}
	}
	if !terminated || found == 0 {
		return bad()
	}
	return found, nil
}

func TestMappedImportSlot(t *testing.T) {
	fixture := func() []byte {
		b := make([]byte, 4096)
		copy(b, "MZ")
		binary.LittleEndian.PutUint32(b[60:], 128)
		copy(b[128:], "PE\x00\x00")
		binary.LittleEndian.PutUint16(b[132:], 0x8664)
		binary.LittleEndian.PutUint16(b[148:], 240)
		binary.LittleEndian.PutUint16(b[152:], 0x20b)
		binary.LittleEndian.PutUint32(b[260:], 16)
		binary.LittleEndian.PutUint32(b[272:], 512)
		binary.LittleEndian.PutUint32(b[276:], 40)
		binary.LittleEndian.PutUint32(b[512:], 800)
		binary.LittleEndian.PutUint32(b[524:], 768)
		binary.LittleEndian.PutUint32(b[528:], 832)
		copy(b[768:], "KERNEL32.dll\x00")
		binary.LittleEndian.PutUint64(b[800:], 896)
		copy(b[898:], "FindFirstVolumeW\x00")
		return b
	}
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
		valid  bool
	}{
		{"valid", func([]byte) {}, true},
		{"wrong machine", func(b []byte) { binary.LittleEndian.PutUint16(b[132:], 0x14c) }, false},
		{"bad PE offset", func(b []byte) { binary.LittleEndian.PutUint32(b[60:], 0xfffffff0) }, false},
		{"oversized directory", func(b []byte) { binary.LittleEndian.PutUint32(b[276:], 0xffffffff) }, false},
		{"wrong DLL", func(b []byte) { copy(b[768:], "OTHERDLL.dll\x00") }, false},
		{"ordinal only", func(b []byte) { binary.LittleEndian.PutUint64(b[800:], 1<<63|1) }, false},
		{"bad thunk", func(b []byte) { binary.LittleEndian.PutUint64(b[800:], 0x100000000) }, false},
		{"unaligned IAT", func(b []byte) { binary.LittleEndian.PutUint32(b[528:], 833) }, false},
		{"duplicate symbol", func(b []byte) { binary.LittleEndian.PutUint64(b[808:], 896) }, false},
		{"missing terminator", func(b []byte) { binary.LittleEndian.PutUint32(b[276:], 20) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fixture()
			tc.mutate(b)
			slot, err := mappedImportSlot(b, "FindFirstVolumeW")
			if (err == nil) != tc.valid || (tc.valid && slot != 832) {
				t.Fatalf("slot=%d err=%v", slot, err)
			}
		})
	}
}
