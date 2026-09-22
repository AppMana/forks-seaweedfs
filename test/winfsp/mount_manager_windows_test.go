package winfsp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

// A root-only filesystem isolates mount-manager behavior from Git and filer
// metadata. This opt-in test creates only disposable mounts below t.TempDir.
type mountManagerRootFS struct {
	fuse.FileSystemBase
	sentinel string
}

func (fs *mountManagerRootFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	if strings.EqualFold(path, "/"+fs.sentinel) {
		stat.Mode, stat.Nlink, stat.Ino = fuse.S_IFREG|0444, 1, 2
		return 0
	}
	if path != "/" {
		return -fuse.ENOENT
	}
	stat.Mode = fuse.S_IFDIR | 0755
	stat.Nlink = 2
	stat.Ino = 1
	return 0
}

func (fs *mountManagerRootFS) Open(path string, flags int) (int, uint64) {
	if strings.EqualFold(path, "/"+fs.sentinel) {
		return 0, 2
	}
	return -fuse.ENOENT, 0
}

func (*mountManagerRootFS) Read(path string, buff []byte, ofst int64, fh uint64) int { return 0 }

func TestMountManagerDirectoryLifecycle(t *testing.T) {
	if os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_MANAGER_LAB") != "1" {
		t.Skip("requires a disposable elevated Windows VM with WinFsp")
	}
	root := t.TempDir()
	for cycle := 0; cycle < 64; cycle++ {
		func() {
			point := filepath.Join(root, fmt.Sprintf("mnt-%02d", cycle))
			sentinel := fmt.Sprintf("sentinel-%02d-%d", cycle, time.Now().UnixNano())
			host := fuse.NewFileSystemHost(&mountManagerRootFS{sentinel: sentinel})
			host.SetCapCaseInsensitive(true)
			done := make(chan bool, 1)
			go func() {
				done <- host.Mount(`\\.\`+point, []string{"-o", "FileInfoTimeout=1000", "-o", "FileSystemName=seaweedfs"})
			}()
			defer func() {
				if !host.Unmount() {
					t.Error("WinFsp Unmount returned failure")
				}
				select {
				case ok := <-done:
					if !ok {
						t.Error("WinFsp Mount returned failure")
					}
				case <-time.After(10 * time.Second):
					t.Fatal("WinFsp unmount did not complete")
				}
			}()
			deadline := time.Now().Add(10 * time.Second)
			for {
				// This entry exists only in our callback, never on underlying NTFS.
				if _, err := os.Stat(filepath.Join(point, sentinel)); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("mount root never became accessible")
				}
				time.Sleep(10 * time.Millisecond)
			}
			for probe := 0; probe < 256; probe++ {
				path, err := windows.UTF16PtrFromString(point)
				if err != nil {
					t.Fatal(err)
				}
				h, err := windows.CreateFile(path, 0, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
				if err != nil {
					t.Fatalf("cycle=%d probe=%d CreateFile: %v", cycle, probe, err)
				}
				buf := make([]uint16, 32768)
				n, pathErr := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
				if pathErr != nil || n == 0 || n >= uint32(len(buf)) {
					for _, flags := range []uint32{1, 2, 8, 9, 10} {
						b := make([]uint16, 32768)
						count, e := windows.GetFinalPathNameByHandle(h, &b[0], uint32(len(b)), flags)
						t.Logf("flags=%d n=%d name=%q err=%v", flags, count, windows.UTF16ToString(b), e)
					}
					windows.CloseHandle(h)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					listing, listingErr := exec.CommandContext(ctx, "mountvol.exe").CombinedOutput()
					cancel()
					t.Logf("mountvol err=%v\n%s", listingErr, listing)
					t.Fatalf("cycle=%d probe=%d DOS path n=%d err=%v", cycle, probe, n, pathErr)
				}
				if err := windows.CloseHandle(h); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(windows.UTF16ToString(buf), sentinel)); err != nil {
					t.Fatalf("returned DOS path inaccessible: %v", err)
				}
				time.Sleep(2 * time.Millisecond)
			}
			t.Logf("cycle=%d: 256 DOS-path queries succeeded", cycle)
		}()
	}
}
