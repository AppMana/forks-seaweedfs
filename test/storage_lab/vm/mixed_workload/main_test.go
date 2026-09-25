package main

import (
	"os"
	"path/filepath"
	"testing"
)

func seeded(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".sync"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"linux", "windows"} {
		if err := phase(root, owner, "seed"); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestMixedLifecycle(t *testing.T) {
	root := seeded(t)
	for _, action := range []string{"verify-seed", "rewrite", "verify-rewrite", "rename-delete", "verify-final", "verify-remount"} {
		for _, owner := range []string{"linux", "windows"} {
			if err := phase(root, owner, action); err != nil {
				t.Fatalf("%s %s: %v", owner, action, err)
			}
		}
	}
}

func TestMixedOpenDescriptorCoherence(t *testing.T) {
	root := seeded(t)
	errs := make(chan error, 2)
	for _, owner := range []string{"linux", "windows"} {
		go func(owner string) { errs <- phase(root, owner, "cache-coherence-files") }(owner)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

func TestMixedOracleRejectsDamage(t *testing.T) {
	for _, damage := range []string{"corrupt", "truncate", "missing", "unexpected"} {
		t.Run(damage, func(t *testing.T) {
			root := seeded(t)
			path := name(root, "windows", 3, false)
			var err error
			switch damage {
			case "corrupt":
				err = os.WriteFile(path, make([]byte, len(payload("windows", 3, 0))), 0600)
			case "truncate":
				err = os.Truncate(path, 4)
			case "missing":
				err = os.Remove(path)
			case "unexpected":
				err = os.WriteFile(filepath.Join(root, "unexpected"), nil, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := verify(root, 0, false); err == nil {
				t.Fatal("damaged fixture accepted")
			}
		})
	}
}

func TestMixedOracleRejectsLostDelete(t *testing.T) {
	root := seeded(t)
	for _, action := range []string{"rewrite", "rename-delete"} {
		for _, owner := range []string{"linux", "windows"} {
			if err := phase(root, owner, action); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := write(name(root, "linux", 0, true), payload("linux", 0, 1)); err != nil {
		t.Fatal(err)
	}
	if err := verify(root, 1, true); err == nil {
		t.Fatal("lost delete accepted")
	}
}
