package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
)

// The crash window AFTER both renames but BEFORE cache invalidation still has
// a marker, but no .cpd/.cpx. Old cache timestamps do not prove it is current.
func TestReconcileAfterBothRenamesInvalidatesOldLevelDB(t *testing.T) {
	dir := t.TempDir()
	v, err := NewVolume(dir, dir, "", 1, NeedleMapLevelDb, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.Version3, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { v.Close() }()
	// Index replay starts at the cache's watermark, which advances every
	// watermarkBatchSize entries. Cross it, then delete the first needle: the
	// compacted .idx holds exactly watermarkBatchSize entries, every one at a
	// shifted offset, so replay rewrites none of them and only invalidating
	// the cache stops it pointing every needle at its pre-compaction offset.
	last := uint64(watermarkBatchSize + 1)
	var want []byte
	for id := uint64(1); id <= last; id++ {
		n := newEmptyNeedle(id)
		n.Data = bytes.Repeat([]byte{byte(id)}, 64)
		n.Checksum = needle.NewCRC(n.Data)
		if _, _, _, err := v.writeNeedle2(n, true, true, false); err != nil {
			t.Fatal(err)
		}
		want = n.Data
	}
	if _, err := v.deleteNeedle2(newEmptyNeedle(1)); err != nil {
		t.Fatal(err)
	}
	if err := v.CompactByIndex(nil); err != nil {
		t.Fatal(err)
	}
	v.Close()
	for _, pair := range [][2]string{{".cpd", ".dat"}, {".cpx", ".idx"}} {
		// Windows cannot rename over an existing file. Both are disposable
		// fixture files; reproduce the completed-rename state explicitly.
		if err := os.Remove(v.FileName(pair[1])); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(v.FileName(pair[0]), v.FileName(pair[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(v.FileName(".cpc"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	// Closing LevelDB can update LOG after the candidate index was created.
	// Make that ordering deterministic rather than relying on timer resolution.
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(v.FileName(".ldb"), "LOG"), future, future); err != nil {
		t.Fatal(err)
	}
	if err := v.reconcileCompactState(); err != nil {
		t.Fatal(err)
	}
	v, err = NewVolume(dir, dir, "", 1, NeedleMapLevelDb, nil, nil, 0, needle.Version3, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := newEmptyNeedle(last)
	if _, err := v.readNeedle(got, nil, nil); err != nil || !bytes.Equal(got.Data, want) {
		t.Fatalf("recovered volume reused stale cache: read=%v, payload bytes=%d", err, len(got.Data))
	}
	mustNotExist(t, v.FileName(".cpc"))
}

func TestReconcileCacheInvalidationFailureRetainsMarker(t *testing.T) {
	dir := t.TempDir()
	v := &Volume{dir: dir, dirIdx: dir, Id: 1}
	for _, ext := range []string{".dat", ".idx", ".cpc"} {
		if err := os.WriteFile(v.FileName(ext), []byte("preserve"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// A nonempty directory at the cache-file path makes removal fail even
	// when the test runs with privileged filesystem access.
	if err := os.Mkdir(v.FileName(".rdb"), 0755); err != nil {
		t.Fatal(err)
	}
	obstacle := filepath.Join(v.FileName(".rdb"), "obstacle")
	if err := os.WriteFile(obstacle, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := v.reconcileCompactState(); err == nil {
		t.Fatal("recovery must report failed cache invalidation")
	}
	for _, ext := range []string{".dat", ".idx", ".cpc"} {
		got, err := os.ReadFile(v.FileName(ext))
		if err != nil || string(got) != "preserve" {
			t.Fatalf("failed recovery changed %s: %q, %v", ext, got, err)
		}
	}
	if err := os.Remove(obstacle); err != nil {
		t.Fatal(err)
	}
	if err := v.reconcileCompactState(); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, v.FileName(".cpc"))
}
