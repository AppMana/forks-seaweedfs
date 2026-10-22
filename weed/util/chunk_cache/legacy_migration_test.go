package chunk_cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyCacheMigrationPreservesSourceAndSurvivesRestart(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	old := NewTieredChunkCache(0, source, 64, 1024)
	const id = "42,0100000002"
	payload := bytes.Repeat([]byte("legacy payload"), 100)
	old.SetChunk(id, payload)
	next := NewSharedTieredChunkCache(0, target, "migration", 64, 128, 1024)
	if n, err := next.MigrateLegacyCache(context.Background(), source, 64*1024); err == nil || n != 0 {
		t.Fatalf("active old cache accepted: %d %v", n, err)
	}
	old.Shutdown()
	digests := map[string][32]byte{}
	for _, ext := range []string{"*.dat", "*.idx"} {
		paths, err := filepath.Glob(filepath.Join(source, ext))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			digests[path] = sha256.Sum256(data)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		n, err := next.MigrateLegacyCache(context.Background(), source, 64*1024)
		if err != nil || n != int64(len(payload)+cacheHeaderSize) {
			t.Fatalf("migration: %d %v", n, err)
		}
	}
	next.Shutdown()
	reopened := NewSharedTieredChunkCache(0, target, "other-modality", 64, 128, 1024)
	defer reopened.Shutdown()
	got := make([]byte, len(payload))
	n, err := reopened.ReadChunkAt(got, id, 0)
	if err != nil || n != len(payload) || !bytes.Equal(got, payload) {
		t.Fatalf("migrated cache miss after restart: %d %v", n, err)
	}
	for path, digest := range digests {
		data, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(data) != digest {
			t.Errorf("source changed: %s: %v", path, err)
		}
	}
}

func TestLegacyCacheMigrationRejectsBudgetAndCancellation(t *testing.T) {
	source := t.TempDir()
	old := NewTieredChunkCache(0, source, 64, 1024)
	old.SetChunk("42,0100000002", bytes.Repeat([]byte("x"), 1300))
	old.Shutdown()
	next := NewSharedTieredChunkCache(0, t.TempDir(), "migration", 64, 128, 1024)
	defer next.Shutdown()
	if n, err := next.MigrateLegacyCache(context.Background(), source, 10); err == nil || n != 0 {
		t.Fatalf("budget ignored: %d %v", n, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := next.MigrateLegacyCache(ctx, source, 64*1024); err == nil || n != 0 {
		t.Fatalf("cancellation ignored: %d %v", n, err)
	}
}
