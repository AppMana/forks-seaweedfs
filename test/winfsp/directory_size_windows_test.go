package winfsp

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// Git for Windows can use directory enumeration metadata to decide whether
// to run its content/LFS filter. A direct Stat succeeding is not sufficient.
func TestDirectoryEnumerationSizeAfterClose(t *testing.T) {
	dir := testRoot(t)
	path := filepath.Join(dir, "asset")
	pattern, err := windows.UTF16PtrFromString(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"initial", "changed-longer"} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		var data windows.Win32finddata
		h, err := windows.FindFirstFile(pattern, &data)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for {
			if windows.UTF16ToString(data.FileName[:]) == "asset" {
				found = true
				size := uint64(data.FileSizeHigh)<<32 | uint64(data.FileSizeLow)
				if size != uint64(len(content)) {
					t.Errorf("directory size after close = %d, want %d", size, len(content))
				}
				break
			}
			if err = windows.FindNextFile(h, &data); err != nil {
				break
			}
		}
		if err := windows.FindClose(h); err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatalf("closed file missing from directory: %v", err)
		}
	}
}
