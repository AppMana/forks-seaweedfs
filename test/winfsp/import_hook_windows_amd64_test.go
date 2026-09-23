package winfsp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// labImportHook changes one IAT pointer in the explicitly selected lab DLL.
// Call only while no mount is running; restore before starting a normal mount.
// The library reference and restoration are also registered as test cleanups.
func labImportHook(t *testing.T, symbol string, callback uintptr) (uintptr, func()) {
	t.Helper()
	path := os.Getenv("SEAWEEDFS_WINDOWS_EXPECT_WINFSP_DLL")
	if os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_MANAGER_LAB") != "1" || !filepath.IsAbs(path) || callback == 0 {
		t.Fatal("IAT injection requires isolated lab, explicit absolute DLL and callback")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	image, err := labMappedPE(raw)
	if err != nil {
		t.Fatal(err)
	}
	rva, err := mappedImportSlot(image, symbol)
	if err != nil {
		t.Fatal(err)
	}
	dll, err := windows.LoadDLL(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := dll.Release(); err != nil {
			t.Error(err)
		}
	})
	buf := make([]uint16, 32768)
	n, err := windows.GetModuleFileName(windows.Handle(dll.Handle), &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || n >= uint32(len(buf)) || !strings.EqualFold(filepath.Clean(windows.UTF16ToString(buf)), filepath.Clean(path)) {
		t.Fatalf("loaded DLL identity mismatch: %q %v", windows.UTF16ToString(buf), err)
	}
	t.Logf("verified loaded lab WinFsp DLL: %s", path)
	address := uintptr(dll.Handle) + uintptr(rva)
	read := func() (uintptr, error) {
		var b [8]byte
		var count uintptr
		err := windows.ReadProcessMemory(windows.CurrentProcess(), address, &b[0], 8, &count)
		if err != nil || count != 8 {
			return 0, fmt.Errorf("read IAT count=%d: %v", count, err)
		}
		return uintptr(binary.LittleEndian.Uint64(b[:])), nil
	}
	original, err := read()
	if err != nil {
		t.Fatal(err)
	}
	api := windows.NewLazySystemDLL("kernel32.dll").NewProc(symbol)
	if err := api.Find(); err != nil {
		t.Fatal(err)
	}
	if original == 0 || original != api.Addr() {
		t.Fatalf("unexpected original IAT target %#x != %#x", original, api.Addr())
	}
	write := func(value uintptr) (err error) {
		var old uint32
		if err = windows.VirtualProtect(address, 8, windows.PAGE_READWRITE, &old); err != nil {
			return err
		}
		defer func() { var ignored uint32; err = errors.Join(err, windows.VirtualProtect(address, 8, old, &ignored)) }()
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(value))
		var count uintptr
		if err = windows.WriteProcessMemory(windows.CurrentProcess(), address, &b[0], 8, &count); err != nil {
			return err
		}
		if count != 8 {
			return fmt.Errorf("short IAT write: %d", count)
		}
		got, err := read()
		if err != nil {
			return err
		}
		if got != value {
			return fmt.Errorf("IAT readback mismatch")
		}
		return nil
	}
	active := true
	restore := func() {
		if !active {
			return
		}
		if err := write(original); err != nil {
			t.Fatalf("restore IAT: %v", err)
		}
		active = false
		t.Logf("restored lab DLL IAT: %s", symbol)
	}
	t.Cleanup(restore)
	if err := write(callback); err != nil {
		t.Fatal(err)
	}
	return original, restore
}
