package winfsp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

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
	for _, tc := range []struct{ name, target string }{
		{"relative", "target.bin"},
		{"absolute_same_volume", target},
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
}
