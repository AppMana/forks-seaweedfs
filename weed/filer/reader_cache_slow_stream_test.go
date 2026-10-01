package filer

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/util/chunk_cache"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

// A slow stream copying its current chunk out in slices must keep that chunk
// while faster streams fill the shared memory budget with other chunks. The
// S3 gateway reads 2-4 MiB chunks in small slices; when the budget was full,
// the slow stream's completed chunk counted as idle and was evicted between
// two slices, so every slice re-downloaded the whole chunk. Production saw a
// gateway fetch 31 chunks 166 times to deliver 5 MB of one object.
func TestSlowStreamKeepsItsChunkUnderBudgetPressure(t *testing.T) {
	const chunkSize = 64 << 10
	const sliceSize = 8 << 10
	const slowChunks = 3
	const budgetChunks = 4

	var mu sync.Mutex
	fetches := map[string]int{}
	rc := NewReaderCache(64, (*chunk_cache.TieredChunkCache)(nil), func(context.Context, string) ([]string, error) {
		return []string{"unused"}, nil
	}, nil, NewReaderCacheBudget(budgetChunks*chunkSize))
	defer rc.destroy()
	rc.fetchChunkDataFn = func(_ context.Context, buffer []byte, _ []string, _ []byte, _ bool, _ bool, _ int64, fileId string, _ util_http.RefreshUrlsFunc) (int, error) {
		mu.Lock()
		fetches[fileId]++
		mu.Unlock()
		for i := range buffer {
			buffer[i] = fileId[len(fileId)-1]
		}
		return len(buffer), nil
	}

	newStream := func(prefix string, chunks int) *ChunkReadAt {
		views := NewIntervalList[*ChunkView]()
		for i := 0; i < chunks; i++ {
			views.AppendInterval(&Interval[*ChunkView]{
				StartOffset: int64(i * chunkSize),
				StopOffset:  int64((i + 1) * chunkSize),
				Value: &ChunkView{
					FileId:     fmt.Sprintf("%s%d", prefix, i),
					ViewSize:   chunkSize,
					ViewOffset: int64(i * chunkSize),
					ChunkSize:  chunkSize,
				},
			})
		}
		return NewChunkReaderAtFromClient(context.Background(), rc, views, int64(chunkSize*chunks), 0)
	}

	read := func(stream *ChunkReadAt, offset int64, want byte) {
		t.Helper()
		buf := make([]byte, sliceSize)
		n, err := stream.ReadAt(buf, offset)
		if (err != nil && err != io.EOF) || n != sliceSize {
			t.Fatalf("read at %d: n=%d err=%v", offset, n, err)
		}
		if buf[0] != want || buf[n-1] != want {
			t.Fatalf("read at %d: got %q, want %q", offset, buf[0], want)
		}
	}

	slow := newStream("slow", slowChunks)
	fast := 0
	for offset := int64(0); offset < chunkSize*slowChunks; offset += sliceSize {
		read(slow, offset, byte('0'+offset/chunkSize))
		// Between two slices of the slow stream, a faster stream downloads a
		// whole object of budget-filling size.
		object := newStream(fmt.Sprintf("fast%d-", fast), budgetChunks)
		fast++
		for o := int64(0); o < chunkSize*budgetChunks; o += sliceSize {
			read(object, o, byte('0'+o/chunkSize))
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < slowChunks; i++ {
		fileId := fmt.Sprintf("slow%d", i)
		if fetches[fileId] != 1 {
			t.Errorf("%s fetched %d times while its stream read it slice by slice, want 1", fileId, fetches[fileId])
		}
	}
}
