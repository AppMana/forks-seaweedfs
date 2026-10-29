package filer

import (
	"context"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/util/chunk_cache"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

func TestPrefetchDoesNotEvictCurrentChunkForBudget(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "scope"}[scoped], func(t *testing.T) {
			const size = 64 << 10
			rc := NewReaderCache(32, (*chunk_cache.TieredChunkCache)(nil), func(context.Context, string) ([]string, error) { return []string{"unused"}, nil }, nil, NewReaderCacheBudget(size))
			defer rc.destroy()
			if scoped {
				rc = rc.NewRequestScope(32, size)
			}
			fetched := make(chan string, 8)
			rc.fetchChunkDataFn = func(_ context.Context, b []byte, _ []string, _ []byte, _ bool, _ bool, _ int64, fid string, _ util_http.RefreshUrlsFunc) (int, error) {
				fetched <- fid
				return len(b), nil
			}
			stream := &chunkStream{}
			defer rc.releaseStream(stream)
			if _, err := rc.readChunkAt(context.Background(), stream, make([]byte, 1024), "current", nil, false, 0, size, false); err != nil {
				t.Fatal(err)
			}
			<-fetched
			rc.MaybeCache(&Interval[*ChunkView]{Value: &ChunkView{FileId: "next", ChunkSize: size, ViewSize: size}}, 1)
			select {
			case fid := <-fetched:
				t.Errorf("speculation fetched %s by evicting the current chunk", fid)
			case <-time.After(100 * time.Millisecond):
			}
			if !isRetained(rc.rootOrSelf(), "current") {
				t.Error("prefetch evicted pinned current chunk")
			}
		})
	}
}
