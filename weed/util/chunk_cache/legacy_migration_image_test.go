package chunk_cache

import (
	"bytes"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Opt-in: exercise a published binary, not just the library or command handler.
func TestPublishedImageMigratesLegacyCache(t *testing.T) {
	image := os.Getenv("SEAWEEDFS_CACHE_MIGRATION_IMAGE")
	if image == "" {
		t.Skip("requires an explicit published candidate image")
	}
	if !strings.Contains(image, "@sha256:") {
		t.Fatal("digest-pinned image required")
	}
	source, target := t.TempDir(), t.TempDir()
	old := NewTieredChunkCache(0, source, 8, 1<<20)
	const id = "42,0100000002"
	payload := bytes.Repeat([]byte("legacy-image-payload"), 100000)
	old.SetChunk(id, payload)
	old.Shutdown()
	digests := map[string][32]byte{}
	for _, suffix := range []string{"*.dat", "*.idx"} {
		paths, err := filepath.Glob(filepath.Join(source, suffix))
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
		cmd := exec.Command("docker", "run", "--rm", "--network", "none",
			"--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()),
			"--mount", "type=bind,src="+source+",dst=/legacy",
			"--mount", "type=bind,src="+target+",dst=/managed",
			"--entrypoint", "/usr/bin/weed", image,
			"cache.migrate", "-source=/legacy", "-target=/managed", "-scope=image-test",
			"-cacheCapacityMB=8", "-wholeCacheCapacityMB=16", "-maxCopyMB=8", "-timeout=30s")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("published migration failed: %v: %s", err, out)
		}
	}
	cache := NewSharedTieredChunkCache(0, target, "other-process", 8, 16, 1<<20)
	defer cache.Shutdown()
	got := make([]byte, len(payload))
	n, err := cache.ReadChunkAt(got, id, 0)
	if err != nil || n != len(got) || !bytes.Equal(got, payload) {
		t.Fatalf("published migration changed payload: %d %v", n, err)
	}
	for path, digest := range digests {
		data, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(data) != digest {
			t.Errorf("published migration changed source %s: %v", path, err)
		}
	}
}
