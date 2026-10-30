package filer

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/util/chunk_cache"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
)

// A slow stream copying its current chunk out in slices must keep that chunk
// while faster streams fill the shared memory budget with other chunks.
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

// When a stream has completed and pinned its current chunk (1*chunkSize in
// b.idle with pins==1) while the remaining budget slots are occupied by
// in-flight downloads (in b.reservations, not yet in b.idle), a new download
// waiting for budget must wait for an in-flight download to finish or a chunk
// to be unpinned rather than evicting the pinned chunk out from under the
// active reader.
func TestSlowStreamKeepsItsChunkWhenOtherSlotsAreInFlight(t *testing.T) {
	const chunkSize = 64 << 10
	const sliceSize = 8 << 10
	const budgetChunks = 4

	var mu sync.Mutex
	fetches := map[string]int{}
	inFlightEntered := make(chan string, 16)
	releaseInFlight := make(chan struct{})

	rc := NewReaderCache(64, (*chunk_cache.TieredChunkCache)(nil), func(context.Context, string) ([]string, error) {
		return []string{"unused"}, nil
	}, nil, NewReaderCacheBudget(budgetChunks*chunkSize))
	defer rc.destroy()

	rc.fetchChunkDataFn = func(_ context.Context, buffer []byte, _ []string, _ []byte, _ bool, _ bool, _ int64, fileId string, _ util_http.RefreshUrlsFunc) (int, error) {
		mu.Lock()
		fetches[fileId]++
		mu.Unlock()
		if fileId != "slow0" {
			inFlightEntered <- fileId
			<-releaseInFlight
		}
		for i := range buffer {
			buffer[i] = fileId[len(fileId)-1]
		}
		return len(buffer), nil
	}

	newSingleChunkStream := func(fid string) *ChunkReadAt {
		views := NewIntervalList[*ChunkView]()
		views.AppendInterval(&Interval[*ChunkView]{
			StartOffset: 0,
			StopOffset:  chunkSize,
			Value: &ChunkView{
				FileId:     fid,
				ViewSize:   chunkSize,
				ViewOffset: 0,
				ChunkSize:  chunkSize,
			},
		})
		return NewChunkReaderAtFromClient(context.Background(), rc, views, chunkSize, 0)
	}

	slow := newSingleChunkStream("slow0")
	defer slow.ReleaseStream()

	// Read slice 0 of slow0: slow0 completes and is pinned (pins == 1) in b.idle.
	buf := make([]byte, sliceSize)
	if n, err := slow.ReadAt(buf, 0); (err != nil && err != io.EOF) || n != sliceSize {
		t.Fatalf("initial slow0 slice read: n=%d err=%v", n, err)
	}

	// Launch 4 concurrent downloads (fast1..fast4). The first 3 fill the
	// remaining 3 budget slots in b.reservations while in flight. The 4th
	// enters reserveSelf when b.idle contains ONLY slow0 (pinned!).
	var wg sync.WaitGroup
	for i := 1; i <= 4; i++ {
		wg.Add(1)
		fid := fmt.Sprintf("fast%d", i)
		go func() {
			defer wg.Done()
			s := newSingleChunkStream(fid)
			defer s.ReleaseStream()
			b := make([]byte, sliceSize)
			_, _ = s.ReadAt(b, 0)
		}()
	}

	// Wait until 3 fast downloads are in flight (holding 3*chunkSize in b.reservations)
	// and give the 4th goroutine a moment to hit reserveSelf.
	for i := 0; i < 3; i++ {
		<-inFlightEntered
	}
	time.Sleep(15 * time.Millisecond)

	// Unblock all in-flight downloads so they can finish and release their pins.
	close(releaseInFlight)
	wg.Wait()

	// Now read the remaining slices of slow0. Because slow0 was pinned the
	// entire time, it must still be cached and never re-fetched.
	for off := int64(sliceSize); off < chunkSize; off += sliceSize {
		if n, err := slow.ReadAt(buf, off); (err != nil && err != io.EOF) || n != sliceSize {
			t.Fatalf("slow0 read at %d: n=%d err=%v", off, n, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if fetches["slow0"] != 1 {
		t.Fatalf("slow0 fetched %d times while pinned, want 1", fetches["slow0"])
	}
}

// When a stream's session or shared budget holds N chunks (e.g. 4 chunks) and
// prefetchCount >= N (DefaultPrefetchCount = 4), reading sequentially in small
// slices must not let prefetching overrun the budget and FIFO-evict the next
// chunk (chunk1) before the stream reaches it.
func TestPrefetchWindowFitsBudgetAndDoesNotEvictNextChunk(t *testing.T) {
	const chunkSize = 64 << 10
	const sliceSize = 8 << 10
	const chunkCount = 12
	const budgetChunks = 4 // matches DefaultPrefetchCount = 4

	var mu sync.Mutex
	fetches := map[string]int{}
	root := NewReaderCache(64, (*chunk_cache.TieredChunkCache)(nil), func(context.Context, string) ([]string, error) {
		return []string{"unused"}, nil
	}, nil, NewReaderCacheBudget(512<<20))
	defer root.destroy()

	root.fetchChunkDataFn = func(_ context.Context, buffer []byte, _ []string, _ []byte, _ bool, _ bool, _ int64, fileId string, _ util_http.RefreshUrlsFunc) (int, error) {
		mu.Lock()
		fetches[fileId]++
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		for i := range buffer {
			buffer[i] = fileId[len(fileId)-1]
		}
		return len(buffer), nil
	}

	scope := root.NewRequestScope(32, int64(budgetChunks*chunkSize))
	defer scope.destroy()

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
	reader := NewChunkReaderAtFromClient(context.Background(), scope, views, int64(chunkSize*chunkCount), DefaultPrefetchCount)
	defer reader.Close()

	buf := make([]byte, sliceSize)
	for off := int64(0); off < int64(chunkSize*chunkCount); off += sliceSize {
		n, err := reader.ReadAt(buf, off)
		if (err != nil && err != io.EOF) || n != sliceSize {
			t.Fatalf("read at %d: n=%d err=%v", off, n, err)
		}
		want := fmt.Sprintf("chunk%02d", off/chunkSize)
		if buf[0] != want[len(want)-1] {
			t.Fatalf("read at %d: got %q, want %q", off, buf[0], want[len(want)-1])
		}
	}

	mu.Lock()
	defer mu.Unlock()
	total := 0
	for i := 0; i < chunkCount; i++ {
		fid := fmt.Sprintf("chunk%02d", i)
		total += fetches[fid]
		if fetches[fid] != 1 {
			t.Errorf("%s fetched %d times, want 1", fid, fetches[fid])
		}
	}
	if total != chunkCount {
		t.Errorf("total fetches = %d for %d chunks (%.2fx amplification), want %d", total, chunkCount, float64(total)/float64(chunkCount), chunkCount)
	}
}

func TestUnalignedRangeStreamDoesNotDropStreamPinOrEvictNextChunk(t *testing.T) {
	const chunkSize = 64 << 10
	const sliceSize = 16 << 10
	const chunkCount = 10
	const budgetChunks = 4

	var mu sync.Mutex
	fetches := map[string]int{}
	rangeFetches := map[string]int{}
	root := NewReaderCache(64, (*chunk_cache.TieredChunkCache)(nil), func(context.Context, string) ([]string, error) {
		return []string{"unused"}, nil
	}, nil, NewReaderCacheBudget(512<<20))
	defer root.destroy()

	root.fetchChunkDataFn = func(_ context.Context, buffer []byte, _ []string, _ []byte, _ bool, isFullChunk bool, _ int64, fileId string, _ util_http.RefreshUrlsFunc) (int, error) {
		mu.Lock()
		if isFullChunk {
			fetches[fileId]++
		} else {
			rangeFetches[fileId]++
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		for i := range buffer {
			buffer[i] = fileId[len(fileId)-1]
		}
		return len(buffer), nil
	}

	scope := root.NewRequestScope(32, int64(budgetChunks*chunkSize))
	defer scope.ReleaseScope()

	// Simulate an unaligned open-ended Range GET starting at chunkSize/8 (8 KiB into chunk00)
	startOff := int64(chunkSize / 8)
	totalSize := int64(chunkSize * chunkCount)
	rangeSize := totalSize - startOff

	vis := NewIntervalList[*VisibleInterval]()
	for i := 0; i < chunkCount; i++ {
		vis.AppendInterval(&Interval[*VisibleInterval]{
			StartOffset: int64(i * chunkSize),
			StopOffset:  int64((i + 1) * chunkSize),
			Value: &VisibleInterval{
				start:     int64(i * chunkSize),
				stop:      int64((i + 1) * chunkSize),
				fileId:    fmt.Sprintf("chunk%02d", i),
				chunkSize: chunkSize,
			},
		})
	}
	views := ViewFromVisibleIntervals(vis, startOff, rangeSize)
	reader := NewChunkReaderAtFromClient(context.Background(), scope, views, totalSize, DefaultPrefetchCount)
	defer reader.ReleaseStream()

	buf := make([]byte, sliceSize)
	for off := startOff; off < totalSize; off += int64(len(buf)) {
		if off+int64(len(buf)) > totalSize {
			buf = buf[:totalSize-off]
		}
		n, err := reader.ReadAt(buf, off)
		if (err != nil && err != io.EOF) || n != len(buf) {
			t.Fatalf("read at %d: n=%d want=%d err=%v", off, n, len(buf), err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < chunkCount; i++ {
		fid := fmt.Sprintf("chunk%02d", i)
		if fetches[fid] != 1 {
			t.Errorf("%s whole fetches = %d, want 1", fid, fetches[fid])
		}
	}
}
