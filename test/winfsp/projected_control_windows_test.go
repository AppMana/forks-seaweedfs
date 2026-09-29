package winfsp

import (
	"testing"

	"golang.org/x/sys/windows"
)

// Use the same metadata-only directory access as WinFsp's absolute-target
// prefix walker. A container projection may wrap this file object even when
// controls still reach the backing filesystem. This is a live test, not a
// filesystem-label or volume-serial approximation of driver identity.
func TestWindowsProjectedWinFspControl(t *testing.T) {
	root := testRoot(t)
	for _, tc := range []struct {
		name, path string
		winfsp     bool
	}{
		{"mount", *mountPoint, true},
		{"child", root, true},
		{"external", t.TempDir(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, err := windows.UTF16PtrFromString(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			h, err := windows.CreateFile(path, windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
				nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(h)
			// WinFsp inc/winfsp/fsctl.h: FSP_FSCTL_QUERY_WINFSP.
			const queryWinFsp = (9 << 16) | ((0x800 + '?') << 2)
			var n uint32
			err = windows.DeviceIoControl(h, queryWinFsp, nil, 0, nil, 0, &n, nil)
			t.Logf("path=%q queryWinFsp=%v bytes=%d", tc.path, err, n)
			if (err == nil) != tc.winfsp {
				t.Fatalf("WinFsp control routing: %v", err)
			}
		})
	}
}
