package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle_map"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/syndtr/goleveldb/leveldb"
)

// The append-only index was fsynced, but the derived LevelDB WAL did not
// survive a crash. A diagnostic LOG mtime cannot prove that the WAL did.
func TestLevelDbReplaysDurableIndexDespiteNewerLog(t *testing.T) {
	dir := t.TempDir()
	index, err := os.Create(filepath.Join(dir, "1.idx"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	key, offset, size := types.NeedleId(42), types.ToOffset(1024), types.Size(512)
	if _, err := index.Write(needle_map.ToBytes(key, offset, size)); err != nil {
		t.Fatal(err)
	}
	if err := index.Sync(); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "1.ldb")
	db, err := leveldb.OpenFile(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dbPath, "LOG"), future, future); err != nil {
		t.Fatal(err)
	}
	m, err := NewLevelDbNeedleMap(dbPath, index, nil, 0, needle.GetCurrentVersion())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	got, found := m.Get(key)
	if !found || got.Offset != offset || got.Size != size {
		t.Fatalf("durable index entry disappeared behind newer diagnostic LOG: found=%t value=%v", found, got)
	}
}

func TestLevelDbReplayPreservesRepeatedKeyOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offsets []int64
		deleted bool
	}{
		{"overwrite-back", []int64{2048, 1024}, false},
		{"delete-back", []int64{0, 1024}, false},
		{"delete-final", []int64{2048, 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			index, err := os.Create(filepath.Join(dir, "1.idx"))
			if err != nil {
				t.Fatal(err)
			}
			defer index.Close()
			for _, offset := range tc.offsets {
				size := types.Size(512)
				if offset == 0 {
					size = types.TombstoneFileSize
				}
				if _, err := index.Write(needle_map.ToBytes(42, types.ToOffset(offset), size)); err != nil {
					t.Fatal(err)
				}
			}
			dbPath := filepath.Join(dir, "1.ldb")
			db, err := leveldb.OpenFile(dbPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := levelDbWrite(db, 42, types.ToOffset(1024), 512, false, 0); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			m, err := NewLevelDbNeedleMap(dbPath, index, nil, 0, needle.GetCurrentVersion())
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			got, found := m.Get(42)
			if tc.deleted {
				if found {
					t.Fatalf("deleted key resurrected: %v", got)
				}
			} else if !found || got.Offset != types.ToOffset(1024) || got.Size != 512 {
				t.Fatalf("buffered earlier mutation overrode final record: %v %t", got, found)
			}
		})
	}
}

func TestLevelDbReplaySparseKeysAcrossBatches(t *testing.T) {
	dir := t.TempDir()
	indexPath, dbPath := filepath.Join(dir, "1.idx"), filepath.Join(dir, "1.ldb")
	index, err := os.Create(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := NewLevelDbNeedleMap(dbPath, index, nil, 0, needle.GetCurrentVersion())
	if err != nil {
		t.Fatal(err)
	}
	want := map[types.NeedleId]int64{}
	for i := 0; i < 50; i++ {
		key, offset := types.NeedleId(2*i+1), int64(i+1)*1024
		if err := m.Put(key, types.ToOffset(offset), 512); err != nil {
			t.Fatal(err)
		}
		want[key] = offset
	}
	m.Close()
	index, err = os.OpenFile(indexPath, os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()
	// Multiple bounded batches, wide key gaps, repeated keys and tombstones.
	// The old odd keys are untouched and must not be lost while seeking gaps.
	for i := 0; i < 10007; i++ {
		key, offset := types.NeedleId((i%97)*10000+2), int64(i+51)*1024
		size := types.Size(512)
		want[key] = offset
		if i%7 == 0 {
			size = types.TombstoneFileSize
			want[key] = 0
		}
		if _, err := index.Write(needle_map.ToBytes(key, types.ToOffset(offset), size)); err != nil {
			t.Fatal(err)
		}
	}
	if err := index.Sync(); err != nil {
		t.Fatal(err)
	}
	m, err = NewLevelDbNeedleMap(dbPath, index, nil, 0, needle.GetCurrentVersion())
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for key, offset := range want {
		got, found := m.Get(key)
		if offset == 0 {
			if found {
				t.Errorf("deleted key %d resurrected: %v", key, got)
			}
		} else if !found || got.Offset != types.ToOffset(offset) || got.Size != 512 {
			t.Errorf("key %d: found=%t got=%v want offset=%d", key, found, got, offset)
		}
	}
}
