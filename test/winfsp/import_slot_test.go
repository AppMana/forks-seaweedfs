package winfsp

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"
)

// mappedImportSlot locates one named kernel32 IAT slot in a mapped PE64 image.
// It is test-only: callers must independently verify the loaded module identity
// and restore the slot after injection. No process-wide API patching is needed.
func mappedImportSlot(image []byte, symbol string) (uint32, error) {
	bad := func() (uint32, error) { return 0, fmt.Errorf("invalid or ambiguous PE64 import %q", symbol) }
	span := func(off, n uint32) bool { return uint64(off)+uint64(n) <= uint64(len(image)) }
	if len(image) < 64 || len(image) > 64<<20 || string(image[:2]) != "MZ" {
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
				if found != 0 || binary.LittleEndian.Uint64(image[slot:slot+8]) == 0 {
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
		binary.LittleEndian.PutUint64(b[832:], 896)
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
		{"empty IAT", func(b []byte) { binary.LittleEndian.PutUint64(b[832:], 0) }, false},
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

// Construct the mapped layout from the exact DLL file without reading discarded
// or inaccessible pages of the loaded module. The live IAT target must still be
// verified separately before any memory change. OFT-less images are unsupported.
func labMappedPE(raw []byte) ([]byte, error) {
	if len(raw) > 64<<20 {
		return nil, fmt.Errorf("lab DLL exceeds size limit")
	}
	f, err := pe.NewFile(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h, ok := f.OptionalHeader.(*pe.OptionalHeader64)
	if !ok || h.SizeOfImage == 0 || h.SizeOfImage > 64<<20 || h.SizeOfHeaders > h.SizeOfImage || uint64(h.SizeOfHeaders) > uint64(len(raw)) {
		return nil, fmt.Errorf("unsupported lab PE image size/layout")
	}
	image := make([]byte, h.SizeOfImage)
	copy(image, raw[:h.SizeOfHeaders])
	type interval struct{ start, end uint64 }
	ranges := []interval{{0, uint64(h.SizeOfHeaders)}}
	for _, section := range f.Sections {
		start, end := uint64(section.VirtualAddress), uint64(section.VirtualAddress)+uint64(section.VirtualSize)
		if section.VirtualSize == 0 || end > uint64(len(image)) || uint64(section.Offset)+uint64(section.Size) > uint64(len(raw)) {
			return nil, fmt.Errorf("PE section outside file or mapped image")
		}
		for _, r := range ranges {
			if start < r.end && r.start < end {
				return nil, fmt.Errorf("overlapping PE virtual ranges")
			}
		}
		ranges = append(ranges, interval{start, end})
		count := min(section.Size, section.VirtualSize)
		copy(image[section.VirtualAddress:], raw[section.Offset:uint64(section.Offset)+uint64(count)])
	}
	return image, nil
}

func TestLabMappedPE(t *testing.T) {
	fixture := func() []byte {
		b := make([]byte, 2048)
		copy(b, "MZ")
		binary.LittleEndian.PutUint32(b[60:], 128)
		copy(b[128:], "PE\x00\x00")
		binary.LittleEndian.PutUint16(b[132:], 0x8664)
		binary.LittleEndian.PutUint16(b[134:], 2)
		binary.LittleEndian.PutUint16(b[148:], 240)
		binary.LittleEndian.PutUint16(b[152:], 0x20b)
		binary.LittleEndian.PutUint32(b[208:], 4096)
		binary.LittleEndian.PutUint32(b[212:], 512)
		binary.LittleEndian.PutUint32(b[260:], 16)
		for i, v := range []uint32{512, 768} {
			s := 392 + i*40
			copy(b[s:], ".test")
			binary.LittleEndian.PutUint32(b[s+8:], 16)
			binary.LittleEndian.PutUint32(b[s+12:], v)
			binary.LittleEndian.PutUint32(b[s+16:], 32)
			binary.LittleEndian.PutUint32(b[s+20:], uint32(1024+i*32))
		}
		copy(b[1024:], bytes.Repeat([]byte{'A'}, 32))
		copy(b[1056:], bytes.Repeat([]byte{'B'}, 32))
		return b
	}
	for _, tc := range []struct {
		name   string
		mutate func([]byte)
		valid  bool
	}{
		{"raw padding", func([]byte) {}, true},
		{"zero fill", func(b []byte) { binary.LittleEndian.PutUint32(b[400:], 48) }, true},
		{"header overlap", func(b []byte) { binary.LittleEndian.PutUint32(b[404:], 500) }, false},
		{"section overlap", func(b []byte) { binary.LittleEndian.PutUint32(b[444:], 520) }, false},
		{"raw bounds", func(b []byte) { binary.LittleEndian.PutUint32(b[412:], 2040) }, false},
		{"virtual bounds", func(b []byte) { binary.LittleEndian.PutUint32(b[404:], 4090) }, false},
		{"unsupported zero virtual size", func(b []byte) { binary.LittleEndian.PutUint32(b[400:], 0) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fixture()
			tc.mutate(b)
			mapped, err := labMappedPE(b)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if tc.valid {
				if !bytes.Equal(mapped[512:528], bytes.Repeat([]byte{'A'}, 16)) {
					t.Fatal("raw mapping incorrect")
				}
				start := 528
				if tc.name == "zero fill" {
					start = 544
				}
				if !bytes.Equal(mapped[start:start+16], make([]byte, 16)) {
					t.Fatal("padding was copied or tail not zero-filled")
				}
			}
		})
	}
}

func TestMappedImportSlotCandidate(t *testing.T) {
	path := os.Getenv("WINFSP_LAB_PE_TEST_DLL")
	if path == "" {
		t.Skip("optional local DLL import-layout check")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	image, err := labMappedPE(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"FindFirstVolumeW", "DeviceIoControl"} {
		slot, err := mappedImportSlot(image, name)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s IAT RVA=%#x", name, slot)
	}
}
