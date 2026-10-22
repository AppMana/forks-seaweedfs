package chunk_cache

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
)

func TestSharedCacheDoesNotEvictLegacyOrForeignFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"legacy/c0_2_0.dat", "00/unrelated.dat"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.Repeat([]byte("L"), 4096), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c := NewSharedTieredChunkCache(0, dir, "test", 1, 1, 1024)
	defer c.Shutdown()
	c.SetChunk("1,0100000001", []byte("managed"))
	for _, name := range []string{"legacy/c0_2_0.dat", "00/unrelated.dat"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, bytes.Repeat([]byte("L"), 4096)) {
			t.Errorf("foreign file changed: %s: %v", name, err)
		}
	}
}

func TestSharedCacheRepairsSameSizeInvalidEntry(t *testing.T) {
	c := NewSharedTieredChunkCache(0, t.TempDir(), "test", 4, 8, 1024)
	defer c.Shutdown()
	const id = "1,0100000001"
	data := bytes.Repeat([]byte("valid payload"), 100)
	c.SetChunk(id, data)
	fid, err := needle.ParseFileIdFromString(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.sharedDisk.chunkFullPath(fid), make([]byte, len(data)+cacheHeaderSize), 0600); err != nil {
		t.Fatal(err)
	}
	c.SetChunk(id, data)
	got := make([]byte, len(data))
	n, err := c.ReadChunkAt(got, id, 0)
	if err != nil || n != len(data) || !bytes.Equal(got, data) {
		t.Fatalf("invalid same-size cache entry reused: n=%d err=%v data=%q", n, err, got)
	}
}

func TestSharedCacheRejectsOverflowingReadOffset(t *testing.T) {
	c := NewSharedTieredChunkCache(0, t.TempDir(), "test", 4, 8, 1024)
	defer c.Shutdown()
	c.SetChunk("1,0100000001", []byte("payload"))
	n, err := c.ReadChunkAt(make([]byte, 10), "1,0100000001", ^uint64(0))
	if n != 0 || err != ErrorOutOfBounds {
		t.Fatalf("overflowing offset: n=%d err=%v", n, err)
	}
}
