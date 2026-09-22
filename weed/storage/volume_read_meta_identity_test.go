package storage

import (
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
)

func TestReadNeedleMetaRejectsDifferentNeedleAtCopiedOffset(t *testing.T) {
	dir := t.TempDir()
	v, err := NewVolume(dir, dir, "", 1, NeedleMapInMemory, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	for _, id := range []uint64{1, 2} {
		n := newEmptyNeedle(id)
		n.Data = []byte("identical size, different identity")
		n.Checksum = needle.NewCRC(n.Data)
		if _, _, _, err := v.writeNeedle2(n, true, false, false); err != nil {
			t.Fatal(err)
		}
	}
	value, ok := v.nm.Get(2)
	if !ok {
		t.Fatal("missing fixture needle")
	}
	// A copied index may point at a different same-sized needle after vacuum.
	if err := v.readNeedleMetaAt(newEmptyNeedle(1), value.Offset.ToActualOffset(), int32(value.Size)); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("wrong needle identity accepted: %v", err)
	}
	if err := v.readNeedleMetaAt(newEmptyNeedle(2), value.Offset.ToActualOffset(), int32(value.Size)); err != nil {
		t.Fatal(err)
	}
}
