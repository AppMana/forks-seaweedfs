package winfsp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const crashSiblingContent = "unrelated data must survive mount cleanup"

func validateCrashOwnership(root, ready, token string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !filepath.IsAbs(ready) || filepath.Dir(ready) != root || !strings.HasPrefix(filepath.Base(ready), "ready-") {
		return fmt.Errorf("crash paths are not confined to an absolute owned directory")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("crash root is not a plain directory: %v", err)
	}
	owner, err := os.ReadFile(filepath.Join(root, ".crash-owner"))
	if err != nil || len(token) != 64 || string(owner) != token {
		return fmt.Errorf("missing or mismatched crash ownership token")
	}
	data, err := os.ReadFile(filepath.Join(root, "unrelated-data.txt"))
	if err != nil || string(data) != crashSiblingContent {
		return fmt.Errorf("parent-owned sibling not intact: %v", err)
	}
	for _, path := range []string{ready, ready + ".tmp", filepath.Join(root, "reused-mount")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return fmt.Errorf("child target already exists or cannot be checked: %s: %v", path, err)
		}
	}
	return nil
}

func TestCrashChildOwnership(t *testing.T) {
	root := t.TempDir()
	token := strings.Repeat("a", 64)
	for name, content := range map[string]string{".crash-owner": token, "unrelated-data.txt": crashSiblingContent} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ready := filepath.Join(root, "ready-0")
	for _, tc := range []struct {
		name, root, ready, token string
		valid                    bool
	}{
		{"owned", root, ready, token, true},
		{"missing token", root, ready, "", false},
		{"wrong token", root, ready, strings.Repeat("b", 64), false},
		{"outside checkpoint", root, filepath.Join(filepath.Dir(root), "ready-0"), token, false},
		{"relative root", "relative", "relative/ready-0", token, false},
		{"unowned root", t.TempDir(), ready, token, false},
		{"reserved checkpoint", root, filepath.Join(root, "unrelated-data.txt"), token, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateCrashOwnership(tc.root, tc.ready, tc.token); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "unrelated-data.txt"), []byte("intact user data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateCrashOwnership(root, ready, token); err == nil {
		t.Error("accepted mismatched sibling")
	}
	data, err := os.ReadFile(filepath.Join(root, "unrelated-data.txt"))
	if err != nil || string(data) != "intact user data" {
		t.Fatalf("validation modified sibling: %v", err)
	}
}
