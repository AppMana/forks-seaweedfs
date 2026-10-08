//go:build linux

package chunk_cache

import (
	"path/filepath"
	"syscall"
	"testing"
)

// A mount's read cache reserved its whole size limit on disk when it was
// created (fallocate of each cache volume), before caching a byte: on
// appmana-031 (2026-10-08) every SeaweedFS mount took ~51 GiB of the root
// disk at mount time, 206 GiB in all while a dataset job filled the rest.
// The limit bounds what the cache may hold; disk is used as data is cached.
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
