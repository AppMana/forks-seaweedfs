package fuse_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	mrand "math/rand"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mmapTestPageSize matches the granularity of a real mmap page fault (the
// unit safetensors' safe_open/get_tensor effectively touches memory in).
const mmapTestPageSize = 4096

// mmapConfig uses a small ChunkSizeMB (matching production's default of 2)
// so a modestly sized test file spans many chunks -- the scenario that
// actually exercises ReaderPattern/ReaderCache chunk-boundary behavior.
func mmapConfig() *TestConfig {
	return &TestConfig{
		Collection:  "",
		Replication: "000",
		ChunkSizeMB: 2,
		CacheSizeMB: 100,
		NumVolumes:  3,
		EnableDebug: false,
		SkipCleanup: false,
	}
}

// writeMmapTestFile creates a file of the given size with deterministic
// pseudo-random content (so touched pages can be verified byte-for-byte)
// and waits for it to be fully visible through the mount before returning.
func writeMmapTestFile(t *testing.T, framework *FuseTestFramework, name string, size int) []byte {
	t.Helper()
	content := make([]byte, size)
	_, err := rand.Read(content)
	require.NoError(t, err)

	mountPath := filepath.Join(framework.GetMountPoint(), name)
	require.NoError(t, os.WriteFile(mountPath, content, 0644))
	waitForFileSize(t, mountPath, int64(size), 30*time.Second)
	waitForFileContent(t, mountPath, content, 30*time.Second)
	return content
}

// mmapOpenAndMap opens name read-only and maps it MAP_SHARED (safetensors'
// actual mode for safe_open/get_tensor), returning the mapping and a closer.
func mmapOpenAndMap(t *testing.T, framework *FuseTestFramework, name string, size int) (data []byte, closeFn func()) {
	t.Helper()
	mountPath := filepath.Join(framework.GetMountPoint(), name)

	f, err := os.OpenFile(mountPath, os.O_RDONLY, 0)
	require.NoError(t, err)

	mapped, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		t.Fatalf("mmap failed: %v", err)
	}

	return mapped, func() {
		syscall.Munmap(mapped)
		f.Close()
	}
}

// sequentialPageOrder returns page indices 0..numPages-1 in increasing order.
func sequentialPageOrder(numPages int) []int {
	order := make([]int, numPages)
	for i := range order {
		order[i] = i
	}
	return order
}

// reversePageOrder returns page indices numPages-1..0 in decreasing order.
func reversePageOrder(numPages int) []int {
	order := make([]int, numPages)
	for i := range order {
		order[i] = numPages - 1 - i
	}
	return order
}

// shuffledPageOrder returns a deterministic pseudo-random permutation of
// page indices, approximating safetensors' dict-order (not file-offset
// order) tensor access.
func shuffledPageOrder(numPages int) []int {
	order := sequentialPageOrder(numPages)
	r := mrand.New(mrand.NewSource(42))
	r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	return order
}

// runMmapTouchScenario maps the file and touches every page in the given
// order (single-threaded), verifying each touched page's bytes against the
// known content. The touch loop runs in a goroutine so a regression that
// hangs on a page fault fails the test cleanly via the context deadline
// instead of hanging the whole CI job.
func runMmapTouchScenario(t *testing.T, framework *FuseTestFramework, filename string, size int, order []int) {
	t.Helper()
	content := writeMmapTestFile(t, framework, filename, size)
	mapped, closeFn := mmapOpenAndMap(t, framework, filename, size)
	defer closeFn()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		for _, page := range order {
			start := page * mmapTestPageSize
			end := start + mmapTestPageSize
			if end > size {
				end = size
			}
			if !bytes.Equal(mapped[start:end], content[start:end]) {
				done <- fmt.Errorf("page %d (offset %d): mmap data mismatch", page, start)
				return
			}
		}
		done <- nil
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatalf("mmap touch scenario did not complete within timeout (%d pages, file %s): possible read hang", len(order), filename)
	}
}

// TestMmapReadSequential touches pages in increasing file-offset order --
// the baseline case that should trivially stay classified as sequential.
func TestMmapReadSequential(t *testing.T) {
	config := mmapConfig()
	framework := NewFuseTestFramework(t, config)
	defer framework.Cleanup()
	require.NoError(t, framework.Setup(config))

	const size = 12 * 1024 * 1024 // spans 6 chunks at ChunkSizeMB=2
	numPages := size / mmapTestPageSize
	runMmapTouchScenario(t, framework, "mmap_sequential.bin", size, sequentialPageOrder(numPages))
}

// TestMmapReadReverse touches pages in decreasing file-offset order -- a
// backward, but still fully deterministic and repeated, access pattern.
func TestMmapReadReverse(t *testing.T) {
	config := mmapConfig()
	framework := NewFuseTestFramework(t, config)
	defer framework.Cleanup()
	require.NoError(t, framework.Setup(config))

	const size = 12 * 1024 * 1024
	numPages := size / mmapTestPageSize
	runMmapTouchScenario(t, framework, "mmap_reverse.bin", size, reversePageOrder(numPages))
}

// TestMmapReadShuffled touches pages in a non-contiguous, dict-order-like
// sequence -- closest to how safetensors actually walks tensors during
// safe_open/get_tensor, and the pattern that originally triggered the false
// "random mode" classification and its resulting request-count explosion.
func TestMmapReadShuffled(t *testing.T) {
	config := mmapConfig()
	framework := NewFuseTestFramework(t, config)
	defer framework.Cleanup()
	require.NoError(t, framework.Setup(config))

	const size = 12 * 1024 * 1024
	numPages := size / mmapTestPageSize
	runMmapTouchScenario(t, framework, "mmap_shuffled.bin", size, shuffledPageOrder(numPages))
}

// TestMmapReadConcurrent touches the mapping from multiple goroutines at
// once -- exercising WFS.Read's shared (not exclusive) per-handle lock,
// which allows genuinely concurrent, out-of-order-completing reads on one
// handle (the second contributing mechanism behind the original bug).
func TestMmapReadConcurrent(t *testing.T) {
	config := mmapConfig()
	framework := NewFuseTestFramework(t, config)
	defer framework.Cleanup()
	require.NoError(t, framework.Setup(config))

	const size = 16 * 1024 * 1024 // spans 8 chunks at ChunkSizeMB=2
	filename := "mmap_concurrent.bin"
	content := writeMmapTestFile(t, framework, filename, size)
	mapped, closeFn := mmapOpenAndMap(t, framework, filename, size)
	defer closeFn()

	numPages := size / mmapTestPageSize
	const numWorkers = 8

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	done := make(chan error, numWorkers)
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			// Each worker walks the whole file forward, offset by a
			// stagger, so multiple goroutines have overlapping in-flight
			// reads on the same handle-adjacent regions concurrently.
			for i := 0; i < numPages; i++ {
				page := (i + workerID) % numPages
				start := page * mmapTestPageSize
				end := start + mmapTestPageSize
				if end > size {
					end = size
				}
				if !bytes.Equal(mapped[start:end], content[start:end]) {
					done <- fmt.Errorf("worker %d page %d (offset %d): mmap data mismatch", workerID, page, start)
					return
				}
			}
		}(w)
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	for {
		select {
		case err, ok := <-done:
			if !ok {
				return
			}
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatalf("concurrent mmap touch scenario did not complete within timeout (%d workers, file %s): possible read hang", numWorkers, filename)
		}
	}
}

// TestMmapReadWithExplicitSequentialMode exercises the -readerCacheMode
// override end-to-end through a real mount: with the mode forced to
// "sequential", a shuffled (non-contiguous) mmap access pattern must still
// complete correctly and promptly, since the override bypasses the
// (already-fixed) inference heuristic entirely.
func TestMmapReadWithExplicitSequentialMode(t *testing.T) {
	config := mmapConfig()
	config.MountOptions = []string{"-readerCacheMode=sequential"}
	framework := NewFuseTestFramework(t, config)
	defer framework.Cleanup()
	require.NoError(t, framework.Setup(config))

	const size = 12 * 1024 * 1024
	numPages := size / mmapTestPageSize
	runMmapTouchScenario(t, framework, "mmap_forced_sequential.bin", size, shuffledPageOrder(numPages))
}
