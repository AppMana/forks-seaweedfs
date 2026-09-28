package winfsp

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// These are native Windows attributes, not POSIX mode approximations. Clearing
// ARCHIVE must also persist: backup software uses that bit as mutable state.
func TestWindowsAttributesRoundTrip(t *testing.T) {
	root := testRoot(t)
	for _, flags := range []uint32{windows.FILE_ATTRIBUTE_HIDDEN, windows.FILE_ATTRIBUTE_SYSTEM, windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_ARCHIVE, windows.FILE_ATTRIBUTE_NORMAL} {
		t.Run(fmt.Sprintf("%x", flags), func(t *testing.T) {
			name := filepath.Join(root, fmt.Sprintf("file-%x", flags))
			if err := writeAndSync(name, []byte("intact attribute payload")); err != nil {
				t.Fatal(err)
			}
			p, err := windows.UTF16PtrFromString(name)
			if err != nil {
				t.Fatal(err)
			}
			defer windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_NORMAL)
			if err := windows.SetFileAttributes(p, flags); err != nil {
				t.Fatal(err)
			}
			assertWindowsAttributes(t, name, flags)
			if data, err := os.ReadFile(name); err != nil || string(data) != "intact attribute payload" {
				t.Fatalf("attribute update damaged data: %q %v", data, err)
			}
		})
	}
}

func TestWindowsUnicodeComponentLimits(t *testing.T) {
	root := testRoot(t)
	for _, name := range []string{strings.Repeat("a", 255), strings.Repeat("界", 255), strings.Repeat("🐟", 127) + "a"} {
		path := filepath.Join(root, name)
		if err := writeAndSync(path, []byte("unicode intact payload")); err != nil {
			t.Errorf("create %d-byte valid Windows name: %v", len(name), err)
			continue
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "unicode intact payload" {
			t.Errorf("read valid name: %q %v", data, err)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, entry := range entries {
			if entry.Name() == name {
				found = true
			}
		}
		if !found {
			t.Error("directory listing lost Unicode filename")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{strings.Repeat("a", 256), strings.Repeat("界", 256), strings.Repeat("🐟", 128)} {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0666); err == nil {
			t.Errorf("accepted overlong Windows component: %d bytes", len(name))
		}
	}
}

func TestWindowsCreationTimeStable(t *testing.T) {
	name := filepath.Join(testRoot(t), "birth.txt")
	if err := writeAndSync(name, []byte("birth time payload")); err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var before, after windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &before); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1700000000, 123456700)
	if err := os.Chtimes(name, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := windows.GetFileInformationByHandle(h, &after); err != nil {
		t.Fatal(err)
	}
	if before.CreationTime != after.CreationTime {
		t.Errorf("changing modification time changed creation time: %v -> %v", before.CreationTime, after.CreationTime)
	}
	want := windows.NsecToFiletime(stamp.UnixNano())
	if err := windows.SetFileTime(h, &want, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := windows.GetFileInformationByHandle(h, &after); err != nil {
		t.Fatal(err)
	}
	if after.CreationTime != want {
		t.Errorf("creation time=%v want exact 100ns value %v", after.CreationTime, want)
	}
}

func TestWindowsNFSReparseRoundTrip(t *testing.T) {
	name := filepath.Join(testRoot(t), "special")
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var data [24]byte
	binary.LittleEndian.PutUint32(data[:4], 0x80000014) // IO_REPARSE_TAG_NFS
	binary.LittleEndian.PutUint16(data[4:6], 16)
	binary.LittleEndian.PutUint64(data[8:16], 0x524843) // NFS_SPECFILE_CHR
	binary.LittleEndian.PutUint32(data[16:20], 0x42)
	binary.LittleEndian.PutUint32(data[20:24], 0x62)
	var count uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &data[0], uint32(len(data)), nil, 0, &count, nil); err != nil {
		t.Fatal(err)
	}
	var got [16384]byte
	if err := windows.DeviceIoControl(h, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &got[0], uint32(len(got)), &count, nil); err != nil {
		t.Fatal(err)
	}
	if count != uint32(len(data)) || string(got[:count]) != string(data[:]) {
		t.Fatalf("reparse data=%x count=%d want=%x", got[:count], count, data)
	}
}

func assertWindowsAttributes(t *testing.T, name string, want uint32) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := windows.GetFileAttributes(p)
	if err != nil || got != want {
		t.Fatalf("attributes of %s = %#x, %v; want %#x", name, got, err, want)
	}
}

func TestWindowsAttributesPersistence(t *testing.T) {
	if *mountPoint == "" || (*phase != "write" && *phase != "verify") {
		t.Skip("requires mounted write/remount/verify phases")
	}
	root := filepath.Join(*mountPoint, "winfsp-attributes-persist")
	if *phase == "write" {
		if err := os.Mkdir(root, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, flags := range []uint32{windows.FILE_ATTRIBUTE_HIDDEN | windows.FILE_ATTRIBUTE_SYSTEM, windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_ATTRIBUTE_READONLY | windows.FILE_ATTRIBUTE_ARCHIVE} {
		name := filepath.Join(root, fmt.Sprintf("file-%x", flags))
		p, err := windows.UTF16PtrFromString(name)
		if err != nil {
			t.Fatal(err)
		}
		if *phase == "write" {
			if err := writeAndSync(name, []byte("persistent intact payload")); err != nil {
				t.Fatal(err)
			}
			if err := windows.SetFileAttributes(p, flags); err != nil {
				t.Fatal(err)
			}
		}
		assertWindowsAttributes(t, name, flags)
		if data, err := os.ReadFile(name); err != nil || string(data) != "persistent intact payload" {
			t.Fatalf("persistent payload differs: %q %v", data, err)
		}
	}
}

func TestDefaultFileAttributesArchive(t *testing.T) {
	name := filepath.Join(testRoot(t), "ArchiveFile.txt")
	if err := os.WriteFile(name, []byte("intact payload"), 0666); err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	attrs, err := windows.GetFileAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	if attrs != windows.FILE_ATTRIBUTE_ARCHIVE {
		t.Fatalf("file attributes=%#x, want ARCHIVE=%#x", attrs, windows.FILE_ATTRIBUTE_ARCHIVE)
	}
}

// Register the real Windows watcher before creating a file; no sleeps, polling,
// or retries can turn an incorrect notification name into a passing result.
func TestDirectoryChangeNotificationPreservesCase(t *testing.T) {
	if *mountPoint == "" {
		t.Skip("requires a real mounted filesystem")
	}
	if os.Getenv("SEAWEEDFS_NOTIFICATION_CHILD") != "1" {
		// A broken driver can hang cancellation too. A bounded child owns the
		// kernel I/O and pinned memory until completion or process teardown.
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestDirectoryChangeNotificationPreservesCase$", "-test.v", "-test.count=1", "-test.timeout=15s", "-mountpoint="+*mountPoint)
		cmd.Env = append(os.Environ(), "SEAWEEDFS_NOTIFICATION_CHILD=1")
		output, err := cmd.CombinedOutput()
		t.Logf("notification child:\n%s", output)
		if err != nil || ctx.Err() != nil || !strings.Contains(string(output), "--- PASS: TestDirectoryChangeNotificationPreservesCase ") || strings.Contains(string(output), "--- SKIP:") {
			t.Fatalf("native notification probe failed: %v deadline=%v", err, ctx.Err())
		}
		return
	}
	dir := testRoot(t)
	if err := os.Mkdir(filepath.Join(dir, "Subdirectory"), 0777); err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(event)
	overlap := windows.Overlapped{HEvent: event}
	buf := make([]byte, 4096)
	var pinned runtime.Pinner
	pinned.Pin(&overlap)
	pinned.Pin(&buf[0])
	defer pinned.Unpin()
	var count uint32
	if err := windows.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)), true,
		windows.FILE_NOTIFY_CHANGE_FILE_NAME, nil, &overlap, 0); err != nil && err != windows.ERROR_IO_PENDING {
		t.Fatal(err)
	}
	defer func() {
		windows.CancelIoEx(h, &overlap)
		windows.GetOverlappedResult(h, &overlap, &count, true)
		runtime.KeepAlive(buf)
		runtime.KeepAlive(&overlap)
	}()
	if err := os.WriteFile(filepath.Join(dir, "Subdirectory", "file0"), []byte("payload"), 0666); err != nil {
		t.Fatal(err)
	}
	state, err := windows.WaitForSingleObject(event, 10000)
	if err != nil || state != windows.WAIT_OBJECT_0 {
		t.Fatalf("notification wait=%d err=%v", state, err)
	}
	if err := windows.GetOverlappedResult(h, &overlap, &count, false); err != nil {
		t.Fatal(err)
	}
	if count < 12 {
		t.Fatalf("short notification: %d", count)
	}
	action := binary.LittleEndian.Uint32(buf[4:8])
	length := binary.LittleEndian.Uint32(buf[8:12])
	if length%2 != 0 || length > count-12 {
		t.Fatalf("invalid notification name length %d in %d bytes", length, count)
	}
	name := make([]uint16, length/2)
	for i := range name {
		name[i] = binary.LittleEndian.Uint16(buf[12+2*i:])
	}
	got := string(utf16.Decode(name))
	if action != windows.FILE_ACTION_ADDED || got != `Subdirectory\file0` {
		t.Fatalf("notification action=%d name=%q; want ADDED %q", action, got, `Subdirectory\file0`)
	}
}
