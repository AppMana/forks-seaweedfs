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
		if *phase == "verify" {
			if err := windows.SetFileAttributes(p, windows.FILE_ATTRIBUTE_NORMAL); err != nil {
				t.Fatal(err)
			}
		}
	}
	if *phase == "verify" {
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
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
