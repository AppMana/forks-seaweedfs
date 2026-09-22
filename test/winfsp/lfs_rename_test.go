package winfsp

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Match Git LFS's clean-filter sequence, including close before the Go
// os.Rename call. Do not add sleeps/retries: successful Close/MkdirAll must
// make the subsequent rename valid immediately. Git LFS canonicalizes Windows
// paths, so exercise .GIT against an existing lowercase .git directory too.
func TestGitLfsObjectRename(t *testing.T) {
	base := testRoot(t)
	for index, spelling := range []string{".git", ".GIT"} {
		t.Run(spelling, func(t *testing.T) {
			if spelling == ".GIT" && runtime.GOOS != "windows" {
				t.Skip("uppercase alias requires Windows case-insensitive mount")
			}
			root := filepath.Join(base, fmt.Sprintf("case-%d", index))
			if err := os.MkdirAll(filepath.Join(root, ".git", "lfs", "tmp"), 0755); err != nil {
				t.Fatal(err)
			}
			lfs := filepath.Join(root, spelling, "lfs")
			objects := make(map[string][]byte)
			for iteration := 0; iteration < 256; iteration++ {
				payload := []byte(fmt.Sprintf("object-%d-%s", iteration, bytes.Repeat([]byte("x"), 4096)))
				oid := fmt.Sprintf("%x", sha256.Sum256(payload))
				file, err := os.CreateTemp(filepath.Join(lfs, "tmp"), "")
				if err != nil {
					t.Fatalf("iteration %d create: %v", iteration, err)
				}
				if err := os.Chmod(file.Name(), 0644); err != nil {
					file.Close()
					t.Fatalf("iteration %d chmod: %v", iteration, err)
				}
				_, writeErr := file.Write(payload)
				closeErr := file.Close()
				if writeErr != nil || closeErr != nil {
					t.Fatalf("iteration %d write=%v close=%v", iteration, writeErr, closeErr)
				}
				destination := filepath.Join(lfs, "objects", oid[:2], oid[2:4], oid)
				if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
					t.Fatalf("iteration %d mkdir: %v", iteration, err)
				}
				if _, err := os.Stat(destination); !os.IsNotExist(err) {
					t.Fatalf("iteration %d destination unexpectedly exists: %v", iteration, err)
				}
				if err := os.Rename(file.Name(), destination); err != nil {
					// Observe only after failure; probing beforehand can mask the race.
					sourceInfo, sourceErr := os.Stat(file.Name())
					parentInfo, parentErr := os.Stat(filepath.Dir(destination))
					t.Fatalf("iteration %d rename: %v; source=%v/%v parent=%v/%v", iteration, err, sourceInfo, sourceErr, parentInfo, parentErr)
				}
				objects[destination] = payload
				if _, err := os.Stat(file.Name()); !os.IsNotExist(err) {
					t.Fatalf("iteration %d renamed source remains: %v", iteration, err)
				}
			}
			for path, want := range objects {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("intact object %s changed: read=%v got=%x want=%x", path, err, sha256.Sum256(got), sha256.Sum256(want))
				}
			}
		})
	}
}
