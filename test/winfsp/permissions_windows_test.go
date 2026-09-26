package winfsp

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func setWindowsBasicSecurity(name, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(name, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

// The verify phase runs in a fresh mount/process: cached descriptors cannot
// satisfy this assertion. Restoring access must recover exactly the old bytes.
func TestWindowsPermissionsPersistence(t *testing.T) {
	if *mountPoint == "" || (*phase != "write" && *phase != "verify") {
		t.Skip("requires a mounted write/verify persistence phase")
	}
	name := filepath.Join(*mountPoint, "winfsp-permissions-persist.bin")
	payload := []byte("intact permissions remount payload")
	if *phase == "write" {
		if err := writeAndSync(name, payload); err != nil {
			t.Fatal(err)
		}
		if err := setWindowsBasicSecurity(name, "D:P(A;;SD;;;WD)"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.ReadFile(name); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("%s: persisted read restriction not enforced: %v", *phase, err)
	}
	if f, err := os.OpenFile(name, os.O_WRONLY, 0); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		if f != nil {
			f.Close()
		}
		t.Errorf("%s: persisted write restriction not enforced: %v", *phase, err)
	}
	if *phase == "verify" {
		if err := setWindowsBasicSecurity(name, "D:P(A;;FA;;;WD)"); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(name); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("remount damaged protected payload: %q %v", got, err)
		}
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
}

// Exercise the native security boundary, not os.Chmod's Windows readonly bit.
// The upstream backup/restore tests separately exercise privilege bypass.
func TestWindowsBasicAccessDenial(t *testing.T) {
	name := filepath.Join(testRoot(t), "permissions.bin")
	payload := []byte("permission changes must not damage this data")
	if err := writeAndSync(name, payload); err != nil {
		t.Fatal(err)
	}
	set := func(sddl string) error {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			return err
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			return err
		}
		return windows.SetNamedSecurityInfo(name, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	}
	defer func() {
		if err := set("D:P(A;;FA;;;WD)"); err != nil {
			t.Errorf("restore permissions: %v", err)
		}
	}()
	if err := set("D:P(A;;SD;;;WD)"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(name); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Errorf("read must be denied, got %v", err)
	}
	if f, err := os.OpenFile(name, os.O_WRONLY, 0); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		if f != nil {
			f.Close()
		}
		t.Errorf("write open must be denied, got %v", err)
	}
	if err := set("D:P(A;;FA;;;WD)"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(name); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("restored access must preserve data: %q, %v", got, err)
	}
}

// Retain individual samples for matched-build lab comparisons. No absolute
// wall-clock threshold: guest scheduling differs across hosts. Each timed path
// validates data, so a failed/short read cannot masquerade as a speedup.
func TestWindowsAccessPerformance(t *testing.T) {
	name := filepath.Join(testRoot(t), "access-perf.bin")
	payload := bytes.Repeat([]byte("access-performance"), 256)
	if err := writeAndSync(name, payload); err != nil {
		t.Fatal(err)
	}
	for sample := 0; sample < 5; sample++ {
		start := time.Now()
		for i := 0; i < 256; i++ {
			got, err := os.ReadFile(name)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("open/read: %v", err)
			}
		}
		t.Logf("ACCESS_PERF sample=%d operation=open_read_close iterations=256 ns_per_op=%d", sample, time.Since(start).Nanoseconds()/256)
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(payload))
		start = time.Now()
		for i := 0; i < 4096; i++ {
			n, err := f.ReadAt(buf, 0)
			if err != nil || n != len(buf) || !bytes.Equal(buf, payload) {
				f.Close()
				t.Fatalf("handle read: %d %v", n, err)
			}
		}
		t.Logf("ACCESS_PERF sample=%d operation=handle_read iterations=4096 ns_per_op=%d", sample, time.Since(start).Nanoseconds()/4096)
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
