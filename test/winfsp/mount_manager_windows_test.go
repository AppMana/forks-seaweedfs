package winfsp

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

var mountManagerGUIDJunction = flag.Bool("mount-manager-guid-junction", false, "lab experiment: replace only the owned junction's NT target with its volume GUID")
var mountManagerNTJunctionControl = flag.Bool("mount-manager-nt-junction-control", false, "lab control: perform identical identity queries and rewrite the original NT junction target")
var mountManagerCheckCleanup = flag.Bool("mount-manager-check-cleanup", false, "lab qualification: reuse the same mount path and verify junction removal and sibling preservation")
var mountManagerCrashChild = flag.Bool("mount-manager-crash-child", false, "internal lab child: signal readiness and await termination before unmount")

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
	if *mountManagerGUIDJunction && *mountManagerNTJunctionControl {
		t.Fatal("junction treatment and control are mutually exclusive")
	}
	t.Logf("junction experiment: guid=%t nt-control=%t", *mountManagerGUIDJunction, *mountManagerNTJunctionControl)
	root := t.TempDir()
	if *mountManagerCrashChild {
		root = os.Getenv("SEAWEEDFS_MOUNT_CRASH_ROOT")
		if root == "" || !*mountManagerCheckCleanup || os.Getenv("SEAWEEDFS_MOUNT_CRASH_READY") == "" {
			t.Fatal("crash child requires parent-owned root, readiness path and cleanup checks")
		}
		if err := validateCrashOwnership(root, os.Getenv("SEAWEEDFS_MOUNT_CRASH_READY"), os.Getenv("SEAWEEDFS_MOUNT_CRASH_TOKEN")); err != nil {
			t.Fatal(err)
		}
	}
	sibling := filepath.Join(root, "unrelated-data.txt")
	const siblingContent = "unrelated data must survive mount cleanup"
	if *mountManagerCheckCleanup && !*mountManagerCrashChild {
		if err := os.WriteFile(sibling, []byte(siblingContent), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for cycle := 0; cycle < 64; cycle++ {
		func() {
			point := filepath.Join(root, fmt.Sprintf("mnt-%02d", cycle))
			if *mountManagerCheckCleanup {
				point = filepath.Join(root, "reused-mount")
			}
			var mountedGUID string
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
				if *mountManagerCheckCleanup {
					if mountedGUID != "" {
						paths, err := mountManagerVolumePaths(mountedGUID)
						if err != nil && err != windows.ERROR_FILE_NOT_FOUND && err != windows.ERROR_PATH_NOT_FOUND {
							t.Errorf("query removed volume %q: %v", mountedGUID, err)
						}
						for _, path := range paths {
							if strings.EqualFold(filepath.Clean(path), filepath.Clean(point)) {
								t.Errorf("cycle=%d: stale mount mapping %q -> %q", cycle, mountedGUID, path)
							}
						}
					}
					// Lstat observes the junction itself: an inaccessible target
					// must not masquerade as removal of a leftover junction.
					if _, err := os.Lstat(point); !os.IsNotExist(err) {
						t.Errorf("cycle=%d: mount point remains after unmount: %v", cycle, err)
					}
					data, err := os.ReadFile(sibling)
					if err != nil || string(data) != siblingContent {
						t.Errorf("cycle=%d: unrelated sibling changed: %v", cycle, err)
					}
					if !t.Failed() {
						t.Logf("cycle=%d: owned junction removed and sibling preserved", cycle)
						t.Logf("cycle=%d: owned mount mapping removed", cycle)
					}
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
			if *mountManagerGUIDJunction || *mountManagerNTJunctionControl {
				setLabMountGUIDTarget(t, point, *mountManagerGUIDJunction)
				if _, err := os.Stat(filepath.Join(point, sentinel)); err != nil {
					t.Fatalf("junction intervention lost virtual sentinel: %v", err)
				}
			}
			if cycle == 0 {
				if expected := os.Getenv("SEAWEEDFS_WINDOWS_EXPECT_WINFSP_DLL"); expected != "" {
					var module windows.Handle
					name, err := windows.UTF16PtrFromString("winfsp-x64.dll")
					if err != nil {
						t.Fatal(err)
					}
					if err := windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, name, &module); err != nil {
						t.Fatal(err)
					}
					buf := make([]uint16, 32768)
					n, err := windows.GetModuleFileName(module, &buf[0], uint32(len(buf)))
					if err != nil || n == 0 || n >= uint32(len(buf)) {
						t.Fatalf("loaded DLL path: n=%d err=%v", n, err)
					}
					actual := windows.UTF16ToString(buf)
					if !strings.EqualFold(filepath.Clean(actual), filepath.Clean(expected)) {
						t.Fatalf("DLL fallback: loaded %q, expected %q", actual, expected)
					}
					t.Logf("verified loaded lab WinFsp DLL: %s", actual)
				}
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
				// Observe delayed mount-manager state changes as well as initial
				// registration. Every lookup is still single-shot: the interval
				// must never turn a failed lookup into a retry-and-pass.
				time.Sleep(10 * time.Millisecond)
			}
			t.Logf("cycle=%d: 256 DOS-path queries succeeded", cycle)
			if *mountManagerCheckCleanup {
				// Query identity only after the canonicalization checks, so it
				// cannot prime registration and conceal the original regression.
				p, err := windows.UTF16PtrFromString(point)
				if err != nil {
					t.Fatal(err)
				}
				h, err := windows.CreateFile(p, 0, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
				if err != nil {
					t.Fatal(err)
				}
				buf := make([]uint16, 32768)
				n, queryErr := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 1)
				closeErr := windows.CloseHandle(h)
				if queryErr != nil || closeErr != nil || n == 0 || n >= uint32(len(buf)) {
					t.Fatalf("volume identity: query=%v close=%v n=%d", queryErr, closeErr, n)
				}
				mountedGUID = windows.UTF16ToString(buf)
				paths, err := mountManagerVolumePaths(mountedGUID)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, path := range paths {
					found = found || strings.EqualFold(filepath.Clean(path), filepath.Clean(point))
				}
				if !found {
					t.Fatalf("mounted path %q absent from GUID %q paths %q", point, mountedGUID, paths)
				}
				if *mountManagerCrashChild {
					ready := os.Getenv("SEAWEEDFS_MOUNT_CRASH_READY")
					if err := os.WriteFile(ready+".tmp", []byte(mountedGUID), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(ready+".tmp", ready); err != nil {
						t.Fatal(err)
					}
					// The parent terminates this process. No Unmount or deferred
					// cleanup runs; the guest OS must release the mount handles.
					select {}
				}
			}
		}()
	}
}

// GetVolumePathNamesForVolumeName returns a MULTI_SZ and the required WCHAR
// count on ERROR_MORE_DATA. Only buffer growth is retried, never a missing path.
func mountManagerVolumePaths(guid string) ([]string, error) {
	p, err := windows.UTF16PtrFromString(guid)
	if err != nil {
		return nil, err
	}
	buf := make([]uint16, 256)
	for attempt := 0; attempt < 3; attempt++ {
		var n uint32
		err := windows.GetVolumePathNamesForVolumeName(p, &buf[0], uint32(len(buf)), &n)
		if err == windows.ERROR_MORE_DATA && n > uint32(len(buf)) && n <= 1<<20 {
			buf = make([]uint16, n)
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 || n > uint32(len(buf)) || buf[n-1] != 0 {
			return nil, fmt.Errorf("invalid volume path buffer length %d", n)
		}
		var paths []string
		for start, i := 0, 0; i < int(n); i++ {
			if buf[i] == 0 {
				if i == start {
					return paths, nil
				}
				paths = append(paths, windows.UTF16ToString(buf[start:i]))
				start = i + 1
			}
		}
		return nil, fmt.Errorf("unterminated volume path list")
	}
	return nil, fmt.Errorf("volume path list kept growing")
}

// Diagnostic intervention before any assertions, never a repair after failure.
// Only called with this test's freshly created mount, under t.TempDir. The
// junction must still target the exact device identified through its open root.
func setLabMountGUIDTarget(t *testing.T, point string, useGUID bool) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(point)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	query := func(flags uint32) string {
		buf := make([]uint16, 32768)
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
		if err != nil || n == 0 || n >= uint32(len(buf)) {
			t.Fatalf("intervention query flags=%d n=%d err=%v", flags, n, err)
		}
		return windows.UTF16ToString(buf)
	}
	guid, device := query(1), query(2)
	if !strings.HasPrefix(guid, `\\?\Volume{`) || !strings.HasSuffix(guid, `}\`) || !strings.HasPrefix(device, `\Device\Volume{`) {
		t.Fatalf("unexpected volume identities: GUID=%q device=%q", guid, device)
	}
	jh, err := windows.CreateFile(p, windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(jh)
	old := make([]byte, 16384)
	var n uint32
	if err := windows.DeviceIoControl(jh, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &old[0], uint32(len(old)), &n, nil); err != nil {
		t.Fatal(err)
	}
	if n < 16 || binary.LittleEndian.Uint32(old[:4]) != windows.IO_REPARSE_TAG_MOUNT_POINT {
		t.Fatal("owned mount is not a junction")
	}
	off, size := int(binary.LittleEndian.Uint16(old[8:10])), int(binary.LittleEndian.Uint16(old[10:12]))
	if off%2 != 0 || size%2 != 0 || 16+off+size > int(n) {
		t.Fatal("invalid existing junction buffer")
	}
	name := make([]uint16, size/2)
	for i := range name {
		name[i] = binary.LittleEndian.Uint16(old[16+off+2*i:])
	}
	if !strings.EqualFold(string(utf16.Decode(name)), device) {
		t.Fatalf("junction target does not match owned device: %q != %q", string(utf16.Decode(name)), device)
	}
	target := `\??\` + strings.TrimPrefix(guid, `\\?\`)
	if !useGUID {
		target = device
	}
	u, err := windows.UTF16FromString(target)
	if err != nil {
		t.Fatal(err)
	}
	bytesPerName := len(u) * 2
	data := make([]byte, 16+2*bytesPerName)
	binary.LittleEndian.PutUint32(data[:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(data[4:6], uint16(len(data)-8))
	binary.LittleEndian.PutUint16(data[10:12], uint16(bytesPerName-2))
	binary.LittleEndian.PutUint16(data[12:14], uint16(bytesPerName))
	binary.LittleEndian.PutUint16(data[14:16], uint16(bytesPerName-2))
	for i, c := range u {
		binary.LittleEndian.PutUint16(data[16+2*i:], c)
		binary.LittleEndian.PutUint16(data[16+bytesPerName+2*i:], c)
	}
	if err := windows.DeviceIoControl(jh, windows.FSCTL_SET_REPARSE_POINT, &data[0], uint32(len(data)), nil, 0, &n, nil); err != nil {
		t.Fatalf("junction intervention failed: %v", err)
	}
	if err := windows.DeviceIoControl(jh, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &old[0], uint32(len(old)), &n, nil); err != nil {
		t.Fatal(err)
	}
	if n < 16 || binary.LittleEndian.Uint32(old[:4]) != windows.IO_REPARSE_TAG_MOUNT_POINT {
		t.Fatal("rewritten mount is not a junction")
	}
	off, size = int(binary.LittleEndian.Uint16(old[8:10])), int(binary.LittleEndian.Uint16(old[10:12]))
	if off%2 != 0 || size%2 != 0 || 16+off+size > int(n) {
		t.Fatal("invalid rewritten junction buffer")
	}
	name = make([]uint16, size/2)
	for i := range name {
		name[i] = binary.LittleEndian.Uint16(old[16+off+2*i:])
	}
	if string(utf16.Decode(name)) != target {
		t.Fatalf("junction readback does not match intervention: %q != %q", string(utf16.Decode(name)), target)
	}
	t.Logf("EXPERIMENT: junction target %q -> %q", device, target)
}
