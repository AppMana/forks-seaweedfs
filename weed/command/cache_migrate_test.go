package command

import (
	"bytes"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/util/chunk_cache"
)

func TestCacheMigrateCommandCopiesLegacyCache(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	legacy := chunk_cache.NewTieredChunkCache(0, source, 8, 1<<20)
	const id = "42,0100000002"
	payload := bytes.Repeat([]byte("old cache"), 200000)
	legacy.SetChunk(id, payload)
	legacy.Shutdown()
	for name, value := range map[string]string{"source": source, "target": target, "scope": "test", "cacheCapacityMB": "8", "wholeCacheCapacityMB": "16", "maxCopyMB": "8"} {
		flag := cmdCacheMigrate.Flag.Lookup(name)
		previous := flag.Value.String()
		t.Cleanup(func() { _ = flag.Value.Set(previous) })
		if err := flag.Value.Set(value); err != nil {
			t.Fatal(err)
		}
	}
	if !cmdCacheMigrate.Run(cmdCacheMigrate, nil) {
		t.Fatal("migration command failed")
	}
	cache := chunk_cache.NewSharedTieredChunkCache(0, target, "reader", 8, 16, 1<<20)
	defer cache.Shutdown()
	got := make([]byte, len(payload))
	n, err := cache.ReadChunkAt(got, id, 0)
	if err != nil || n != len(got) || !bytes.Equal(got, payload) {
		t.Fatalf("migrated bytes mismatch: %d %v", n, err)
	}
}
