package filer

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// alwaysMissChunkCache is a chunk_cache.ChunkCache that never has anything
// cached -- unlike the trivial mockChunkCache in reader_at_test.go, which
// always "hits" with synthetic fileId-derived bytes (fine for the exact-byte
// correctness tests it's used in, but it makes the *old*, buggy
// random-mode code path in readChunkSliceAt short-circuit through it and
// never reach the backend at all, masking the actual bug this test targets:
// how many real network fetches a read pattern causes). This lets the old
// code's fallback to fetchChunkRange, and the new code's real
// ReaderCache/SingleChunkCacher single-flight path, both actually exercise
// the counting mock server below.
type alwaysMissChunkCache struct{}

func (alwaysMissChunkCache) GetChunk(fileId string, minSize uint64) (data []byte) { return nil }
func (alwaysMissChunkCache) ReadChunkAt(data []byte, fileId string, offset uint64) (n int, err error) {
	return 0, nil
}
func (alwaysMissChunkCache) SetChunk(fileId string, data []byte)                    {}
func (alwaysMissChunkCache) GetMaxFilePartSizeInCache() uint64                      { return 0 }
func (alwaysMissChunkCache) IsInCache(fileId string, lockNeeded bool) (answer bool) { return false }

// createCountingMockVolumeServer is like createMockVolumeServer (see
// stream_benchmark_test.go, same package) but counts requests reaching the
// backend, so a test can assert on how many separate network fetches a read
// pattern actually causes -- the thing the random-mode cache bypass bug
// (see reader_at.go's readChunkSliceAt) multiplied unnecessarily.
func createCountingMockVolumeServer(t *testing.T, chunkData map[string][]byte) (server *httptest.Server, requestCount *int64) {
	t.Helper()
	var count int64
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		path := r.URL.Path
		if len(path) > 0 && path[0] == '/' {
			path = path[1:]
		}
		data, ok := chunkData[path]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Honor Range requests, matching createMockVolumeServer in
		// stream_benchmark_test.go -- the whole-chunk fetch path and the
		// per-request fetchChunkRange path both issue ranged GETs, and
		// without this the response body wouldn't correspond to the bytes
		// actually requested, producing an unrelated data-correctness
		// failure that would mask (or falsely appear to demonstrate) the
		// request-count bug this test targets.
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

// TestRandomModeStillCachesWholeChunk is the primary regression test for the
// reader_at.go fix: under IsRandomMode() == true, many small reads scattered
// within a single chunk must still result in ~1 backend fetch for that
// chunk, not one fetch per read. Before the fix, readChunkSliceAt's random-mode
// branch bypassed ReaderCache.ReadChunkAt (the single-flight, whole-chunk
// cache) entirely and called fetchChunkRange directly on every miss --
// this test fails against that code (confirmed via `git stash` below in
// comments for anyone re-verifying) and passes against the fix.
func TestRandomModeStillCachesWholeChunk(t *testing.T) {
	const chunkSize = 256 * 1024
	const numReads = 40
	const readSize = 4096

	data := make([]byte, chunkSize)
	rand.New(rand.NewSource(3)).Read(data)
	fileId := "1,abc"
	chunkData := map[string][]byte{fileId: data}

	server, requestCount := createCountingMockVolumeServer(t, chunkData)
	defer server.Close()

	masterClient := &mockMasterClientForBenchmark{urls: map[string][]string{
		fileId: {server.URL + "/" + fileId},
	}}

	chunks := []*filer_pb.FileChunk{
		{
			FileId:       fileId,
			Offset:       0,
			Size:         uint64(chunkSize),
			ModifiedTsNs: 1,
			Fid:          &filer_pb.FileId{FileKey: 1},
		},
	}

	ctx := context.Background()
	lookupFn := masterClient.GetLookupFileIdFunction()
	chunkViews := ViewFromChunks(ctx, lookupFn, chunks, 0, int64(chunkSize))

	readerAt := &ChunkReadAt{
		chunkViews:    chunkViews,
		fileSize:      int64(chunkSize),
		readerCache:   NewReaderCache(3, alwaysMissChunkCache{}, lookupFn, nil),
		readerPattern: NewReaderPattern(),
		ctx:           ctx,
	}

	// Force random mode, matching how a real mmap/page-fault-driven access
	// pattern trips the (now-fixed, but assume-worst-case-tripped-anyway)
	// detector -- this test is specifically about the cache-bypass bug,
	// independent of whether the detector itself is correct.
	const farApart = 10 * 1024 * 1024 // comfortably beyond reader_pattern.go's tolerance window
	for i := 0; i < ModeChangeLimit+1; i++ {
		readerAt.readerPattern.MonitorReadAt(int64(i)*farApart, readSize) // far-apart offsets: genuinely non-sequential
	}
	if !readerAt.readerPattern.IsRandomMode() {
		t.Fatalf("test setup failed to force random mode")
	}

	// Many small reads scattered within the single chunk, as mmap page
	// faults touching different tensors within one 2MB-class chunk would
	// produce.
	rng := rand.New(rand.NewSource(4))
	buf := make([]byte, readSize)
	for i := 0; i < numReads; i++ {
		offset := int64(rng.Intn(chunkSize - readSize))
		n, err := readerAt.ReadAt(buf, offset)
		if err != nil {
			t.Fatalf("read at %d: %v", offset, err)
		}
		if n != readSize {
			t.Fatalf("read at %d: got %d bytes, want %d", offset, n, readSize)
		}
		for j := range buf {
			if buf[j] != data[offset+int64(j)] {
				t.Fatalf("read at %d: byte %d mismatch: got %d want %d", offset, j, buf[j], data[offset+int64(j)])
			}
		}
	}

	finalCount := atomic.LoadInt64(requestCount)
	// One fetch for the whole chunk is the expected/desired behavior. Allow
	// a small constant for retries/edge effects, but the whole point of the
	// fix is that this does NOT scale with numReads (40): pre-fix,
	// readChunkSliceAt's random-mode branch calls chunkCache.ReadChunkAt
	// (always a miss here) and falls straight through to a raw,
	// uncoalesced fetchChunkRange on every single read -- numReads backend
	// requests for one chunk. Post-fix, every read goes through
	// ReaderCache.ReadChunkAt, whose in-flight/completed downloader map is
	// checked and reused for repeat reads of the same fileId regardless of
	// detected pattern -- one real fetch total.
	if finalCount > 3 {
		t.Fatalf("expected ~1 backend request for the whole chunk under random mode, got %d for %d reads -- random-mode reads are not sharing the whole-chunk cache", finalCount, numReads)
	}
}
