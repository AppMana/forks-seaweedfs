package winfsp

import (
	"encoding/binary"
	"testing"
)

func TestMountPointReparseRoundTrip(t *testing.T) {
	for _, target := range []string{
		`\??\Volume{7c5aae82-c6e6-11f1-94f7-c7d2afe5d61a}\`,
		`\Device\Volume{7c5aae80-c6e6-11f1-94f7-c7d2afe5d61a}\`,
	} {
		buf := mountPointReparseBuffer(target)
		got, err := mountPointReparseTarget(buf)
		if err != nil {
			t.Fatalf("%q: %v", target, err)
		}
		if got != target {
			t.Fatalf("round trip = %q, want %q", got, target)
		}
		if int(binary.LittleEndian.Uint16(buf[4:6])) != len(buf)-8 {
			t.Fatalf("%q: reparse data length %d, buffer %d", target, binary.LittleEndian.Uint16(buf[4:6]), len(buf))
		}
	}
}

// Verify the mount-point reparse buffer layout produced by WinFsp 2.1 for a
// mount-manager directory mount.
func TestMountPointReparseTargetParsesWinFspJunction(t *testing.T) {
	target := `\Device\Volume{7c5aae80-c6e6-11f1-94f7-c7d2afe5d61a}\`
	buf := mountPointReparseBuffer(target)
	if len(buf)-8 != 0xe0 {
		t.Fatalf("reparse data length %#x, fsutil reported 0xe0", len(buf)-8)
	}
	got, err := mountPointReparseTarget(buf)
	if err != nil || got != target {
		t.Fatalf("target = %q, %v", got, err)
	}
}

func TestMountPointReparseTargetRejectsMalformed(t *testing.T) {
	good := mountPointReparseBuffer(`\??\Volume{x}\`)
	symlink := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(symlink[:4], 0xA000000C)
	overrun := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(overrun[10:12], 0x7ffe)
	odd := append([]byte(nil), good...)
	binary.LittleEndian.PutUint16(odd[8:10], 1)
	for name, buf := range map[string][]byte{
		"short":   good[:12],
		"symlink": symlink,
		"overrun": overrun,
		"odd":     odd,
	} {
		if _, err := mountPointReparseTarget(buf); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
