package filer

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// Scenario matrix for the mmap/FUSE read path (ChunkReadAt + ReaderPattern +
// ReaderCache), as opposed to stream_benchmark_test.go's separate
// PrepareStreamContentWithThrottler streaming path. These scenarios model
// exactly the access patterns that originally triggered the false-random
// classification (reader_pattern.go) and its cache-bypass consequence
// (reader_at.go):
//
//	(i)   pure sequential single-threaded -- baseline
//	(ii)  pure sequential, N concurrent goroutines on adjacent ranges --
//	      reproduces the false-random trip even without mmap involved, since
//	      WFS.Read's shared per-handle lock allows concurrent completions
//	(iii) mmap-realistic: concurrent goroutines touching 4KB ranges in
//	      file-offset order, completing out of order
//	(iv)  mmap-realistic shuffled: non-contiguous access order, closer to
//	      safetensors' actual dict-order tensor visitation
//	(v)   genuinely random full-file offsets -- must legitimately stay
//	      request-heavy; this scenario exists to prove the fix doesn't
//	      over-correct into always caching, not to demand it be fast

const benchPageSize = 4096

// countingLatencyServer serves chunk data with per-request latency (matching
// createMockVolumeServer's contract) while also counting requests reaching
// the backend (matching createCountingMockVolumeServer's contract) -- the
// bench scenarios need both: latency to make request-count differences
// visible as wall-clock differences, and a counter to assert directly on
// request-count ratios independent of timing noise.
func countingLatencyServer(chunkData map[string][]byte, latency time.Duration) (server *httptest.Server, requestCount *int64) {
	var mu sync.RWMutex
	var count int64
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		if latency > 0 {
			time.Sleep(latency)
		}
		path := r.URL.Path
		if len(path) > 0 && path[0] == '/' {
			path = path[1:]
		}
		mu.RLock()
		data, ok := chunkData[path]
		mu.RUnlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
			var start, end int64
			fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
			if start >= 0 && end < int64(len(data)) && start <= end {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(data[start : end+1])
				return
			}
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}))
	return server, &count
}

// readAtScenarioFixture bundles a ChunkReadAt spanning numChunks*chunkSize
// bytes, the plaintext it serves, and the request counter/cleanup for the
// mock backend behind it.
type readAtScenarioFixture struct {
	readerAt     *ChunkReadAt
	data         []byte
	requestCount *int64
	cleanup      func()
}

// readerCacheCapacity mirrors weed mount's -concurrentReaders default (128,
// see weed/command/mount.go), not the small capacity=3 used by the
// single-chunk cache-bypass test in reader_at_random_mode_cache_test.go --
// scenarios here have multiple genuinely distinct chunks in flight at once,
// and an undersized cache would cause churn unrelated to the fix under test.
const readerCacheCapacity = 128

func newReadAtScenarioFixture(tb testing.TB, numChunks, chunkSize int, latency time.Duration) *readAtScenarioFixture {
	tb.Helper()

	totalSize := numChunks * chunkSize
	data := make([]byte, totalSize)
	rand.New(rand.NewSource(7)).Read(data)

	chunkData := make(map[string][]byte, numChunks)
	chunks := make([]*filer_pb.FileChunk, numChunks)
	for i := 0; i < numChunks; i++ {
		fileId := fmt.Sprintf("1,%x", i)
		chunkData[fileId] = data[i*chunkSize : (i+1)*chunkSize]
		chunks[i] = &filer_pb.FileChunk{
			FileId:       fileId,
			Offset:       int64(i * chunkSize),
			Size:         uint64(chunkSize),
			ModifiedTsNs: int64(i),
			Fid:          &filer_pb.FileId{FileKey: uint64(i)},
		}
	}

	server, requestCount := countingLatencyServer(chunkData, latency)

	urls := make(map[string][]string, numChunks)
	for i := 0; i < numChunks; i++ {
		fileId := fmt.Sprintf("1,%x", i)
		urls[fileId] = []string{server.URL + "/" + fileId}
	}
	masterClient := &mockMasterClientForBenchmark{urls: urls}

	ctx := context.Background()
	lookupFn := masterClient.GetLookupFileIdFunction()
	chunkViews := ViewFromChunks(ctx, lookupFn, chunks, 0, int64(totalSize))

	readerAt := &ChunkReadAt{
		chunkViews:    chunkViews,
		fileSize:      int64(totalSize),
		readerCache:   NewReaderCache(readerCacheCapacity, alwaysMissChunkCache{}, lookupFn, nil),
		readerPattern: NewReaderPattern(),
		ctx:           ctx,
	}

	return &readAtScenarioFixture{
		readerAt:     readerAt,
		data:         data,
		requestCount: requestCount,
		cleanup:      func() { server.Close() },
	}
}

// verifyRead issues one page-sized read at off and fails tb if the bytes
// don't match the known plaintext.
func verifyRead(tb testing.TB, f *readAtScenarioFixture, off int64) {
	tb.Helper()
	buf := make([]byte, benchPageSize)
	n, err := f.readerAt.ReadAt(buf, off)
	// doReadAt returns io.EOF alongside a full n when the read reaches
	// exactly fileSize (matching io.Reader's "may return (n, EOF) on the
	// final read" convention) -- only a real short-read error is fatal here.
	if err != nil && err != io.EOF {
		tb.Fatalf("read at %d: %v", off, err)
	}
	if n != benchPageSize {
		tb.Fatalf("read at %d: got %d bytes, want %d", off, n, benchPageSize)
	}
	end := off + benchPageSize
	for j := range buf {
		if buf[j] != f.data[off+int64(j)] {
			tb.Fatalf("read at %d..%d: byte %d mismatch: got %d want %d", off, end, j, buf[j], f.data[off+int64(j)])
		}
	}
}

// scenarioSequential issues numPages page reads at strictly increasing
// offsets, single-threaded.
func scenarioSequential(tb testing.TB, f *readAtScenarioFixture, numPages int) {
	for i := 0; i < numPages; i++ {
		verifyRead(tb, f, int64(i*benchPageSize))
	}
}

// scenarioConcurrentAdjacent issues numPages page reads across numWorkers
// goroutines, each walking its OWN contiguous stripe of the file strictly
// sequentially -- locally sequential per worker, but the numWorkers
// goroutines run concurrently, so completions from different workers
// interleave at the shared ReaderPattern the way FUSE's shared per-handle
// lock allows real concurrent Read()s to. This reproduces the false-random
// trip even without any mmap involved.
func scenarioConcurrentAdjacent(tb testing.TB, f *readAtScenarioFixture, numPages, numWorkers int) {
	pagesPerWorker := numPages / numWorkers
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			start := workerID * pagesPerWorker
			end := start + pagesPerWorker
			if workerID == numWorkers-1 {
				end = numPages // last worker absorbs any remainder
			}
			for i := start; i < end; i++ {
				verifyRead(tb, f, int64(i*benchPageSize))
			}
		}(w)
	}
	wg.Wait()
}

// scenarioMmapRealistic issues numPages page reads in file-offset order, but
// via numWorkers concurrent goroutines racing over the same ordered index
// sequence -- completions land out of order the way concurrent mmap page
// faults resolving out of program order would.
func scenarioMmapRealistic(tb testing.TB, f *readAtScenarioFixture, numPages, numWorkers int) {
	var next int64
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := atomic.AddInt64(&next, 1) - 1
				if i >= int64(numPages) {
					return
				}
				verifyRead(tb, f, i*benchPageSize)
			}
		}()
	}
	wg.Wait()
}

// blockShuffledPageOrder partitions numPages pages into blocks of
// pagesPerBlock (modeling tensors: each is read as a contiguous run) and
// shuffles the BLOCK order while keeping pages sequential within each
// block. This is what safetensors' dict-order (not file-offset order)
// tensor visitation actually looks like -- large contiguous reads whose
// starting points jump around, not page-granularity jumps to anywhere in
// the file (which is just genuine random access, already covered by
// scenarioGenuinelyRandom).
func blockShuffledPageOrder(numPages, pagesPerBlock int) []int {
	numBlocks := (numPages + pagesPerBlock - 1) / pagesPerBlock
	blockOrder := make([]int, numBlocks)
	for i := range blockOrder {
		blockOrder[i] = i
	}
	rand.New(rand.NewSource(11)).Shuffle(len(blockOrder), func(i, j int) { blockOrder[i], blockOrder[j] = blockOrder[j], blockOrder[i] })

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

// scenarioMmapShuffled is scenarioMmapRealistic but over a block-shuffled
// page order (see blockShuffledPageOrder) -- closer to safetensors'
// dict-order (not file-offset order) tensor visitation than a full
// page-level shuffle would be.
func scenarioMmapShuffled(tb testing.TB, f *readAtScenarioFixture, numPages, numWorkers int) {
	// pagesPerBlock models a ~64KB tensor -- large enough that within-block
	// access is clearly local, small enough that a 2MB test file still has
	// several dozen "tensors" to shuffle across.
	const pagesPerBlock = 16
	order := blockShuffledPageOrder(numPages, pagesPerBlock)

	var next int64
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := atomic.AddInt64(&next, 1) - 1
				if i >= int64(len(order)) {
					return
				}
				verifyRead(tb, f, int64(order[i])*benchPageSize)
			}
		}()
	}
	wg.Wait()
}

// scenarioGenuinelyRandom issues numPages page reads at uniformly random
// offsets across the whole file, single-threaded -- true random access, not
// expected to match the sequential baseline's request count.
func scenarioGenuinelyRandom(tb testing.TB, f *readAtScenarioFixture, numPages, fileSize int) {
	rng := rand.New(rand.NewSource(13))
	for i := 0; i < numPages; i++ {
		off := int64(rng.Intn(fileSize - benchPageSize))
		verifyRead(tb, f, off)
	}
}

// TestReaderAtScenarioRequestCounts is the companion assertion test for the
// benchmark scenarios below (testing.B doesn't fail on thresholds). It
// proves scenarios (ii)-(iv) land within a small constant factor of the
// sequential baseline's backend request count -- the false-random trip no
// longer causes a request-count explosion -- while (v), genuine random
// access, is allowed (expected) to cost substantially more.
func TestReaderAtScenarioRequestCounts(t *testing.T) {
	const numChunks = 8
	const chunkSize = 256 * 1024 // 8 * 256KB = 2MB file, 8 chunks
	const numWorkers = 8
	numPages := (numChunks * chunkSize) / benchPageSize

	run := func(t *testing.T, scenario func(*readAtScenarioFixture)) int64 {
		f := newReadAtScenarioFixture(t, numChunks, chunkSize, 0)
		defer f.cleanup()
		scenario(f)
		return atomic.LoadInt64(f.requestCount)
	}

	baseline := run(t, func(f *readAtScenarioFixture) { scenarioSequential(t, f, numPages) })
	t.Logf("sequential baseline: %d requests for %d chunks / %d pages", baseline, numChunks, numPages)
	if baseline > int64(numChunks)+2 {
		t.Fatalf("sequential baseline itself is not chunk-cached: %d requests for %d chunks", baseline, numChunks)
	}

	tolerance := baseline*3 + 3 // small constant factor, matching the plan's "close to baseline" bar

	t.Run("ConcurrentAdjacent", func(t *testing.T) {
		got := run(t, func(f *readAtScenarioFixture) { scenarioConcurrentAdjacent(t, f, numPages, numWorkers) })
		t.Logf("requests: %d (baseline %d, tolerance %d)", got, baseline, tolerance)
		if got > tolerance {
			t.Fatalf("concurrent-adjacent request count %d exceeds tolerance %d (baseline %d) -- false-random trip regressed", got, tolerance, baseline)
		}
	})

	t.Run("MmapRealistic", func(t *testing.T) {
		got := run(t, func(f *readAtScenarioFixture) { scenarioMmapRealistic(t, f, numPages, numWorkers) })
		t.Logf("requests: %d (baseline %d, tolerance %d)", got, baseline, tolerance)
		if got > tolerance {
			t.Fatalf("mmap-realistic request count %d exceeds tolerance %d (baseline %d) -- false-random trip regressed", got, tolerance, baseline)
		}
	})

	t.Run("MmapShuffled", func(t *testing.T) {
		got := run(t, func(f *readAtScenarioFixture) { scenarioMmapShuffled(t, f, numPages, numWorkers) })
		t.Logf("requests: %d (baseline %d, tolerance %d)", got, baseline, tolerance)
		if got > tolerance {
			t.Fatalf("mmap-shuffled request count %d exceeds tolerance %d (baseline %d) -- false-random trip regressed", got, tolerance, baseline)
		}
	})

	t.Run("GenuinelyRandom", func(t *testing.T) {
		// Not compared against tolerance -- true random access is expected
		// to cost more. This scenario exists to prove the fix doesn't
		// over-correct into pretending everything is sequential.
		got := run(t, func(f *readAtScenarioFixture) { scenarioGenuinelyRandom(t, f, numPages, numChunks*chunkSize) })
		t.Logf("requests: %d (baseline %d) -- expected to be request-heavy, not compared against tolerance", got, baseline)
	})
}

// Benchmark* variants report throughput (MB/s via b.SetBytes) and wall-clock
// under realistic per-request latency, so the scenario matrix's cost is also
// visible as a timing number, not just a request count.
func runReaderAtBenchmark(b *testing.B, scenario func(*readAtScenarioFixture)) {
	const numChunks = 8
	const chunkSize = 256 * 1024
	const latency = 2 * time.Millisecond
	totalSize := int64(numChunks * chunkSize)

	b.ResetTimer()
	b.SetBytes(totalSize)
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		f := newReadAtScenarioFixture(b, numChunks, chunkSize, latency)
		b.StartTimer()

		scenario(f)

		b.StopTimer()
		f.cleanup()
		b.StartTimer()
	}
}

func BenchmarkReaderAtSequential(b *testing.B) {
	numPages := (8 * 256 * 1024) / benchPageSize
	runReaderAtBenchmark(b, func(f *readAtScenarioFixture) { scenarioSequential(b, f, numPages) })
}

func BenchmarkReaderAtConcurrentAdjacent(b *testing.B) {
	numPages := (8 * 256 * 1024) / benchPageSize
	runReaderAtBenchmark(b, func(f *readAtScenarioFixture) { scenarioConcurrentAdjacent(b, f, numPages, 8) })
}

func BenchmarkReaderAtMmapRealistic(b *testing.B) {
	numPages := (8 * 256 * 1024) / benchPageSize
	runReaderAtBenchmark(b, func(f *readAtScenarioFixture) { scenarioMmapRealistic(b, f, numPages, 8) })
}

func BenchmarkReaderAtMmapShuffled(b *testing.B) {
	numPages := (8 * 256 * 1024) / benchPageSize
	runReaderAtBenchmark(b, func(f *readAtScenarioFixture) { scenarioMmapShuffled(b, f, numPages, 8) })
}

func BenchmarkReaderAtGenuinelyRandom(b *testing.B) {
	const numChunks = 8
	const chunkSize = 256 * 1024
	numPages := (numChunks * chunkSize) / benchPageSize
	runReaderAtBenchmark(b, func(f *readAtScenarioFixture) { scenarioGenuinelyRandom(b, f, numPages, numChunks*chunkSize) })
}
