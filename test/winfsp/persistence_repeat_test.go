package winfsp

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Invoke the real phase functions: verification before reboot must not consume
// the evidence that verification after reboot needs. A local directory models
// verifier ownership, not WinFsp durability or Windows metadata behavior.
func TestPersistenceVerifyPreservesFixtures(t *testing.T) {
	oldMount, oldFiler, oldDir := *mountPoint, *filerAddr, *persistSubdir
	t.Cleanup(func() { *mountPoint, *filerAddr, *persistSubdir = oldMount, oldFiler, oldDir })
	*mountPoint, *filerAddr, *persistSubdir = t.TempDir(), "", "evidence"
	persistenceWrite(t)
	for round := 1; round <= 2; round++ {
		t.Run(fmt.Sprintf("verify_%d", round), persistenceVerify)
		for _, f := range fixtures {
			got, err := os.ReadFile(filepath.Join(*mountPoint, *persistSubdir, filepath.FromSlash(f.relPath)))
			if err != nil || !bytes.Equal(got, contentFor(f.relPath, f.size)) {
				t.Errorf("verify %d changed fixture %s: read=%v bytes=%d", round, f.relPath, err, len(got))
			}
		}
	}
}
