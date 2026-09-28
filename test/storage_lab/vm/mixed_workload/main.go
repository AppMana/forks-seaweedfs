// mixed_workload is a native client used only on fresh lab mounts.
package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const files = 24

func payload(owner string, index, generation int) []byte {
	sizes := []int{0, 37, 4097, 2*1024*1024 + 103}
	size := sizes[index%len(sizes)]
	if generation == 1 {
		size = 113 + index*19
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", owner, index, generation)))
	b := make([]byte, size)
	for i := range b {
		b[i] = h[i%len(h)]
	}
	return b
}

func name(root, owner string, index int, renamed bool) string {
	suffix := ".bin"
	if renamed {
		suffix = ".renamed"
	}
	return filepath.Join(root, fmt.Sprintf("%s-%02d%s", owner, index, suffix))
}

func write(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = fmt.Errorf("short write: %d/%d", n, len(data))
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func verify(root string, generation int, final bool) error {
	for _, owner := range []string{"linux", "windows"} {
		for i := 0; i < files; i++ {
			path := name(root, owner, i, final)
			if final {
				oldPath := name(root, owner, i, false)
				if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
					return fmt.Errorf("old name survives: %s: %v", oldPath, err)
				}
				if i%2 == 0 {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						return fmt.Errorf("deleted name survives: %s: %v", path, err)
					}
					continue
				}
			}
			got, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			want := payload(owner, i, generation)
			if !bytes.Equal(got, want) {
				return fmt.Errorf("content mismatch %s: got %d bytes sha256=%x, want %d sha256=%x", path, len(got), sha256.Sum256(got), len(want), sha256.Sum256(want))
			}
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	wantCount := 2*files + 1 // .sync is the barrier directory
	if final {
		wantCount = files + 1
	}
	if len(entries) != wantCount {
		return fmt.Errorf("directory inventory: got %d want %d", len(entries), wantCount)
	}
	return nil
}

func phase(root, owner, action string) error {
	peer := "windows"
	if owner == "windows" {
		peer = "linux"
	} else if owner != "linux" {
		return fmt.Errorf("invalid owner %q", owner)
	}
	switch action {
	case "cache-coherence", "cache-coherence-files":
		return cacheCoherence(root, owner, peer, action)
	case "seed", "rewrite", "rename-delete":
		for i := 0; i < files; i++ {
			var err error
			switch action {
			case "seed":
				err = write(name(root, owner, i, false), payload(owner, i, 0))
			case "rewrite":
				err = write(name(root, peer, i, false), payload(peer, i, 1))
			case "rename-delete":
				err = os.Rename(name(root, peer, i, false), name(root, peer, i, true))
				if err == nil && i%2 == 0 {
					err = os.Remove(name(root, peer, i, true))
				}
			}
			if err != nil {
				return fmt.Errorf("%s %s file %d: %w", owner, action, i, err)
			}
		}
		return nil
	case "verify-seed":
		return verify(root, 0, false)
	case "verify-rewrite":
		return verify(root, 1, false)
	case "verify-final", "verify-remount":
		return verify(root, 1, true)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}

// Only the rendezvous retries. Payload, rename, delete and inventory failures
// are never retried into a pass. The barrier also proves a shared namespace.
func barrier(root, owner, action string) error {
	peer := "windows"
	if owner == "windows" {
		peer = "linux"
	}
	if err := write(filepath.Join(root, ".sync", action+"-"+owner), []byte(owner)); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(root, ".sync", action+"-"+peer))
		if err == nil && string(b) == peer {
			return nil
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("shared namespace barrier timed out: %s %s", owner, action)
}

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: mixed_workload ROOT linux|windows PHASE RUN_TOKEN")
		os.Exit(2)
	}
	root, owner, action, token := os.Args[1], os.Args[2], os.Args[3], os.Args[4]
	if err := barrier(root, owner, action); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := phase(root, owner, action); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("MIXED_COMPLETE:%s:%s:%s\n", token, owner, action)
}
