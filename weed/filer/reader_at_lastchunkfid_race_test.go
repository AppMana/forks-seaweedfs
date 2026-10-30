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

// newMultiChunkFixture builds a ChunkReadAt over numChunks distinct chunks,
// backed by a request-counting server, with a ReaderCache large enough that
// capacity eviction never fires.
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
	chunkViews, err := ViewFromChunks(ctx, lookupFn, chunks, 0, fileSize)
	if err != nil {
		t.Fatal(err)
	}

	readerAt := &ChunkReadAt{
		chunkViews:  chunkViews,
		fileSize:    fileSize,
		readerCache: NewReaderCache(numChunks+4, alwaysMissChunkCache{}, lookupFn, nil).KeepUntilEvicted(),
		// Sequential mode: the eviction-on-transition code this test targets
		// sits on the common path, after the random-mode branch.
		readerPattern: NewReaderPatternWithMode(ReaderCacheModeSequential),
		ctx:           ctx,
	}

	return readerAt, requestCount, server.Close
}

// TestConcurrentChunkTransitionsDoNotEvictEachOther verifies that concurrent
// goroutines reading from distinct chunks at OffsetInChunk == 0 do not evict
// each other's active downloaders.
func TestConcurrentChunkTransitionsDoNotEvictEachOther(t *testing.T) {
	const numChunks = 6
	const chunkSize = 64 * 1024
	const rounds = 40
	const readSize = 4096

	readerAt, requestCount, closeFn := newMultiChunkFixture(t, numChunks, chunkSize)
	defer closeFn()
	defer readerAt.readerCache.destroy()

	// Step every chunk through round r before starting r+1 so each goroutine
	// observes a lastChunkFid written by a different chunk.
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

// TestLastChunkFidConcurrentAccessIsRaceFree exercises concurrent chunk
// transitions across goroutines under -race.
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

// TestChunkTransitionDoesNotEvictPreviousChunk verifies that walking across
// chunks and re-reading chunk 0 serves chunk 0 from the cache without a second
// backend fetch.
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
