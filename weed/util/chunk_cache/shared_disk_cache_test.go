package chunk_cache

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

func TestSharedTieredChunkCacheCrossMountAndTwoLevelScoping(t *testing.T) {
	sharedDir := t.TempDir()

	// Mount A: per-scope limit = 2 KB, whole-cache limit = 3 KB
	// Mount B: per-scope limit = 2 KB, whole-cache limit = 3 KB
	// maxEntries = 0 so all reads hit the shared on-disk cache layer
	mountA := NewSharedTieredChunkCache(0, sharedDir, "mount-a", 2, 3, 1024)
	defer mountA.Shutdown()
	mountB := NewSharedTieredChunkCache(0, sharedDir, "mount-b", 2, 3, 1024)
	defer mountB.Shutdown()

	var announced []string
	mountA.SetOnCacheChunk(func(fileId string) {
		announced = append(announced, fileId)
	})

	chunk1 := bytes.Repeat([]byte("A"), 900)
	chunk2 := bytes.Repeat([]byte("B"), 900)
	chunk3 := bytes.Repeat([]byte("C"), 900)
	chunk4 := bytes.Repeat([]byte("D"), 900)

	fid1 := "1,0100000001"
	fid2 := "2,0200000002"
	fid3 := "3,0300000003"
	fid4 := "4,0400000004"

	// 1. Mount A caches fid1; Mount B can immediately read fid1 from shared disk cache.
	mountA.SetChunk(fid1, chunk1)
	if len(announced) != 1 || announced[0] != fid1 {
		t.Fatalf("expected onCacheChunk to announce %s, got %v", fid1, announced)
	}

	buf := make([]byte, len(chunk1))
	n, err := mountB.ReadChunkAt(buf, fid1, 0)
	if err != nil || n != len(chunk1) || !bytes.Equal(buf, chunk1) {
		t.Fatalf("expected mountB to read chunk1 cached by mountA (n=%d, err=%v)", n, err)
	}

	// 2. Mount B caches fid2 (900 B). Total in mountB = 900 B, total in sharedDir = ~1800 B.
	time.Sleep(10 * time.Millisecond)
	mountB.SetChunk(fid2, chunk2)

	// 3. Per-scope eviction: Mount A caches fid3 (900 B) and fid4 (900 B).
	// Mount A's scope limit is 2048 B, so adding fid3 + fid4 (2700 B total in Mount A)
	// must evict Mount A's oldest chunk (fid1) while preserving Mount B's chunk (fid2)!
	time.Sleep(10 * time.Millisecond)
	mountA.SetChunk(fid3, chunk3)
	time.Sleep(10 * time.Millisecond)
	mountA.SetChunk(fid4, chunk4)

	if mountA.IsInCache(fid1, true) {
		t.Fatalf("expected Mount A's oldest chunk fid1 to be evicted by Mount A's per-scope limit")
	}
	if !mountB.IsInCache(fid2, true) {
		t.Fatalf("expected Mount B's chunk fid2 to survive Mount A's per-scope eviction")
	}
	if !mountA.IsInCache(fid3, true) || !mountA.IsInCache(fid4, true) {
		t.Fatalf("expected Mount A's newer chunks fid3 and fid4 to remain cached")
	}

	// 4. Whole-cache eviction: Mount B writes another chunk fid5 (900 B).
	// Mount B's own scope usage is ~1800 B (<= 2048 B scope limit),
	// so per-scope eviction does NOT trigger, but whole-cache usage across sharedDir
	// is ~3600 B (> 3072 B whole-cache limit), which evicts the globally oldest chunk (fid2).
	time.Sleep(10 * time.Millisecond)
	fid5 := fmt.Sprintf("5,%02x00000005", 5)
	chunk5 := bytes.Repeat([]byte("E"), 900)
	mountB.SetChunk(fid5, chunk5)

	if mountB.IsInCache(fid2, true) {
		t.Fatalf("expected globally oldest chunk fid2 to be evicted by whole-cache limit")
	}
	if !mountB.IsInCache(fid5, true) {
		t.Fatalf("expected newest chunk fid5 to remain in cache")
	}
}
