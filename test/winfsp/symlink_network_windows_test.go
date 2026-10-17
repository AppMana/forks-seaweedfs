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
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

var (
	networkMountRoots  = flag.Bool("network-mount-roots", false, "-mountpoint is a network-mode mount (WEED_WINFSP_VOLUME_PREFIX) whose -winfspMountRoots names -mountpoint as a container mount path")
	networkOtherVolume = flag.String("network-other-volume", "", "the -dir of a second network-mode mount, under another volume prefix")
)

// storedSymlinkTarget returns the target the filer holds for a link under -mountpoint.
func storedSymlinkTarget(t *testing.T, link string) string {
	t.Helper()
	rel, err := filepath.Rel(*mountPoint, link)
	if err != nil {
		t.Fatal(err)
	}
	want, err := scopedFilerPath(*filerRoot, filepath.ToSlash(rel))
	if err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "http", Host: *filerAddr, Path: strings.TrimSuffix(path.Dir(want), "/") + "/"}
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
	var listing struct {
		Entries []struct {
			FullPath      string
			SymlinkTarget string
		}
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	for _, entry := range listing.Entries {
		if entry.FullPath == want {
			return entry.SymlinkTarget
		}
	}
	t.Fatalf("filer omitted %s", want)
	return ""
}

// A pod reaches the volume through the network (UNC) path at a container
// mount path. A link it makes to an absolute path under that mount path must be
// stored volume-relative, so the container filter can follow it without leaving
// the volume; foreign UNC targets must never become volume paths.
func TestWindowsNetworkSymlinkTargets(t *testing.T) {
	if !*networkMountRoots {
		t.Skip("needs -network-mount-roots: a network-mode mount whose roots name -mountpoint")
	}
	if *filerAddr == "" {
		t.Fatal("network symlink checks require -filer")
	}
	root := testRoot(t)
	payload := []byte("a pod link under its mount path stays inside the volume")
	target := filepath.Join(root, "target.bin")
	if err := writeAndSync(target, payload); err != nil {
		t.Fatal(err)
	}

	t.Run("absolute_under_mount_root", func(t *testing.T) {
		link := filepath.Join(root, "under-root")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(link)
		rel, err := filepath.Rel(*mountPoint, target)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := storedSymlinkTarget(t, link), "/"+filepath.ToSlash(rel); got != want {
			t.Fatalf("stored target %q, want volume-relative %q", got, want)
		}
		if got, err := os.ReadFile(link); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("link read: %q %v", got, err)
		}
	})

	t.Run("absolute_outside_kept", func(t *testing.T) {
		external := filepath.Join(t.TempDir(), "external.bin")
		if err := writeAndSync(external, payload); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "outside")
		if err := os.Symlink(external, link); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(link)
		// Following it from a network volume is the host's symlink
		// evaluation policy (remote-to-local), not the file system's.
		if got, err := os.Readlink(link); err != nil || !strings.EqualFold(got, external) {
			t.Fatalf("outside target must read back as written: %q %v", got, err)
		}
		if got := storedSymlinkTarget(t, link); !strings.EqualFold(got, filepath.ToSlash(external)) {
			t.Fatalf("outside target stored as %q, want it as written", got)
		}
	})

	t.Run("unc_own_volume_in_volume", func(t *testing.T) {
		share, err := os.Readlink(*mountPoint)
		if err != nil || !strings.HasPrefix(share, `\\`) {
			t.Fatalf("network volume share: %q %v", share, err)
		}
		rel, err := filepath.Rel(*mountPoint, target)
		if err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "unc-own")
		if err := os.Symlink(filepath.Join(share, rel), link); err != nil {
			t.Fatalf("a UNC target on this volume must be accepted: %v", err)
		}
		defer os.Remove(link)
		if got, want := storedSymlinkTarget(t, link), "/"+filepath.ToSlash(rel); got != want {
			t.Fatalf("stored target %q, want volume-relative %q", got, want)
		}
		if got, err := os.ReadFile(link); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("link read: %q %v", got, err)
		}
	})

	// Every WinFsp network volume shares one MUP provider, which the signed
	// driver takes as proof that the target is on this volume.
	t.Run("unc_other_volume_rejected", func(t *testing.T) {
		if *networkOtherVolume == "" {
			t.Fatal("needs -network-other-volume")
		}
		share, err := os.Readlink(*networkOtherVolume)
		if err != nil || !strings.HasPrefix(share, `\\`) {
			t.Fatalf("second network volume share: %q %v", share, err)
		}
		other := filepath.Join(share, "other-volume.bin")
		if err := writeAndSync(other, payload); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(other)
		link := filepath.Join(root, "unc-other-volume")
		defer os.Remove(link)
		if err := os.Symlink(other, link); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			if err == nil {
				t.Fatalf("another volume's UNC target accepted and stored as %q", storedSymlinkTarget(t, link))
			}
			t.Fatalf("another volume's UNC target must be denied: %v", err)
		}
	})

	t.Run("unc_outside_rejected", func(t *testing.T) {
		// reachable, as on a node running the SMB server
		if _, err := os.Stat(`\\localhost\C$\Windows\win.ini`); err != nil {
			t.Fatalf("foreign UNC target must be reachable for this case: %v", err)
		}
		link := filepath.Join(root, "unc")
		defer os.Remove(link)
		if err := os.Symlink(`\\localhost\C$\Windows\win.ini`, link); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			if err == nil {
				t.Fatalf("foreign UNC target accepted and stored as %q", storedSymlinkTarget(t, link))
			}
			t.Fatalf("foreign UNC target must be denied: %v", err)
		}
	})
}
