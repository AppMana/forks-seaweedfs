package storage

import (
	"bytes"
	"os"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

// Production volume 1191 has a nonempty, incomplete record beyond its last
// indexed record. Model the crash boundary locally, without production data:
// neither startup nor integrity checking may discard that evidence or make
// the volume writable. This is a preservation test, not a recovery algorithm.
func TestVolumeLoadPreservesPartialUnindexedTail(t *testing.T) {
	dir := t.TempDir()
	open := func() *Volume {
		t.Helper()
		v, err := NewVolume(dir, dir, "", 1, NeedleMapInMemory, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	v := open()
	for _, id := range []uint64{1, 2} {
		if _, _, _, err := v.writeNeedle2(newRandomNeedle(id), true, false, false); err != nil {
			v.Close()
			t.Fatal(err)
		}
	}
	datPath, idxPath := v.FileName(".dat"), v.FileName(".idx")
	v.Close()
	v = open()
	if v.noWriteOrDelete {
		v.Close()
		t.Fatal("healthy control unexpectedly reopened read-only")
	}
	v.Close()
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	dat, index := read(datPath), read(idxPath)
	// Lose the final index row and the end of its data record, as can happen
	// when an append is interrupted before its index entry becomes durable.
	if err := os.Truncate(idxPath, int64(len(index)-types.NeedleMapEntrySize)); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(datPath, int64(len(dat)-8)); err != nil {
		t.Fatal(err)
	}
	wantDat, wantIndex := read(datPath), read(idxPath)
	for attempt := 0; attempt < 2; attempt++ {
		v = open()
		if !v.noWriteOrDelete {
			v.Close()
			t.Fatal("partial unindexed tail must prevent writes and deletes")
		}
		if err := v.CompactByIndex(nil); err == nil {
			v.Close()
			t.Fatal("index vacuum accepted a partial unindexed tail; commit would discard it")
		}
		if err := v.CompactByVolumeData(nil); err == nil {
			v.Close()
			t.Fatal("data vacuum accepted a partial unindexed tail")
		}
		v.Close()
		if !bytes.Equal(read(datPath), wantDat) || !bytes.Equal(read(idxPath), wantIndex) {
			t.Fatal("restart changed the original data or index")
		}
	}
}

func TestCompactCommitPreservesNewUnindexedTail(t *testing.T) {
	dir := t.TempDir()
	v, err := NewVolume(dir, dir, "", 1, NeedleMapInMemory, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, _, _, err := v.writeNeedle2(newRandomNeedle(1), true, false, false); err != nil {
		t.Fatal(err)
	}
	// A benignly read-only source is still eligible: do not blanket-ban it.
	v.noWriteOrDelete = true
	if err := v.CompactByIndex(nil); err != nil {
		t.Fatalf("healthy read-only copy: %v", err)
	}
	if err := v.CommitCompact(); err != nil {
		t.Fatalf("healthy read-only commit: %v", err)
	}
	if err := v.CompactByIndex(nil); err != nil {
		t.Fatalf("second copy: %v", err)
	}
	datPath, idxPath := v.FileName(".dat"), v.FileName(".idx")
	datSize, _, err := v.DataBackend.GetStat()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.DataBackend.WriteAt([]byte("interrupted append after compact copy"), datSize); err != nil {
		t.Fatal(err)
	}
	if err := v.DataBackend.Sync(); err != nil {
		t.Fatal(err)
	}
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	wantDat, wantIdx := read(datPath), read(idxPath)
	if err := v.CommitCompact(); err == nil {
		t.Fatal("commit accepted a new unindexed tail")
	}
	if !bytes.Equal(read(datPath), wantDat) || !bytes.Equal(read(idxPath), wantIdx) {
		t.Fatal("failed commit changed original files")
	}
	if _, err := os.Stat(v.FileName(".cpc")); !os.IsNotExist(err) {
		t.Fatalf("failed commit published a recovery marker: %v", err)
	}
}
