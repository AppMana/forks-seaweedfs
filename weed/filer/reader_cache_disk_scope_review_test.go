package filer

import (
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/util/chunk_cache"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

// Exercise the production disk quotas without allocating their capacity.
// The admission limit is a file-prefix cutoff, not the whole-cache capacity.
func TestReviewProductionDiskCacheAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int64
		want   int32
	}{
		{"within495MiB", 490 << 20, 1}, {"past512MiB", 513 << 20, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := chunk_cache.NewSharedTieredChunkCache(8, t.TempDir(), "s3", 2048, 51200, 1<<20)
			defer cache.Shutdown()
			if got := cache.GetMaxFilePartSizeInCache(); got != 512<<20 {
				t.Fatalf("cutoff %d", got)
			}
			rc := NewReaderCache(32, cache, func(context.Context, string) ([]string, error) { return []string{"unused"}, nil }, nil)
			defer rc.destroy()
			var fetches atomic.Int32
			rc.fetchChunkDataFn = func(_ context.Context, b []byte, _ []string, _ []byte, _ bool, _ bool, _ int64, _ string, _ util_http.RefreshUrlsFunc) (int, error) {
				fetches.Add(1)
				for i := range b {
					b[i] = 42
				}
				return len(b), nil
			}
			const size = 4 << 20
			for i := 0; i < 2; i++ {
				views := NewIntervalList[*ChunkView]()
				views.AppendInterval(&Interval[*ChunkView]{StartOffset: tc.offset, StopOffset: tc.offset + size, Value: &ChunkView{FileId: "1,0100000001", ViewOffset: tc.offset, ViewSize: size, ChunkSize: size}})
				reader := NewChunkReaderAtFromClient(context.Background(), rc, views, tc.offset+size, 0)
				buf := make([]byte, size)
				n, err := reader.ReadAt(buf, tc.offset)
				reader.ReleaseStream()
				if n != size || (err != nil && err != io.EOF) || buf[0] != 42 || buf[size-1] != 42 {
					t.Fatalf("read %d: n=%d err=%v", i, n, err)
				}
				rc.UnCache("1,0100000001")
			}
			if got := fetches.Load(); got != tc.want {
				t.Fatalf("fetches=%d want %d", got, tc.want)
			}
		})
	}
}
