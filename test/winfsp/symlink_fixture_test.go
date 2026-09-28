package winfsp

import (
	"strings"
	"testing"
)

type symlinkFilesystemIdentity struct {
	canonicalVolume string
	filesystem      string
	serial          uint32
}

func distinctSymlinkFilesystems(a, b symlinkFilesystemIdentity) bool {
	// A container's projected path can retain the container C: GUID even
	// though the opened file is serviced by WinFsp. Conversely an alias alone
	// does not prove separation. Require positive handle-query evidence.
	if a.filesystem == "" || b.filesystem == "" {
		return false
	}
	return !strings.EqualFold(a.filesystem, b.filesystem) ||
		(a.serial != 0 && b.serial != 0 && a.serial != b.serial)
}

func TestCrossVolumeFixtureIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b symlinkFilesystemIdentity
		want bool
	}{
		{"projected GUID is not filesystem identity", symlinkFilesystemIdentity{"container-C", "FUSE-seaweedfs", 0}, symlinkFilesystemIdentity{"container-C", "NTFS", 123}, true},
		{"separate serials", symlinkFilesystemIdentity{"container-C", "NTFS", 123}, symlinkFilesystemIdentity{"container-C", "NTFS", 456}, true},
		{"same identity", symlinkFilesystemIdentity{"C", "NTFS", 123}, symlinkFilesystemIdentity{"c", "ntfs", 123}, false},
		{"alias alone is insufficient", symlinkFilesystemIdentity{"alias-a", "NTFS", 123}, symlinkFilesystemIdentity{"alias-b", "NTFS", 123}, false},
		{"missing filesystem identity", symlinkFilesystemIdentity{"C", "", 0}, symlinkFilesystemIdentity{"D", "NTFS", 123}, false},
		{"zero serial does not prove separation", symlinkFilesystemIdentity{"C", "NTFS", 0}, symlinkFilesystemIdentity{"C", "NTFS", 123}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := distinctSymlinkFilesystems(tc.a, tc.b); got != tc.want {
				t.Fatalf("distinct filesystems = %v, want %v", got, tc.want)
			}
		})
	}
}
