package winfsp

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var checkBasicPermissions = flag.Bool("check-basic-permissions", false, "assert persisted SYSTEM default identity via -filer (basic-permissions lab only)")

func assertStoredWindowsPermissions(t *testing.T, name string, uid, gid, mode uint32) {
	t.Helper()
	if !*checkBasicPermissions {
		return
	}
	if *filerAddr == "" {
		t.Fatal("basic permission metadata checks require -filer")
	}
	rel, err := filepath.Rel(*mountPoint, name)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "/" + filepath.ToSlash(rel)
	u := url.URL{Scheme: "http", Host: *filerAddr, Path: strings.TrimSuffix(path.Dir(wantPath), "/") + "/"}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("filer listing status: %s", response.Status)
	}
	var listing struct {
		Entries []struct {
			FullPath       string
			Uid, Gid, Mode uint32
		}
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	for _, entry := range listing.Entries {
		if entry.FullPath == wantPath {
			if entry.Uid != uid || entry.Gid != gid || entry.Mode&0777 != mode {
				t.Fatalf("persisted %s: uid=%d gid=%d mode=%#o; want %d/%d/%#o", wantPath, entry.Uid, entry.Gid, entry.Mode&0777, uid, gid, mode)
			}
			return
		}
	}
	t.Fatalf("filer omitted %s", wantPath)
}

// Keep explicit create descriptors separate from descriptor-less nested
// creates. A fix for a token's default DACL must not relax an application's
// deliberately supplied restriction (including at initial creation).
func TestWindowsCreateSecurity(t *testing.T) {
	if *mountPoint == "" {
		t.Skip("requires a mounted filesystem")
	}
	t.Run("default_nested", func(t *testing.T) {
		root := testRoot(t)
		dir := filepath.Join(root, "nested")
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(dir, "payload.bin")
		payload := []byte("descriptor-less create must reopen intact")
		if err := writeAndSync(name, payload); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(name); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("default create/reopen: %q %v", got, err)
		}
		assertStoredWindowsPermissions(t, dir, 18, 544, 0750)
		assertStoredWindowsPermissions(t, name, 18, 544, 0750)
	})
	for _, tc := range []struct {
		name      string
		sddl      string
		deny      bool
		canonical bool
	}{
		{"explicit_delete_only", "D:P(A;;SD;;;WD)", true, false},
		{"explicit_full_access", "D:P(A;;FA;;;WD)", false, false},
		{"explicit_system_default", "O:BAG:SYD:(A;;GA;;;SY)(A;;RCGXGR;;;BA)", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Use a unique leaf directly under the supplied root, so broken
			// default directory creation cannot mask this independent check.
			name := filepath.Join(*mountPoint, "winfsp-create-security-"+tc.name+".bin")
			p, err := windows.UTF16PtrFromString(name)
			if err != nil {
				t.Fatal(err)
			}
			sd, err := windows.SecurityDescriptorFromString(tc.sddl)
			if err != nil {
				t.Fatal(err)
			}
			sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
			// A newly created handle has the requested access; the new DACL
			// governs subsequent opens, as in upstream create_backup_test.
			h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
				&sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
			runtime.KeepAlive(sd)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := setWindowsBasicSecurity(name, "D:P(A;;FA;;;WD)"); err != nil {
					t.Errorf("restore explicit create: %v", err)
					return
				}
				if err := os.Remove(name); err != nil {
					t.Errorf("remove explicit create: %v", err)
				}
			})
			f := os.NewFile(uintptr(h), name)
			payload := []byte("explicit create restriction preserves intact bytes")
			if n, err := f.Write(payload); err != nil || n != len(payload) {
				f.Close()
				t.Fatalf("initial handle write: %d %v", n, err)
			}
			if err := f.Sync(); err != nil {
				f.Close()
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if tc.canonical {
				// The same DACL explicitly supplied must not be normalized.
				// Basic POSIX mapping cannot faithfully represent overlapping
				// group rights; this is not an arbitrary-ACL support claim.
				assertStoredWindowsPermissions(t, name, 544, 18, 0570)
				if err := setWindowsBasicSecurity(name, "D:P(A;;FA;;;WD)"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.deny {
				if _, err := os.ReadFile(name); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
					t.Errorf("explicit create read restriction lost: %v", err)
				}
				if f, err := os.OpenFile(name, os.O_WRONLY, 0); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
					if f != nil {
						f.Close()
					}
					t.Errorf("explicit create write restriction lost: %v", err)
				}
				if err := setWindowsBasicSecurity(name, "D:P(A;;FA;;;WD)"); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := os.ReadFile(name); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("explicit create payload: %q %v", got, err)
			}
		})
	}
}

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
