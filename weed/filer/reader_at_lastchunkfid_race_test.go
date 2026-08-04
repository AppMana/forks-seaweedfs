package filer

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// Regression tests for the third of the three bugs behind the 2026-07-16
// production mmap hang (fork release post.9). The other two -- the
// false-random detector (reader_pattern.go) and the random-mode chunk-cache
// bypass (reader_at.go) -- are covered by reader_pattern_test.go and
// reader_at_random_mode_cache_test.go respectively. This one had no test.
//
// The bug: readChunkSliceAt tracked the previously-read chunk in a plain
// `lastChunkFid string` field and, on a chunk transition, called
// readerCache.UnCache(lastChunkFid) to evict it. Both halves are wrong under
// concurrency, and WFS.Read holds only a SHARED per-handle lock, so
// readChunkSliceAt genuinely runs in parallel for different chunks (mmap page
// faults resolving on separate goroutines):
//
//   a) the plain string field is a data race (caught by -race);
//   b) lastChunkFid is then "whatever some other goroutine last stored", not
//      "the chunk this stream just finished with", so the UnCache destroyed a
//      DIFFERENT, still-in-use chunk's downloader out from under the reader
//      that owned it -- forcing an endless re-fetch storm.
//
// The fix made the field an atomic.Pointer[string] and deleted the UnCache
// call outright, leaving capacity-based LRU eviction in
// ReaderCache.ReadChunkAt as the only reclamation path.
//
// IMPORTANT for future merges: upstream has never touched reader_at.go (4.36
// is byte-identical to 4.23 there) and still carries both halves of this bug.
// It is pure fork divergence, so a future merge that DOES touch this file can
// silently reintroduce it. These tests are the guard.

// newMultiChunkFixture builds a ChunkReadAt over numChunks distinct chunks,
// backed by a request-counting server, with a ReaderCache large enough that
// capacity eviction never fires -- so any re-fetch observed is caused by an
// explicit eviction, not by the cache running out of room.
func newMultiChunkFixture(t *testing.T, numChunks, chunkSize int) (*ChunkReadAt, *int64, func()) {
	t.Helper()

	chunkData := make(map[string][]byte, numChunks)
	urls := make(map[string][]string, numChunks)
	var chunks []*filer_pb.FileChunk

	rng := rand.New(rand.NewSource(11))
	for i := 0; i < numChunks; i++ {
		fileId := fmt.Sprintf("1,chunk%02d", i)
		data := make([]byte, chunkSize)
		rng.Read(data)
		chunkData[fileId] = data
		chunks = append(chunks, &filer_pb.FileChunk{
			FileId:       fileId,
			Offset:       int64(i * chunkSize),
			Size:         uint64(chunkSize),
			ModifiedTsNs: int64(i + 1),
			Fid:          &filer_pb.FileId{FileKey: uint64(i + 1)},
		})
	}

	server, requestCount := createCountingMockVolumeServer(t, chunkData)
	for fileId := range chunkData {
		urls[fileId] = []string{server.URL + "/" + fileId}
	}
	masterClient := &mockMasterClientForBenchmark{urls: urls}

	ctx := context.Background()
	lookupFn := masterClient.GetLookupFileIdFunction()
	fileSize := int64(numChunks * chunkSize)
	chunkViews := ViewFromChunks(ctx, lookupFn, chunks, 0, fileSize)

	readerAt := &ChunkReadAt{
		chunkViews:  chunkViews,
		fileSize:    fileSize,
		readerCache: NewReaderCache(numChunks+4, alwaysMissChunkCache{}, lookupFn),
		// Sequential mode: the eviction-on-transition code this test targets
		// sits on the common path, after the random-mode branch.
		readerPattern: NewReaderPatternWithMode(ReaderCacheModeSequential),
		ctx:           ctx,
	}

	return readerAt, requestCount, server.Close
}

// TestConcurrentChunkTransitionsDoNotEvictEachOther is the regression test for
// the eviction race. Several goroutines each read repeatedly from their OWN
// chunk, always starting at OffsetInChunk == 0 so every read takes the chunk-
// transition branch. Because each goroutine sees a lastChunkFid written by a
// different goroutine, the pre-fix code called UnCache on a chunk another
// goroutine was actively using, destroying its downloader and forcing a
// re-fetch -- every round, forever.
//
// Post-fix each chunk is downloaded exactly once and served from the
// downloader map thereafter, so the backend request count stays pinned near
// numChunks regardless of how many rounds run.
func TestConcurrentChunkTransitionsDoNotEvictEachOther(t *testing.T) {
	const numChunks = 6
	const chunkSize = 64 * 1024
	const rounds = 40
	const readSize = 4096

	readerAt, requestCount, closeFn := newMultiChunkFixture(t, numChunks, chunkSize)
	defer closeFn()
	defer readerAt.readerCache.destroy()

	// Drive the goroutines in lockstep rounds. Without a barrier the Go
	// scheduler happily runs each goroutine's whole loop back to back, so
	// lastChunkFid stays equal to that goroutine's own chunk and the
	// transition branch never fires -- the test would pass even against the
	// buggy code. Stepping every chunk through round r before anyone starts
	// r+1 guarantees that each goroutine observes a lastChunkFid written by a
	// different chunk, which is precisely the production condition.
	starts := make([]chan struct{}, numChunks)
	for i := range starts {
		starts[i] = make(chan struct{})
	}
	done := make(chan error, numChunks)

	var wg sync.WaitGroup
	for c := 0; c < numChunks; c++ {
		wg.Add(1)
		go func(chunkIndex int) {
			defer wg.Done()
			buf := make([]byte, readSize)
			// Offset 0 within this goroutine's own chunk => OffsetInChunk == 0
			// => the transition branch runs on every single read.
			base := int64(chunkIndex * chunkSize)
			for r := 0; r < rounds; r++ {
				<-starts[chunkIndex]
				_, err := readerAt.ReadAt(buf, base)
				if err != nil {
					err = fmt.Errorf("chunk %d round %d: %w", chunkIndex, r, err)
				}
				done <- err
			}
		}(c)
	}

	for r := 0; r < rounds; r++ {
		for i := range starts {
			starts[i] <- struct{}{}
		}
		for i := 0; i < numChunks; i++ {
			if err := <-done; err != nil {
				t.Fatalf("read failed: %v", err)
			}
		}
	}
	wg.Wait()

	got := atomic.LoadInt64(requestCount)
	// Each chunk should be fetched once. Allow generous slack for genuine
	// single-flight races at startup, but the property under test is that the
	// count does NOT scale with rounds (240 total reads here).
	if got > numChunks*3 {
		t.Fatalf("got %d backend requests for %d chunks over %d total reads; "+
			"concurrent chunk transitions are evicting each other's in-use downloaders "+
			"(expected ~%d, i.e. independent of round count)",
			got, numChunks, numChunks*rounds, numChunks)
	}
}

// TestLastChunkFidConcurrentAccessIsRaceFree drives the same field from many
// goroutines with no assertion beyond "did not race or crash". Its value is
// under `go test -race`, where the pre-fix plain-string field is reported as a
// write/write and read/write data race on ChunkReadAt.lastChunkFid.
func TestLastChunkFidConcurrentAccessIsRaceFree(t *testing.T) {
	const numChunks = 4
	const chunkSize = 32 * 1024
	const rounds = 50

	readerAt, _, closeFn := newMultiChunkFixture(t, numChunks, chunkSize)
	defer closeFn()
	defer readerAt.readerCache.destroy()

	var wg sync.WaitGroup
	for g := 0; g < numChunks*2; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			buf := make([]byte, 1024)
			rng := rand.New(rand.NewSource(int64(id)))
			for r := 0; r < rounds; r++ {
				// Deliberately hop between chunks so the transition branch and
				// the lastChunkFid store are hit constantly from all goroutines.
				chunkIndex := rng.Intn(numChunks)
				if _, err := readerAt.ReadAt(buf, int64(chunkIndex*chunkSize)); err != nil {
					t.Errorf("read: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// The eviction-on-transition call must stay deleted. A single-threaded
// sequential walk across chunks and back re-reads chunk 0 at the end; if the
// transition still evicted the previous chunk, that re-read would cost a
// second backend fetch. This is the single-threaded canary for the same fix --
// it fails fast and deterministically, without depending on goroutine
// interleaving.
func TestChunkTransitionDoesNotEvictPreviousChunk(t *testing.T) {
	const numChunks = 4
	const chunkSize = 32 * 1024
	const readSize = 4096

	readerAt, requestCount, closeFn := newMultiChunkFixture(t, numChunks, chunkSize)
	defer closeFn()
	defer readerAt.readerCache.destroy()

	buf := make([]byte, readSize)
	// Walk forward across every chunk, always at OffsetInChunk == 0.
	for c := 0; c < numChunks; c++ {
		if _, err := readerAt.ReadAt(buf, int64(c*chunkSize)); err != nil {
			t.Fatalf("forward read chunk %d: %v", c, err)
		}
	}
	afterForward := atomic.LoadInt64(requestCount)
	if afterForward > numChunks*2 {
		t.Fatalf("forward walk already cost %d requests for %d chunks", afterForward, numChunks)
	}

	// Walk backward over the same chunks. Every one of them was just fetched,
	// so a correct implementation serves all of these from the downloader map
	// and issues zero new backend requests.
	for c := numChunks - 1; c >= 0; c-- {
		if _, err := readerAt.ReadAt(buf, int64(c*chunkSize)); err != nil {
			t.Fatalf("backward read chunk %d: %v", c, err)
		}
	}

	got := atomic.LoadInt64(requestCount)
	if got > afterForward {
		t.Fatalf("re-reading already-fetched chunks cost %d extra backend requests "+
			"(%d -> %d): the chunk transition is evicting the previous chunk's downloader",
			got-afterForward, afterForward, got)
	}
}
