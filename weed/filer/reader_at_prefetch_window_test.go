package filer

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/util/chunk_cache"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

// A sequential reader issuing small reads against slow volume servers (a
// saturated link) must fetch every chunk once. The prefetch on each read
// used to start prefetchCount more chunks beyond those already in flight, so
// the window grew to the request's slot limit, the only completed chunk left
// to evict was the one being read, and each 32 KiB read downloaded its whole
// chunk again: Harbor pulls fetched every 4 MiB chunk ~130 times on
// 2026-10-29.
func TestSequentialSmallReadsFetchEachChunkOnceOnSlowServers(t *testing.T) {
	const chunkSize = 256 << 10
	const chunkCount = 40
	const sliceSize = 32 << 10

	var mu sync.Mutex
	fetches := map[string]int{}
	var inflight, maxInflight atomic.Int32
	root := NewReaderCache(64, (*chunk_cache.TieredChunkCache)(nil), func(context.Context, string) ([]string, error) {
		return []string{"unused"}, nil
	}, nil, NewReaderCacheBudget(512<<20))
	defer root.destroy()
	root.fetchChunkDataFn = func(_ context.Context, buffer []byte, _ []string, _ []byte, _ bool, _ bool, _ int64, fileId string, _ util_http.RefreshUrlsFunc) (int, error) {
		now := inflight.Add(1)
		for {
			old := maxInflight.Load()
			if now <= old || maxInflight.CompareAndSwap(old, now) {
				break
			}
		}
		defer inflight.Add(-1)
		mu.Lock()
		fetches[fileId]++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		for i := range buffer {
			buffer[i] = fileId[len(fileId)-1]
		}
		return len(buffer), nil
	}
	// The S3 gateway's per-request scope: 32 slots, a 64 MiB session budget.
	scope := root.NewRequestScope(32, 64<<20)

	views := NewIntervalList[*ChunkView]()
	for i := 0; i < chunkCount; i++ {
		views.AppendInterval(&Interval[*ChunkView]{
			StartOffset: int64(i * chunkSize),
			StopOffset:  int64((i + 1) * chunkSize),
			Value: &ChunkView{
				FileId:     fmt.Sprintf("chunk%02d", i),
				ViewSize:   chunkSize,
				ViewOffset: int64(i * chunkSize),
				ChunkSize:  chunkSize,
			},
		})
	}
	reader := NewChunkReaderAtFromClient(context.Background(), scope, views, chunkSize*chunkCount, DefaultPrefetchCount)

	buf := make([]byte, sliceSize)
	for off := int64(0); off < chunkSize*chunkCount; off += sliceSize {
		n, err := reader.ReadAt(buf, off)
		if (err != nil && err != io.EOF) || n != sliceSize {
			t.Fatalf("read at %d: n=%d err=%v", off, n, err)
		}
		if want := fmt.Sprintf("chunk%02d", off/chunkSize); buf[0] != want[len(want)-1] {
			t.Fatalf("read at %d returned data of another chunk", off)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	total := 0
	for _, n := range fetches {
		total += n
	}
	if total != chunkCount {
		t.Errorf("fetched %d chunks for %d chunks read once (%.1fx)", total, chunkCount, float64(total)/chunkCount)
	}
	// The demand read plus the prefetch window, never the whole slot limit.
	if got := maxInflight.Load(); got > DefaultPrefetchCount+1 {
		t.Errorf("%d fetches were in flight at once, want at most %d", got, DefaultPrefetchCount+1)
	}
}

// A sequential reader that consumes a chunk in several small ReadAt calls, as
// the S3 GET path does with its 256 KiB copy buffer over 4 MiB chunks, must
// keep its read-ahead at prefetchCount chunks. Each ReadAt triggers a
// prefetch; if chunks already being fetched are not counted toward the
// window, every call starts prefetchCount more downloads and the read-ahead
// runs to the ReaderCache downloader limit, buffering most of the object.
func TestChunkReadAtSequentialPrefetchStaysWithinWindow(t *testing.T) {
	const chunkSize = 64 << 10
	const chunkCount = 64
	const sliceSize = 16 << 10

	var readerChunk atomic.Int64
	var maxLead atomic.Int64
	rc := NewReaderCache(256, (*chunk_cache.TieredChunkCache)(nil), func(context.Context, string) ([]string, error) {
		return []string{"unused"}, nil
	}, nil)
	defer rc.destroy()
	rc.fetchChunkDataFn = func(_ context.Context, buffer []byte, _ []string, _ []byte, _ bool, _ bool, _ int64, fileId string, _ util_http.RefreshUrlsFunc) (int, error) {
		idx, err := strconv.Atoi(strings.TrimPrefix(fileId, "chunk"))
		if err != nil {
			return 0, err
		}
		lead := int64(idx) - readerChunk.Load()
		for {
			cur := maxLead.Load()
			if lead <= cur || maxLead.CompareAndSwap(cur, lead) {
				break
			}
		}
		return len(buffer), nil
	}

	views := NewIntervalList[*ChunkView]()
	for i := 0; i < chunkCount; i++ {
		views.AppendInterval(&Interval[*ChunkView]{
			StartOffset: int64(i * chunkSize),
			StopOffset:  int64((i + 1) * chunkSize),
			Value: &ChunkView{
				FileId:     fmt.Sprintf("chunk%d", i),
				ViewSize:   chunkSize,
				ViewOffset: int64(i * chunkSize),
				ChunkSize:  chunkSize,
			},
		})
	}
	reader := NewChunkReaderAtFromClient(context.Background(), rc, views, chunkSize*chunkCount, DefaultPrefetchCount)
	defer reader.ReleaseStream()

	buf := make([]byte, sliceSize)
	for off := int64(0); off < chunkSize*chunkCount; off += sliceSize {
		readerChunk.Store(off / chunkSize)
		if _, err := reader.ReadAt(buf, off); err != nil && off+sliceSize < chunkSize*chunkCount {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
	}

	if got := maxLead.Load(); got > DefaultPrefetchCount {
		t.Fatalf("read-ahead reached %d chunks past the reader, want at most %d", got, DefaultPrefetchCount)
	}
}
