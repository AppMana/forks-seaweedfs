//go:build linux

package chunk_cache

import (
	"path/filepath"
	"syscall"
	"testing"
)

// A read-cache volume's size limit bounds what the cache may hold; disk blocks
// are allocated only as chunks are written.
func TestChunkCacheVolumeDoesNotReserveItsLimitUpFront(t *testing.T) {
	const limit = 64 << 20
	name := filepath.Join(t.TempDir(), "c0_1")
	v, err := LoadOrCreateChunkCacheVolume(name, limit)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Shutdown()
	var st syscall.Stat_t
	if err := syscall.Stat(name+".dat", &st); err != nil {
		t.Fatal(err)
	}
	if allocated := st.Blocks * 512; allocated >= limit/2 {
		t.Fatalf("empty cache volume occupies %d bytes on disk for a %d byte limit", allocated, limit)
	}
	if v.sizeLimit != limit {
		t.Fatalf("size limit %d, want %d", v.sizeLimit, limit)
	}
}
