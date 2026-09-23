package winfsp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

func assertNoLabMountMapping(t *testing.T, point string) {
	t.Helper()
	buf := make([]uint16, 1024)
	h, err := windows.FindFirstVolume(&buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.FindVolumeClose(h); err != nil {
			t.Error(err)
		}
	}()
	for {
		guid := windows.UTF16ToString(buf)
		paths, err := mountManagerVolumePaths(guid)
		if err != nil && err != windows.ERROR_FILE_NOT_FOUND && err != windows.ERROR_PATH_NOT_FOUND {
			t.Fatal(err)
		}
		for _, path := range paths {
			if strings.EqualFold(filepath.Clean(path), filepath.Clean(point)) {
				t.Fatalf("stale mapping %q -> %q", guid, path)
			}
		}
		err = windows.FindNextVolume(h, &buf[0], uint32(len(buf)))
		if err == windows.ERROR_NO_MORE_FILES {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(point); !os.IsNotExist(err) {
		t.Fatalf("mount junction remains: %v", err)
	}
}

func TestMountManagerRegistrationRollback(t *testing.T) {
	if os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_MANAGER_LAB") != "1" {
		t.Skip("requires isolated Windows VM")
	}
	setError := windows.NewLazySystemDLL("kernel32.dll").NewProc("SetLastError")
	if err := setError.Find(); err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"guid-lookup", "guid-reparse"} {
		t.Run(fault, func(t *testing.T) {
			root := t.TempDir()
			point := filepath.Join(root, "owned-mount")
			sibling := filepath.Join(root, "sibling.txt")
			if err := os.WriteFile(sibling, []byte(crashSiblingContent), 0600); err != nil {
				t.Fatal(err)
			}
			var calls, fired atomic.Int32
			var callback uintptr
			symbol := "FindFirstVolumeW"
			if fault == "guid-reparse" {
				symbol = "DeviceIoControl"
			}
			api := windows.NewLazySystemDLL("kernel32.dll").NewProc(symbol)
			if err := api.Find(); err != nil {
				t.Fatal(err)
			}
			original := api.Addr() // immutable and initialized before hook installation
			if fault == "guid-lookup" {
				callback = syscall.NewCallback(func(name, size uintptr) uintptr {
					calls.Add(1)
					fired.Add(1)
					setError.Call(uintptr(windows.ERROR_ACCESS_DENIED))
					return ^uintptr(0)
				})
			} else {
				callback = syscall.NewCallback(func(h, code, in, inSize, out, outSize, returned, overlapped uintptr) uintptr {
					if code == uintptr(windows.FSCTL_SET_REPARSE_POINT) && calls.Add(1) == 2 {
						fired.Add(1)
						setError.Call(uintptr(windows.ERROR_ACCESS_DENIED))
						return 0
					}
					r, _, err := syscall.SyscallN(original, h, code, in, inSize, out, outSize, returned, overlapped)
					setError.Call(uintptr(err))
					return r
				})
			}
			installedOriginal, restore := labImportHook(t, symbol, callback)
			if installedOriginal != original {
				t.Fatal("callback original differs from validated import")
			}
			host := fuse.NewFileSystemHost(&mountManagerRootFS{sentinel: "rollback-sentinel"})
			done := make(chan bool, 1)
			go func() { done <- host.Mount(`\\.\`+point, nil) }()
			select {
			case ok := <-done:
				if ok {
					t.Fatal("injected mount unexpectedly succeeded")
				}
			case <-time.After(10 * time.Second):
				// A missing injection must fail, never become a passing mount.
				if !waitForLabMountQuiescence(host.Unmount, done, 5*time.Second) {
					// Do not run Go cleanups which would mutate/unload a live DLL.
					// This process and all its paths are owned by the isolated VM.
					fmt.Fprintln(os.Stderr, "FATAL: mount DLL did not quiesce; aborting isolated test process without hook restoration")
					os.Exit(2)
				}
				t.Fatal("injected mount did not fail within deadline")
			}
			wantCalls := int32(1)
			if fault == "guid-reparse" {
				wantCalls = 2
			}
			if fired.Load() != 1 || calls.Load() != wantCalls {
				t.Fatalf("wrong injection: calls=%d fired=%d", calls.Load(), fired.Load())
			}
			restore()
			assertNoLabMountMapping(t, point)
			// The normal candidate must reuse the identical path after rollback.
			retry := fuse.NewFileSystemHost(&mountManagerRootFS{sentinel: "retry-sentinel"})
			retryDone := make(chan bool, 1)
			go func() { retryDone <- retry.Mount(`\\.\`+point, nil) }()
			defer func() {
				if !retry.Unmount() {
					t.Error("retry unmount failed")
				}
				select {
				case ok := <-retryDone:
					if !ok {
						t.Error("retry mount failed")
					}
				case <-time.After(10 * time.Second):
					t.Error("retry unmount timeout")
				}
			}()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(point, "retry-sentinel")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("restored candidate could not reuse mount path")
				}
				time.Sleep(10 * time.Millisecond)
			}
			data, err := os.ReadFile(sibling)
			if err != nil || string(data) != crashSiblingContent {
				t.Fatalf("rollback changed sibling: %v", err)
			}
			t.Logf("rollback verified: %s fired once, mapping removed, same path reused", fault)
		})
	}
}
