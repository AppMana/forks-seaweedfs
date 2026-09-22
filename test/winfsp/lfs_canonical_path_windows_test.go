package winfsp

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Exercise the actual Git LFS 3.7.0 CanonicalizeSystemPath syscall sequence,
// not just an uppercase alias passed to os.Rename. Keep failures stage-specific:
// opening a directory and querying its normalized name are separate operations.
func TestGitLfsDirectoryCanonicalization(t *testing.T) {
	root := testRoot(t)
	for iteration := 0; iteration < 32; iteration++ {
		dir := filepath.Join(root, fmt.Sprintf("repo-%d", iteration), ".git")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		for change := 0; change < 4; change++ {
			want := []byte(fmt.Sprintf("config-%d-%d", iteration, change))
			lock, config := filepath.Join(dir, "config.lock"), filepath.Join(dir, "config")
			if err := os.WriteFile(lock, want, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(lock, config); err != nil {
				t.Fatal(err)
			}
			for _, spelling := range []string{dir, strings.ToUpper(dir)} {
				name, err := windows.UTF16PtrFromString(spelling)
				if err != nil {
					t.Fatal(err)
				}
				handle, err := windows.CreateFile(name, 0, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
				if err != nil {
					t.Fatalf("iteration %d change %d CreateFile(%q): %v", iteration, change, spelling, err)
				}
				buffer := make([]uint16, 100)
				for {
					var n uint32
					n, err = windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
					if err != nil || n < uint32(len(buffer)) {
						break
					}
					buffer = make([]uint16, n+1)
				}
				closeErr := windows.CloseHandle(handle)
				if err != nil {
					t.Fatalf("iteration %d change %d GetFinalPathNameByHandle(%q): %v", iteration, change, spelling, err)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
				canonical := windows.UTF16ToString(buffer)
				// Do not require a particular casing: require that the returned
				// path actually reaches the intact object, as LFS needs it to.
				got, err := os.ReadFile(filepath.Join(canonical, "config"))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("canonical path %q does not reach intact config: read=%v got=%q want=%q", canonical, err, got, want)
				}
			}
		}
	}
}
