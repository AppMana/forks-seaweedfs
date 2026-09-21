package storage

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
)

// A fixed live dataset must not accumulate historical bytes after successful
// compaction. Exercise overwrites, temporary uploads, commit-window writes,
// and disk reload on both index backends used by the existing storage suite.
func TestVacuumStableLiveDatasetHasBoundedGrowth(t *testing.T) {
	for _, kind := range []NeedleMapKind{NeedleMapInMemory, NeedleMapLevelDb} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			dir := t.TempDir()
			v, err := NewVolume(dir, dir, "", 1, kind, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { v.Close() }()
			for cycle := 0; cycle < 12; cycle++ {
				want := make(map[uint64][]byte)
				write := func(id uint64, value byte) {
					t.Helper()
					n := newEmptyNeedle(id)
					n.Data = bytes.Repeat([]byte{value}, 4096)
					n.Checksum = needle.NewCRC(n.Data)
					if _, _, _, err := v.writeNeedle2(n, true, false); err != nil {
						t.Fatal(err)
					}
					if id <= 16 {
						want[id] = n.Data
					}
				}
				for id := uint64(1); id <= 32; id++ {
					write(id, byte(cycle)+byte(id))
				}
				for id := uint64(17); id <= 32; id++ {
					if _, err := v.deleteNeedle2(newEmptyNeedle(id)); err != nil {
						t.Fatal(err)
					}
				}
				if err := v.CompactByIndex(nil); err != nil {
					t.Fatal(err)
				}
				write(1, byte(200+cycle)) // replay a concurrent overwrite
				if err := v.CommitCompact(); err != nil {
					t.Fatal(err)
				}
				stat, err := os.Stat(v.FileName(".dat"))
				if err != nil {
					t.Fatal(err)
				}
				// 16 live records + one commit-window overwrite + small headers.
				if stat.Size() > 18*4200 {
					t.Fatalf("cycle %d: .dat grew to %d for 64 KiB live data", cycle, stat.Size())
				}
				v.Close()
				v, err = NewVolume(dir, dir, "", 1, kind, nil, nil, 0, needle.GetCurrentVersion(), 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				if v.noWriteOrDelete {
					t.Fatalf("cycle %d: reloaded read-only", cycle)
				}
				for id, data := range want {
					n := newEmptyNeedle(id)
					if _, err := v.readNeedle(n, nil, nil); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(n.Data, data) {
						t.Fatalf("cycle %d: wrong payload for %d", cycle, id)
					}
				}
				for id := uint64(17); id <= 32; id++ {
					if _, err := v.readNeedle(newEmptyNeedle(id), nil, nil); err == nil {
						t.Fatalf("deleted needle %d resurrected", id)
					}
				}
			}
		})
	}
}
