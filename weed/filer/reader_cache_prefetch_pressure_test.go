package filer

import (
	"context"
	"sync/atomic"
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

// A consumer may join prefetch while it is waiting for admission. Declining
// speculation must not turn that consumer's demand read into an error.
func TestDemandJoiningDeclinedPrefetchRetries(t *testing.T) {
	const size = 64 << 10
	budget := NewReaderCacheBudget(size)
	rc := newStreamTestReaderCache((*chunk_cache.TieredChunkCache)(nil))
	rc.budget = budget
	defer rc.destroy()
	stream := &chunkStream{}
	defer rc.releaseStream(stream)
	if _, err := rc.readChunkAt(context.Background(), stream, make([]byte, 1), "current", nil, false, 0, size, false); err != nil {
		t.Fatal(err)
	}
	budget.Lock()
	rc.MaybeCache(&Interval[*ChunkView]{Value: &ChunkView{FileId: "next", ChunkSize: size, ViewSize: size}}, 1)
	result := make(chan error, 1)
	go func() {
		_, err := rc.ReadChunkAt(context.Background(), make([]byte, 1), "next", nil, false, 0, size, false)
		result <- err
	}()
	deadline := time.Now().Add(time.Second)
	joined := false
	for time.Now().Before(deadline) {
		rc.RLock()
		cacher := rc.downloaders["next"]
		joined = cacher != nil && atomic.LoadInt32(&cacher.readers) > 0
		rc.RUnlock()
		if joined {
			break
		}
		time.Sleep(time.Millisecond)
	}
	budget.Unlock()
	if !joined {
		t.Error("demand did not join waiting prefetch")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("demand failed to make progress")
	}
}

func TestPrefetchWindowDoesNotDoubleCountActiveUnpinnedReader(t *testing.T) {
	const size = 64 << 10
	rc := NewReaderCache(32, (*chunk_cache.TieredChunkCache)(nil), func(context.Context, string) ([]string, error) {
		return []string{"unused"}, nil
	}, nil, NewReaderCacheBudget(2*size))
	defer rc.destroy()

	chunk0Entered := make(chan struct{})
	releaseChunk0 := make(chan struct{})
	chunk1Fetched := make(chan struct{}, 1)
	rc.fetchChunkDataFn = func(_ context.Context, b []byte, _ []string, _ []byte, _ bool, _ bool, _ int64, fid string, _ util_http.RefreshUrlsFunc) (int, error) {
		if fid == "chunk0" {
			close(chunk0Entered)
			<-releaseChunk0
		} else if fid == "chunk1" {
			chunk1Fetched <- struct{}{}
		}
		return len(b), nil
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := rc.ReadChunkAt(context.Background(), make([]byte, 16), "chunk0", nil, false, 0, size, false)
		readDone <- err
	}()
	<-chunk0Entered

	window := &Interval[*ChunkView]{
		Value: &ChunkView{FileId: "chunk0", ChunkSize: size, ViewSize: size},
		Next: &Interval[*ChunkView]{
			Value: &ChunkView{FileId: "chunk1", ChunkSize: size, ViewSize: size},
		},
	}
	rc.MaybeCache(window, 2)

	select {
	case <-chunk1Fetched:
	case <-time.After(time.Second):
		close(releaseChunk0)
		<-readDone
		t.Fatal("MaybeCache failed to prefetch chunk1 within a 2-chunk budget because active unpinned reader on chunk0 was double-counted")
	}
	close(releaseChunk0)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
}

