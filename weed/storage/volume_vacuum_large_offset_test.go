package storage

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/idx"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

// Sparse files exercise the production offset format without allocating the
// apparent file sizes. Replay must replace ALL offset bytes, in both directions.
func TestMakeupDiffRelocatesLargeOffsets(t *testing.T) {
	if types.OffsetSize != 5 {
		t.Skip("production large-volume format requires -tags 5BytesOffset")
	}
	for _, offsets := range [][2]int64{
		{32 << 30, 8}, {128 << 30, 8}, {200 << 30, 8},
		{8, 32 << 30}, {32 << 30, 128 << 30}, {128 << 30, 200 << 30},
	} {
		t.Run(fmt.Sprintf("%d_to_%d", offsets[0], offsets[1]), func(t *testing.T) {
			dir := t.TempDir()
			v, err := NewVolume(dir, dir, "", 1, NeedleMapInMemory, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			seed := newEmptyNeedle(1)
			seed.Data = []byte("seed")
			seed.Checksum = needle.NewCRC(seed.Data)
			if _, _, _, err := v.writeNeedle2(seed, true, false, false); err != nil {
				t.Fatal(err)
			}
			if err := v.CompactByIndex(nil); err != nil {
				t.Fatal(err)
			}
			if offsets[0] > 8 {
				if err := v.DataBackend.Truncate(offsets[0]); err != nil {
					t.Fatal(err)
				}
			}
			n := newEmptyNeedle(2)
			n.Data = []byte("concurrent write survives relocation")
			n.Checksum = needle.NewCRC(n.Data)
			if _, _, _, err := v.writeNeedle2(n, true, false, false); err != nil {
				t.Fatal(err)
			}
			if err := v.nm.Sync(); err != nil {
				t.Fatal(err)
			}
			if offsets[1] > 8 {
				if err := os.Truncate(v.FileName(".cpd"), offsets[1]); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Stat(v.FileName(".cpd"))
			if err != nil {
				t.Fatal(err)
			}
			if err := v.CommitCompact(); err != nil {
				t.Fatal(err)
			}
			indexFile, err := os.Open(v.FileName(".idx"))
			if err != nil {
				t.Fatal(err)
			}
			defer indexFile.Close()
			found := false
			if err := idx.WalkIndexFile(indexFile, 0, func(key types.NeedleId, offset types.Offset, size types.Size) error {
				if key != n.Id {
					return nil
				}
				found = true
				if offset.ToActualOffset() != before.Size() {
					t.Errorf("replayed offset = %d, want %d", offset.ToActualOffset(), before.Size())
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("replayed needle missing from index")
			}
			f, err := os.Open(v.FileName(".dat"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			blob := make([]byte, n.DiskSize(v.Version()))
			if _, err := f.ReadAt(blob, before.Size()); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(blob, n.Data) {
				t.Fatal("replayed payload missing")
			}
			v.Close()
			reloaded, err := NewVolume(dir, dir, "", 1, NeedleMapInMemory, nil, nil, 0, needle.GetCurrentVersion(), 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer reloaded.Close()
			if reloaded.noWriteOrDelete {
				t.Fatal("large-offset volume became read-only on restart")
			}
			for _, want := range []*needle.Needle{seed, n} {
				got := newEmptyNeedle(uint64(want.Id))
				if _, err := reloaded.readNeedle(got, nil, nil); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got.Data, want.Data) {
					t.Fatal("payload changed across restart")
				}
			}
		})
	}
}

func TestCommitCompactReportsReplayFailure(t *testing.T) {
	dir := t.TempDir()
	v, err := NewVolume(dir, dir, "", 1, NeedleMapInMemory, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	n := newEmptyNeedle(1)
	n.Data = []byte("must survive failed compaction")
	n.Checksum = needle.NewCRC(n.Data)
	if _, _, _, err := v.writeNeedle2(n, true, false, false); err != nil {
		t.Fatal(err)
	}
	if err := v.CompactByIndex(nil); err != nil {
		t.Fatal(err)
	}
	n2 := newEmptyNeedle(2)
	n2.Data = []byte("tail")
	n2.Checksum = needle.NewCRC(n2.Data)
	if _, _, _, err := v.writeNeedle2(n2, true, false, false); err != nil {
		t.Fatal(err)
	}
	// Simulate an obsolete compaction generation. Commit must fail visibly,
	// reload the original files, and never publish a commit marker.
	v.lastCompactRevision++
	if err := v.CommitCompact(); err == nil {
		t.Error("replay failure reported success")
	}
	for _, want := range []*needle.Needle{n, n2} {
		got := newEmptyNeedle(uint64(want.Id))
		if _, err := v.readNeedle(got, nil, nil); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Data, want.Data) {
			t.Fatal("original payload changed")
		}
	}
	if _, err := os.Stat(v.FileName(".cpc")); !os.IsNotExist(err) {
		t.Fatalf("unexpected commit marker: %v", err)
	}
}
