package fuse_test

import (
	"context"
	"io"
	mrand "math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// blockShuffledOrder partitions numPages pages into blocks of pagesPerBlock
// (modeling tensors: each read as a contiguous run) and shuffles the BLOCK
// order while keeping pages sequential within each block -- safetensors'
// actual dict-order (not file-offset order) tensor visitation, not a
// page-level shuffle (which is statistically indistinguishable from genuine
// random access and would give this benchmark no meaningful floor to hold).
func blockShuffledOrder(numPages, pagesPerBlock int) []int {
	numBlocks := (numPages + pagesPerBlock - 1) / pagesPerBlock
	blockOrder := make([]int, numBlocks)
	for i := range blockOrder {
		blockOrder[i] = i
	}
	mrand.New(mrand.NewSource(23)).Shuffle(len(blockOrder), func(i, j int) { blockOrder[i], blockOrder[j] = blockOrder[j], blockOrder[i] })

	order := make([]int, 0, numPages)
	for _, b := range blockOrder {
		start := b * pagesPerBlock
		end := start + pagesPerBlock
		if end > numPages {
			end = numPages
		}
		for p := start; p < end; p++ {
			order = append(order, p)
		}
	}
	return order
}

// TestMmapThroughputFloor is the throughput-floor companion to the
// correctness tests in mmap_read_test.go: a pure correctness test with a
// loose timeout can pass even when a regression makes reads catastrophically
// slower (just not slow enough to hit the timeout). This test measures
// wall-clock for a plain sequential read (the "dd" baseline) against a
// concurrent, block-shuffled mmap touch of the same bytes, and asserts the
// mmap path stays within a bounded multiple of the baseline -- the request-
// count-explosion bug this whole fix series addresses shows up here as an
// order-of-magnitude (not small-constant-factor) slowdown.
func TestMmapThroughputFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping performance test in short mode")
	}

	config := mmapConfig()
	framework := NewFuseTestFramework(t, config)
	defer framework.Cleanup()
	require.NoError(t, framework.Setup(config))

	const size = 32 * 1024 * 1024 // spans 16 chunks at ChunkSizeMB=2
	filename := "mmap_throughput.bin"
	writeMmapTestFile(t, framework, filename, size)
	mountPath := filepath.Join(framework.GetMountPoint(), filename)

	// Baseline: plain sequential read, the "dd"-equivalent path.
	baselineStart := time.Now()
	f, err := os.Open(mountPath)
	require.NoError(t, err)
	n, err := io.Copy(io.Discard, f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, int64(size), n)
	baselineDuration := time.Since(baselineStart)
	t.Logf("sequential baseline: %v for %d bytes (%.1f MB/s)", baselineDuration, size, float64(size)/1024/1024/baselineDuration.Seconds())

	// mmap, block-shuffled, concurrent touch of the same bytes.
	mapped, closeFn := mmapOpenAndMap(t, framework, filename, size)
	defer closeFn()

	numPages := size / mmapTestPageSize
	const pagesPerBlock = 32 // ~128KB "tensors"
	order := blockShuffledOrder(numPages, pagesPerBlock)
	const numWorkers = 8

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mmapStart := time.Now()
	var idx int64 // next(order) cursor, guarded by mu below for simplicity
	var mu sync.Mutex
	next := func() (int, bool) {
		mu.Lock()
		defer mu.Unlock()
		if int(idx) >= len(order) {
			return 0, false
		}
		p := order[idx]
		idx++
		return p, true
	}

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for w := 0; w < numWorkers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					page, ok := next()
					if !ok {
						return
					}
					start := page * mmapTestPageSize
					// Touch (read) the page; correctness is covered by
					// mmap_read_test.go, this loop only needs to force the
					// page fault / WFS read path to execute.
					_ = mapped[start]
				}
			}()
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("mmap throughput scenario did not complete within timeout: possible read hang")
	}
	mmapDuration := time.Since(mmapStart)
	t.Logf("mmap block-shuffled concurrent: %v for %d bytes (%.1f MB/s)", mmapDuration, size, float64(size)/1024/1024/mmapDuration.Seconds())

	// The floor: mmap access must not cost more than a bounded multiple of
	// the sequential baseline. This is deliberately generous (not "must be
	// as fast as sequential") -- the goal is to catch a regression back to
	// the request-count-explosion bug (which showed 10x-100x+ slowdowns and
	// full network-round-trip-per-4KB-page behavior), not to enforce
	// sequential-read parity for an inherently different access pattern.
	const maxSlowdownFactor = 8
	maxAllowed := baselineDuration * maxSlowdownFactor
	if mmapDuration > maxAllowed {
		t.Fatalf("mmap block-shuffled access took %v, more than %dx the sequential baseline (%v, floor %v) -- possible regression to the request-count-explosion bug",
			mmapDuration, maxSlowdownFactor, baselineDuration, maxAllowed)
	}
}
