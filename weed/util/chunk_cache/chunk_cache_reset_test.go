package chunk_cache

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"testing"
)

// A mount's cache directory can disappear underneath it, for example when a
// node agent cleans the cache base while the mount is running. The next
// rotation then fails to reset the oldest cache volume. The cache must keep
// serving lookups and writes instead of panicking on the shut-down volume,
// and must cache again once the directory can be recreated.
func TestCacheSurvivesItsDirectoryBeingRemoved(t *testing.T) {
	dir := t.TempDir()
	cache := NewTieredChunkCache(2, dir, 64, 1024)
	defer cache.Shutdown()

	chunk := func(i int) (string, []byte) {
		data := make([]byte, 2000) // layer 1: larger than one unit, at most four
		rand.Read(data)
		return fmt.Sprintf("1,%x%08x", i+1, 0xaabbccdd), data
	}

	var fileIds []string
	for i := 0; i < 8; i++ {
		fileId, data := chunk(i)
		cache.SetChunk(fileId, data)
		fileIds = append(fileIds, fileId)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	var lastId string
	var lastData []byte
	for i := 8; i < 32; i++ {
		lastId, lastData = chunk(i)
		cache.SetChunk(lastId, lastData)
		fileIds = append(fileIds, lastId)
	}

	buffer := make([]byte, 2000)
	for _, fileId := range fileIds {
		cache.IsInCache(fileId, true)
		cache.ReadChunkAt(buffer, fileId, 0)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("rotation did not recreate the cache directory: %v", err)
	}
	n, err := cache.ReadChunkAt(buffer, lastId, 0)
	if err != nil || n != len(lastData) || !bytes.Equal(buffer, lastData) {
		t.Fatalf("cache did not recover after its directory was removed: read %d bytes, err %v", n, err)
	}
}
