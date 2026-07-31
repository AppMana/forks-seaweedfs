package filer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

const (
	randomCachePerfChunkSize = 2 * 1024 * 1024
	randomCachePerfPageSize  = 4 * 1024
)

type randomCachePerfCache struct {
	cachedBytes atomic.Int64
}

func (*randomCachePerfCache) ReadChunkAt([]byte, string, uint64) (int, error) { return 0, nil }
func (c *randomCachePerfCache) SetChunk(_ string, data []byte) {
	c.cachedBytes.Add(int64(len(data)))
}
func (*randomCachePerfCache) IsInCache(string, bool) bool { return false }
func (*randomCachePerfCache) GetMaxFilePartSizeInCache() uint64 {
	return randomCachePerfChunkSize
}

type randomCachePerfFixture struct {
	reader       *ChunkReadAt
	server       *httptest.Server
	requests     atomic.Int64
	backendBytes atomic.Int64
	cache        *randomCachePerfCache
}

type randomCachePerfResult struct {
	elapsed         time.Duration
	requests        int64
	backendBytes    int64
	cacheWriteBytes int64
}

func newRandomCachePerfFixture(t *testing.T, chunks int, latency time.Duration) *randomCachePerfFixture {
	t.Helper()

	f := &randomCachePerfFixture{cache: &randomCachePerfCache{}}
	data := make([]byte, randomCachePerfChunkSize)
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if latency > 0 {
			time.Sleep(latency)
		}

		start, end := int64(0), int64(len(data)-1)
		if value := strings.TrimPrefix(r.Header.Get("Range"), "bytes="); value != "" {
			parts := strings.SplitN(value, "-", 2)
			var err error
			start, err = strconv.ParseInt(parts[0], 10, 64)
			if err != nil {
				t.Fatalf("parse range start %q: %v", value, err)
			}
			if len(parts) == 2 && parts[1] != "" {
				end, err = strconv.ParseInt(parts[1], 10, 64)
				if err != nil {
					t.Fatalf("parse range end %q: %v", value, err)
				}
			}
			w.WriteHeader(http.StatusPartialContent)
		}
		if start < 0 || end < start || end >= int64(len(data)) {
			t.Fatalf("invalid requested range %d-%d", start, end)
		}
		response := data[start : end+1]
		f.backendBytes.Add(int64(len(response)))
		_, _ = w.Write(response)
	}))

	fileChunks := make([]*filer_pb.FileChunk, chunks)
	for i := range fileChunks {
		fileChunks[i] = &filer_pb.FileChunk{
			FileId:       fmt.Sprintf("1,%08x", i+1),
			Offset:       int64(i * randomCachePerfChunkSize),
			Size:         randomCachePerfChunkSize,
			ModifiedTsNs: int64(i + 1),
			Fid:          &filer_pb.FileId{FileKey: uint64(i + 1)},
		}
	}

	lookup := func(_ context.Context, fileID string) ([]string, error) {
		return []string{f.server.URL + "/" + fileID}, nil
	}
	views := ViewFromChunks(context.Background(), lookup, fileChunks, 0, int64(chunks*randomCachePerfChunkSize))
	// This fixture measures what the *cache* does once the reader is already in
	// random mode; the classifier itself is covered by reader_pattern_test.go.
	// Pin the mode explicitly rather than priming the heuristic with far-apart
	// reads: priming leaves the read frontier parked at the far end of the file
	// while the scenario below walks offsets 0..N upward toward it, so the
	// measured reads drift back inside the tolerance window and silently
	// re-classify as sequential partway through the run. That made the fixture
	// depend on the exact value of the tolerance constant -- it broke when the
	// 4.36 merge adopted upstream's 8 MiB SeqTolerance in place of the fork's
	// 4 MiB offsetTolerance, with the last few chunks flipping to whole-chunk
	// caching. The explicit override is what -readerCacheMode=random exists for.
	f.reader = NewChunkReaderAtFromClientWithMode(context.Background(), NewReaderCache(chunks+1, f.cache, lookup), views, int64(chunks*randomCachePerfChunkSize), 0, ReaderCacheModeRandom)

	if !f.reader.readerPattern.IsRandomMode() {
		t.Fatal("failed to force random reader mode")
	}
	return f
}

func (f *randomCachePerfFixture) close() {
	f.server.Close()
	f.reader.readerCache.destroy()
}

func (f *randomCachePerfFixture) readPage(t *testing.T, offset int64) {
	t.Helper()
	buf := make([]byte, randomCachePerfPageSize)
	n, err := f.reader.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		t.Fatalf("read at %d: %v", offset, err)
	}
	if n != len(buf) {
		t.Fatalf("read at %d: got %d bytes, want %d", offset, n, len(buf))
	}
}

func runRandomCachePerfScenario(t *testing.T, chunks, readsPerChunk int) randomCachePerfResult {
	t.Helper()
	f := newRandomCachePerfFixture(t, chunks, 2*time.Millisecond)
	defer f.close()

	started := time.Now()
	for chunk := 0; chunk < chunks; chunk++ {
		for read := 0; read < readsPerChunk; read++ {
			offsetInChunk := int64((read*7919)%((randomCachePerfChunkSize-randomCachePerfPageSize)/randomCachePerfPageSize)) * randomCachePerfPageSize
			f.readPage(t, int64(chunk*randomCachePerfChunkSize)+offsetInChunk)
		}
	}
	elapsed := time.Since(started)

	result := randomCachePerfResult{
		elapsed:         elapsed,
		requests:        f.requests.Load(),
		backendBytes:    f.backendBytes.Load(),
		cacheWriteBytes: f.cache.cachedBytes.Load(),
	}
	t.Logf("chunks=%d readsPerChunk=%d elapsed=%s requests=%d backendBytes=%d cacheWriteBytes=%d",
		chunks, readsPerChunk, result.elapsed.Round(time.Microsecond), result.requests, result.backendBytes, result.cacheWriteBytes)
	return result
}

// TestRandomCachePromotesOnlyAfterReuse prevents whole-chunk caching from
// turning a one-off 4KB random read into a 2MB backend transfer. A second
// read of the same chunk should promote it, retaining the request-collapse
// benefit for mmap/page-fault workloads that revisit a chunk.
func TestRandomCachePromotesOnlyAfterReuse(t *testing.T) {
	t.Run("SparseReadsStayRanged", func(t *testing.T) {
		const chunks = 32
		result := runRandomCachePerfScenario(t, chunks, 1)
		requestedBytes := int64(chunks * randomCachePerfPageSize)
		if result.backendBytes > requestedBytes*2 {
			t.Fatalf("sparse random reads fetched %d bytes for %d requested bytes", result.backendBytes, requestedBytes)
		}
	})

	t.Run("ReusedChunkIsPromoted", func(t *testing.T) {
		result := runRandomCachePerfScenario(t, 1, 128)
		if result.requests > 2 {
			t.Fatalf("reused chunk caused %d backend requests, want at most 2", result.requests)
		}
		if result.backendBytes > randomCachePerfChunkSize+randomCachePerfPageSize {
			t.Fatalf("reused chunk fetched %d bytes, want at most one range plus one chunk", result.backendBytes)
		}
	})
}

// TestRandomCachePerformanceCharacterization is an A/B harness for the
// random-read whole-chunk caching change. Run it at 3f4c8a188^ and at the
// desired revision to expose the latency/bandwidth tradeoff directly.
func TestRandomCachePerformanceCharacterization(t *testing.T) {
	t.Run("SparseOneReadPerChunk", func(t *testing.T) {
		runRandomCachePerfScenario(t, 32, 1)
	})
	t.Run("ModerateEightReadsPerChunk", func(t *testing.T) {
		runRandomCachePerfScenario(t, 32, 8)
	})
	t.Run("HeavyReuseOneChunk", func(t *testing.T) {
		runRandomCachePerfScenario(t, 1, 128)
	})
}
