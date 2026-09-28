package winfsp

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Query through an open handle; never rewrite the mount junction to make a
// failing pathname work. The literal directory-mounted case remains separate.
func symlinkGUIDTarget(t *testing.T, name string) (string, string) {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &buf[0], uint32(len(buf)), 1)
	if err != nil || n == 0 || n >= uint32(len(buf)) {
		t.Fatalf("query canonical symlink target: n=%d err=%v", n, err)
	}
	canonical := windows.UTF16ToString(buf[:n])
	volume, _, ok := strings.Cut(canonical, `}\`)
	if !ok || !strings.HasPrefix(canonical, `\\?\Volume{`) {
		t.Fatalf("expected canonical volume GUID path, got %q", canonical)
	}
	return canonical, volume + "}"
}

func symlinkHandleIdentity(t *testing.T, name, canonicalVolume string) symlinkFilesystemIdentity {
	t.Helper()
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var serial, componentLength, flags uint32
	var filesystem [256]uint16
	if err := windows.GetVolumeInformationByHandle(windows.Handle(f.Fd()), nil, 0,
		&serial, &componentLength, &flags, &filesystem[0], uint32(len(filesystem))); err != nil {
		t.Fatalf("query filesystem identity of %q: %v", name, err)
	}
	identity := symlinkFilesystemIdentity{canonicalVolume, windows.UTF16ToString(filesystem[:]), serial}
	t.Logf("filesystem identity %q: GUID=%q filesystem=%q serial=%08x", name,
		identity.canonicalVolume, identity.filesystem, identity.serial)
	return identity
}

// Distinguish a genuinely relative sibling target from a same-volume absolute
// target. Upstream's "relative" test starts with a volume-absolute target and
// aborts before reaching its nested relative-link cases when rellinks is off.
func TestWindowsSymlinkTargets(t *testing.T) {
	root := testRoot(t)
	target := filepath.Join(root, "target.bin")
	payload := []byte("symlink resolution must preserve the original payload")
	if err := writeAndSync(target, payload); err != nil {
		t.Fatal(err)
	}
	canonical, volume := symlinkGUIDTarget(t, target)
	nested := filepath.Join(root, "parent", "child", "target.bin")
	if err := os.MkdirAll(filepath.Dir(nested), 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeAndSync(nested, payload); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, target string }{
		{"relative", "target.bin"},
		{"absolute_same_volume", target},
		{"absolute_volume_guid", canonical},
		{"absolute_nested", nested},
		{"absolute_casefold", strings.ToLower(target)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link := filepath.Join(root, "link-"+tc.name)
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(link)
			if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("not a symlink: %v %v", info, err)
			}
			if got, err := os.ReadFile(link); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("link target read: %q %v", got, err)
			}
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("removing link damaged target: %q %v", got, err)
			}
		})
	}
	t.Run("absolute_dangling", func(t *testing.T) {
		missing := filepath.Join(root, "not-yet", "target.bin")
		link := filepath.Join(root, "dangling")
		if err := os.Symlink(missing, link); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(link)
		if _, err := os.ReadFile(link); !os.IsNotExist(err) {
			t.Fatalf("dangling target must be absent: %v", err)
		}
		if err := os.Mkdir(filepath.Dir(missing), 0755); err != nil {
			t.Fatal(err)
		}
		if err := writeAndSync(missing, payload); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(link); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("created dangling target: %q %v", got, err)
		}
	})
	t.Run("cross_volume_rejected", func(t *testing.T) {
		external := filepath.Join(t.TempDir(), "external.bin")
		if err := writeAndSync(external, payload); err != nil {
			t.Fatal(err)
		}
		_, externalVolume := symlinkGUIDTarget(t, external)
		mountedIdentity := symlinkHandleIdentity(t, target, volume)
		externalIdentity := symlinkHandleIdentity(t, external, externalVolume)
		if !distinctSymlinkFilesystems(mountedIdentity, externalIdentity) {
			t.Fatalf("cross-volume fixture is not proven external: mounted=%+v external=%+v; provide a temporary directory on a distinguishable filesystem", mountedIdentity, externalIdentity)
		}
		link := filepath.Join(root, "cross-volume")
		defer os.Remove(link)
		if err := os.Symlink(external, link); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("cross-volume target must remain denied: %v", err)
		}
		if got, err := os.ReadFile(external); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("rejected symlink damaged external target: %q %v", got, err)
		}
	})
}
