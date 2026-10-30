package filer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/util/chunk_cache"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
	"github.com/seaweedfs/seaweedfs/weed/util/mem"
	"github.com/seaweedfs/seaweedfs/weed/wdclient"
)

type CacheInvalidator interface {
	InvalidateCache(fileId string)
}

type fetchChunkDataFnType func(ctx context.Context, buffer []byte, urlStrings []string, cipherKey []byte, isGzipped bool, isFullChunk bool, offset int64, fileId string, refreshUrls util_http.RefreshUrlsFunc) (n int, err error)

type ReaderCache struct {
	root             *ReaderCache
	chunkCache       chunk_cache.ChunkCache
	lookupFileIdFn   wdclient.LookupFileIdFunctionType
	cacheInvalidator CacheInvalidator
	fetchChunkDataFn fetchChunkDataFnType
	sync.RWMutex
	downloaders map[string]*SingleChunkCacher
	limit       int
	budget      *ReaderCacheBudget
	// keepUntilEvicted retains completed chunk buffers after their readers
	// finish or leave, until slot or budget eviction picks them. A mount
	// file's sections share one cache across concurrent and revisiting reads
	// (mmap page faults), so dropping a chunk on leave re-downloads it.
	keepUntilEvicted bool
}

type SingleChunkCacher struct {
	completedTimeNew int64
	readers          int32
	consumed         int32
	pins             int32 // streams currently positioned inside this chunk
	left             int32 // set once the last pinning stream moved on
	readOnce         int32 // set once a foreground read attaches to this chunk
	sync.Mutex
	parent         *ReaderCache
	scope          *ReaderCache
	budget         *ReaderCacheBudget
	chunkFileId    string
	data           []byte
	err            error
	cipherKey      []byte
	isGzipped      bool
	isPrefetch     bool // prefetch must not displace a stream for its memory reservation
	chunkSize      int
	shouldCache    bool
	wg             sync.WaitGroup
	cacheStartedCh chan struct{}
	done           chan struct{} // signals when download is complete
}

func NewReaderCache(limit int, chunkCache chunk_cache.ChunkCache, lookupFileIdFn wdclient.LookupFileIdFunctionType, cacheInvalidator CacheInvalidator, budgets ...*ReaderCacheBudget) *ReaderCache {
	var budget *ReaderCacheBudget
	if len(budgets) > 0 {
		budget = budgets[0]
	}
	return &ReaderCache{
		limit:            limit,
		budget:           budget,
		chunkCache:       chunkCache,
		lookupFileIdFn:   lookupFileIdFn,
		cacheInvalidator: cacheInvalidator,
		fetchChunkDataFn: util_http.RetriedFetchChunkData,
		downloaders:      make(map[string]*SingleChunkCacher),
	}
}

func (rc *ReaderCache) rootOrSelf() *ReaderCache {
	if rc != nil && rc.root != nil {
		return rc.root
	}
	return rc
}

// NewRequestScope creates a per-request / per-session ReaderCache view that
// shares the parent whole-cache downloaders map, ChunkCache, and whole-cache
// budget while enforcing per-request downloader slot and memory budget limits.
func (rc *ReaderCache) NewRequestScope(requestLimit int, requestBudgetBytes int64) *ReaderCache {
	if rc == nil {
		return nil
	}
	root := rc.rootOrSelf()
	if requestLimit <= 0 || (root.limit > 0 && requestLimit > root.limit) {
		requestLimit = root.limit
	}
	var scopeBudget *ReaderCacheBudget
	if root.budget != nil || requestBudgetBytes > 0 {
		scopeBudget = NewScopedReaderCacheBudget(root.budget, requestBudgetBytes)
	}
	return &ReaderCache{
		root:             root,
		limit:            requestLimit,
		budget:           scopeBudget,
		chunkCache:       root.chunkCache,
		lookupFileIdFn:   root.lookupFileIdFn,
		cacheInvalidator: root.cacheInvalidator,
		fetchChunkDataFn: root.fetchChunkDataFn,
		downloaders:      root.downloaders,
		keepUntilEvicted: rc.keepUntilEvicted,
	}
}

// Budget returns the ReaderCacheBudget governing this cache or scope.
func (rc *ReaderCache) Budget() *ReaderCacheBudget {
	if rc == nil {
		return nil
	}
	return rc.budget
}

// ChunkCache returns the underlying ChunkCache.
func (rc *ReaderCache) ChunkCache() chunk_cache.ChunkCache {
	if rc == nil {
		return nil
	}
	return rc.rootOrSelf().chunkCache
}

// KeepUntilEvicted makes the cache retain chunk buffers that were fully read
// or left by every stream until slot or budget eviction removes them.
func (rc *ReaderCache) KeepUntilEvicted() *ReaderCache {
	rc.keepUntilEvicted = true
	return rc
}

func (rc *ReaderCache) scopeDownloaderCountLocked() int {
	root := rc.rootOrSelf()
	if rc == root {
		return len(root.downloaders)
	}
	count := 0
	for _, d := range root.downloaders {
		if d.scope == rc {
			count++
		}
	}
	return count
}

func (rc *ReaderCache) pickOldestVictimLocked(onlyScope *ReaderCache) string {
	root := rc.rootOrSelf()
	oldestReadFid, oldestReadTime := "", time.Now().UnixNano()
	newestPrefetchFid, newestPrefetchTime := "", int64(0)
	pinnedFid, pinnedTime := "", int64(0)
	inFlight := 0
	for fid, downloader := range root.downloaders {
		if onlyScope != nil && downloader.scope != onlyScope {
			continue
		}
		completedTime := atomic.LoadInt64(&downloader.completedTimeNew)
		if completedTime <= 0 {
			inFlight++
			continue
		}
		if atomic.LoadInt32(&downloader.pins) == 0 {
			if !downloader.isUnreadPrefetch() {
				if completedTime < oldestReadTime {
					oldestReadFid, oldestReadTime = fid, completedTime
				}
			} else if newestPrefetchFid == "" || completedTime > newestPrefetchTime {
				newestPrefetchFid, newestPrefetchTime = fid, completedTime
			}
		} else if pinnedFid == "" || completedTime < pinnedTime {
			pinnedFid, pinnedTime = fid, completedTime
		}
	}
	if oldestReadFid != "" {
		return oldestReadFid
	}
	if newestPrefetchFid != "" {
		return newestPrefetchFid
	}
	if inFlight == 0 {
		return pinnedFid
	}
	return ""
}

// MaybeCache prefetches up to 'count' chunks ahead in parallel.
// This improves read throughput for sequential reads by keeping the
// network pipeline full with parallel chunk fetches.
func (rc *ReaderCache) MaybeCache(chunkViews *Interval[*ChunkView], count int) {
	root := rc.rootOrSelf()
	if root.lookupFileIdFn == nil {
		return
	}
	if count <= 0 {
		count = 1
	}

	var started []*SingleChunkCacher
	defer func() {
		for _, cacher := range started {
			<-cacher.cacheStartedCh
		}
	}()

	root.Lock()
	defer root.Unlock()

	if len(root.downloaders) >= root.limit || rc.scopeDownloaderCountLocked() >= rc.limit {
		return
	}

	budgetLimit := rc.budget.effectiveLimit()
	var pinnedBytes int64
	if budgetLimit > 0 {
		for _, d := range root.downloaders {
			if (rc == root || d.scope == rc) && (atomic.LoadInt32(&d.pins) > 0 || atomic.LoadInt32(&d.readers) > 0) {
				pinnedBytes += int64(mem.AllocationSize(d.chunkSize))
			}
		}
	}

	// The window is the next count chunks, whether already downloading,
	// cached or started here. Skipping present chunks without counting them
	// let every read on a slow link start count more, until the prefetch
	// filled the request's slots and evicted the chunk being read.
	window := 0
	var windowBytes int64
	for x := chunkViews; x != nil && window < count; x = x.Next {
		chunkView := x.Value
		window++
		chunkAlloc := int64(mem.AllocationSize(int(chunkView.ChunkSize)))
		if existing, found := root.downloaders[chunkView.FileId]; found {
			if atomic.LoadInt32(&existing.pins) == 0 || (rc != root && existing.scope != rc) {
				windowBytes += chunkAlloc
			}
			continue
		}
		if root.chunkCache.IsInCache(chunkView.FileId, true) {
			glog.V(4).Infof("%s is in cache", chunkView.FileId)
			continue
		}

		if len(root.downloaders) >= root.limit || rc.scopeDownloaderCountLocked() >= rc.limit {
			// abort when slots are filled
			return
		}
		if (chunkView.CanRangeFetch() || !rc.budget.canFit(int(chunkView.ChunkSize))) && !chunkView.IsFullChunk() {
			// the view is clipped to part of the chunk and will be
			// range-fetched, so prefetching it whole would download bytes
			// nobody needs; a ciphered or compressed partial view needs
			// the whole blob anyway and is worth prefetching, but not when
			// it cannot fit the budget at all
			continue
		}
		if budgetLimit > 0 && pinnedBytes+windowBytes+chunkAlloc > budgetLimit {
			// Do not launch prefetches beyond the active budget capacity;
			// otherwise the furthest prefetch evicts the next chunk before the
			// stream reaches it.
			return
		}

		// glog.V(4).Infof("prefetch %s offset %d", chunkView.FileId, chunkView.ViewOffset)
		// cache this chunk if not yet
		shouldCache := chunkView.ChunkSize <= root.chunkCache.GetMaxFilePartSizeInCache()
		cacher := newSingleChunkCacher(root, rc, chunkView.FileId, chunkView.CipherKey, chunkView.IsGzipped, int(chunkView.ChunkSize), shouldCache)
		cacher.isPrefetch = true
		cacher.wg.Add(1) // the fetch, so destroy() waits even before the goroutine runs
		root.downloaders[chunkView.FileId] = cacher
		windowBytes += chunkAlloc
		go cacher.startCaching()
		started = append(started, cacher)
	}

	return
}

// fetchChunkRange downloads only [offset, offset+len(buffer)) of a chunk,
// for views clipped to part of their chunk and for random-mode reads. It
// goes through fetchChunkDataFn so tests observe range fetches the same way
// they observe whole-chunk downloads.
func (rc *ReaderCache) fetchChunkRange(ctx context.Context, buffer []byte, chunkView *ChunkView, offset int64) (int, error) {
	root := rc.rootOrSelf()
	urlStrings, err := root.lookupFileIdFn(ctx, chunkView.FileId)
	if err != nil {
		glog.ErrorfCtx(ctx, "operation LookupFileId %s failed, err: %v", chunkView.FileId, err)
		return 0, err
	}
	return root.fetchChunkDataFn(ctx, buffer, urlStrings, chunkView.CipherKey, chunkView.IsGzipped, false, offset, chunkView.FileId,
		refreshUrls(ctx, root.cacheInvalidator, root.lookupFileIdFn, chunkView.FileId))
}

// chunkStream is one sequential reader's position in a shared ReaderCache.
// The chunk it is reading stays pinned until the stream reads it to the end or
// moves elsewhere, so another stream finishing or leaving the same chunk does
// not drop the buffer from under it.
type chunkStream struct {
	mu     sync.Mutex // guards cacher; concurrent ReadAt calls on one ChunkReadAt share the stream
	cacher *SingleChunkCacher
}

func (rc *ReaderCache) ReadChunkAt(ctx context.Context, buffer []byte, fileId string, cipherKey []byte, isGzipped bool, offset int64, chunkSize int, shouldCache bool) (int, error) {
	return rc.readChunkAt(ctx, nil, buffer, fileId, cipherKey, isGzipped, offset, chunkSize, shouldCache)
}

func (rc *ReaderCache) readChunkAt(ctx context.Context, stream *chunkStream, buffer []byte, fileId string, cipherKey []byte, isGzipped bool, offset int64, chunkSize int, shouldCache bool) (int, error) {
	root := rc.rootOrSelf()
retry:
	root.RLock()
	if cacher, found := root.downloaders[fileId]; found {
		if cacher.hasCompletedError() {
			root.RUnlock()
			root.remove(cacher)
			goto retry
		}
		// Count this read on the cacher before releasing the map lock, so a
		// concurrent destroy() (error eviction here, LRU, or UnCache) cannot
		// start wg.Wait() on a zero counter while this read is about to register.
		cacher.wg.Add(1)
		atomic.AddInt32(&cacher.readers, 1)
		atomic.StoreInt32(&cacher.readOnce, 1)
		previous := stream.pin(cacher)
		root.RUnlock()
		n, err := rc.readFromCacher(ctx, stream, cacher, previous, buffer, offset, chunkSize)
		if n > 0 || err != nil {
			return n, err
		}
		// If n=0 and err=nil, the cacher couldn't provide data for this offset.
		// Fall through to try chunkCache.
	} else {
		root.RUnlock()
	}

	if shouldCache || root.lookupFileIdFn == nil {
		n, err := root.chunkCache.ReadChunkAt(buffer, fileId, uint64(offset))
		if n > 0 {
			// Served from the chunk cache: the stream has left its pinned chunk.
			rc.unpin(stream.detach(nil))
			return n, err
		}
	}

	root.Lock()
	// A downloader may have registered between the read lock and this one.
	if cacher, found := root.downloaders[fileId]; found {
		if cacher.hasCompletedError() {
			delete(root.downloaders, fileId)
			root.Unlock()
			cacher.destroy()
			goto retry
		}
		cacher.wg.Add(1)
		atomic.AddInt32(&cacher.readers, 1)
		atomic.StoreInt32(&cacher.readOnce, 1)
		previous := stream.pin(cacher)
		root.Unlock()
		n, err := rc.readFromCacher(ctx, stream, cacher, previous, buffer, offset, chunkSize)
		return n, err
	}

	// Enforce per-request / per-session slot limit first so a noisy request
	// evicts its own completed chunks before affecting other concurrent streams.
	if rc != root && rc.scopeDownloaderCountLocked() >= rc.limit {
		if oldestFid := rc.pickOldestVictimLocked(rc); oldestFid != "" {
			oldDownloader := root.downloaders[oldestFid]
			delete(root.downloaders, oldestFid)
			root.Unlock()
			oldDownloader.destroy()
			goto retry
		}
	}

	// Clean up old downloaders at the whole-cache level; prefer one no stream
	// is positioned in, but fall back to a pinned one so abandoned pins cannot
	// bypass the limit.
	if len(root.downloaders) >= root.limit {
		if oldestFid := rc.pickOldestVictimLocked(nil); oldestFid != "" {
			oldDownloader := root.downloaders[oldestFid]
			delete(root.downloaders, oldestFid)
			root.Unlock()
			oldDownloader.destroy()
			goto retry
		}
	}

	// glog.V(4).Infof("cache1 %s", fileId)

	cacher := newSingleChunkCacher(root, rc, fileId, cipherKey, isGzipped, chunkSize, shouldCache)
	atomic.StoreInt32(&cacher.readOnce, 1)
	cacher.wg.Add(2) // the fetch plus this read, so destroy() waits even before the goroutine runs
	atomic.AddInt32(&cacher.readers, 1)
	root.downloaders[fileId] = cacher
	previous := stream.pin(cacher)
	root.Unlock()
	rc.unpin(previous)
	go cacher.startCaching()
	<-cacher.cacheStartedCh
	n, err := rc.readFromCacher(ctx, stream, cacher, nil, buffer, offset, chunkSize)
	return n, err
}

// readFromCacher serves the read and releases the pin if the stream finished
// the chunk. The caller must have registered the read (wg + readers) and
// pinned the stream under the map lock; previous is the chunk the stream left
// when it pinned.
func (rc *ReaderCache) readFromCacher(ctx context.Context, stream *chunkStream, cacher *SingleChunkCacher, previous *SingleChunkCacher, buffer []byte, offset int64, chunkSize int) (n int, err error) {
	rc.unpin(previous)
	n, err = cacher.readChunkAt(ctx, buffer, offset)
	if errors.Is(err, errPrefetchBudgetBusy) {
		// A demand reader may have joined a speculative download before
		// admission failed. Retry with demand priority instead of exposing
		// the internal prefetch decision as a client read error.
		rc.rootOrSelf().remove(cacher)
		return rc.readChunkAt(ctx, stream, buffer, cacher.chunkFileId, cacher.cipherKey, cacher.isGzipped, offset, chunkSize, cacher.shouldCache)
	}
	rc.releaseIfFinished(stream, cacher, offset, n, err, chunkSize)
	return
}

// pin makes cacher the stream's current chunk and returns the chunk it was
// pinned to before, which the caller unpins.
func (stream *chunkStream) pin(cacher *SingleChunkCacher) (previous *SingleChunkCacher) {
	if stream == nil {
		return nil
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.cacher == cacher {
		return nil
	}
	atomic.AddInt32(&cacher.pins, 1)
	previous = stream.cacher
	stream.cacher = cacher
	return previous
}

// isPinnedTo reports whether the stream is currently positioned in fileId.
func (stream *chunkStream) isPinnedTo(fileId string) bool {
	if stream == nil {
		return false
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	return stream.cacher != nil && stream.cacher.chunkFileId == fileId
}

// detach unpins the stream from its chunk and returns that chunk for the
// caller to unpin. A non-nil expected cacher makes the detach conditional on
// the stream still being positioned in it.
func (stream *chunkStream) detach(expected *SingleChunkCacher) (previous *SingleChunkCacher) {
	if stream == nil {
		return nil
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if expected != nil && stream.cacher != expected {
		return nil
	}
	previous = stream.cacher
	stream.cacher = nil
	return previous
}

// releaseIfFinished unpins the stream's chunk once the stream has read it to
// the end, since the stream will not come back to it.
func (rc *ReaderCache) releaseIfFinished(stream *chunkStream, cacher *SingleChunkCacher, offset int64, n int, err error, chunkSize int) {
	if stream == nil || err != nil || offset+int64(n) < int64(chunkSize) {
		return
	}
	rc.unpin(stream.detach(cacher))
}

// releaseStream unpins whatever chunk the stream is positioned in.
func (rc *ReaderCache) releaseStream(stream *chunkStream) {
	rc.unpin(stream.detach(nil))
}

// unpin drops one stream's pin. Once no stream is positioned in the chunk it
// is dropped like UnCache would, as soon as no read is in progress either:
// here if none is, otherwise by the last read's removeConsumed.
func (rc *ReaderCache) unpin(downloader *SingleChunkCacher) {
	if downloader == nil {
		return
	}
	if atomic.AddInt32(&downloader.pins, -1) == 0 {
		atomic.StoreInt32(&downloader.left, 1)
		downloader.budget.notifyChanged()
	}
	rc.removeConsumed(downloader)
}

func (rc *ReaderCache) UnCache(fileId string) {
	root := rc.rootOrSelf()
	root.Lock()
	downloader := root.downloaders[fileId]
	delete(root.downloaders, fileId)
	root.Unlock()
	if downloader != nil {
		downloader.destroy()
	}
}

func (rc *ReaderCache) remove(downloader *SingleChunkCacher) {
	root := rc.rootOrSelf()
	root.Lock()
	removed := root.downloaders[downloader.chunkFileId] == downloader
	if removed {
		delete(root.downloaders, downloader.chunkFileId)
	}
	root.Unlock()
	if removed {
		downloader.destroy()
	}
}

// removeUnpinned drops a cacher only while no stream is positioned in it.
// Budget eviction picks its victim under the budget lock, so the pin check
// and the map removal must happen together under the ReaderCache lock.
func (rc *ReaderCache) removeUnpinned(downloader *SingleChunkCacher) (removed bool) {
	root := rc.rootOrSelf()
	root.Lock()
	removed = root.downloaders[downloader.chunkFileId] == downloader &&
		atomic.LoadInt32(&downloader.pins) == 0 &&
		atomic.LoadInt32(&downloader.readers) == 0
	if removed {
		delete(root.downloaders, downloader.chunkFileId)
	}
	root.Unlock()
	if removed {
		downloader.destroy()
	}
	return
}

// consumable reports whether the cacher could be dropped: its buffer was
// fully read or every stream positioned in it has left, and no reader is
// attached or pinned. All fields are atomics, so callers can skip the
// ReaderCache lock whenever removal is impossible.
func (s *SingleChunkCacher) consumable() bool {
	return atomic.LoadInt32(&s.readers) == 0 &&
		atomic.LoadInt32(&s.pins) == 0 &&
		(atomic.LoadInt32(&s.consumed) != 0 || atomic.LoadInt32(&s.left) != 0)
}

func (s *SingleChunkCacher) shouldKeepUntilEvicted() bool {
	if s.scope != nil && s.scope.keepUntilEvicted {
		return true
	}
	return s.parent != nil && s.parent.keepUntilEvicted
}

func (s *SingleChunkCacher) isUnreadPrefetch() bool {
	return s.isPrefetch &&
		atomic.LoadInt32(&s.readOnce) == 0 &&
		atomic.LoadInt32(&s.left) == 0 &&
		atomic.LoadInt32(&s.consumed) == 0
}

// removeConsumed drops a cacher once its buffer was fully read, or the
// streams positioned in it have left, and no readers remain attached or
// pinned. The checks run under the ReaderCache lock so a reader attaching
// at the same time either wins (the cacher stays and that reader's detach
// retries the removal) or misses the map and refetches.
func (rc *ReaderCache) removeConsumed(downloader *SingleChunkCacher) {
	if downloader.shouldKeepUntilEvicted() || !downloader.consumable() {
		return
	}
	root := rc.rootOrSelf()
	root.Lock()
	removed := root.removeConsumedLocked(downloader)
	root.Unlock()
	if removed {
		downloader.destroy()
	}
}

func (rc *ReaderCache) removeConsumedLocked(downloader *SingleChunkCacher) (removed bool) {
	if downloader.shouldKeepUntilEvicted() {
		return false
	}
	if rc.downloaders[downloader.chunkFileId] == downloader && downloader.consumable() {
		delete(rc.downloaders, downloader.chunkFileId)
		return true
	}
	return false
}

func (rc *ReaderCache) destroy() {
	if rc == nil {
		return
	}
	root := rc.rootOrSelf()
	if rc == root {
		root.Lock()
		downloaders := root.downloaders
		root.downloaders = make(map[string]*SingleChunkCacher)
		root.Unlock()
		for _, downloader := range downloaders {
			downloader.destroy()
		}
		return
	}

	// Scoped ReaderCache: only destroy downloaders created by this scope that
	// are not currently read or pinned by another concurrent scope. If another
	// scope is actively reading/pinning a downloader, release this child
	// scope's budget reservation and transfer ownership to the root cache.
	var toDestroy []*SingleChunkCacher
	root.Lock()
	for fid, downloader := range root.downloaders {
		if downloader.scope != rc {
			continue
		}
		if atomic.LoadInt32(&downloader.readers) == 0 && atomic.LoadInt32(&downloader.pins) == 0 {
			delete(root.downloaders, fid)
			toDestroy = append(toDestroy, downloader)
		} else {
			if rc.budget != nil && rc.budget != root.budget {
				rc.budget.releaseSelf(downloader)
			}
			downloader.scope = root
			downloader.budget = root.budget
		}
	}
	root.Unlock()
	for _, downloader := range toDestroy {
		downloader.destroy()
	}
}

// ReleaseScope detaches a finished request/session scope from the shared root
// ReaderCache. Any completed chunks that should be retained in the shared root
// cache (when KeepUntilEvicted is enabled on the root cache) have their child
// scope budget reservation released while remaining tracked in the root budget;
// unpinned consumable chunks are cleaned up immediately.
func (rc *ReaderCache) ReleaseScope() {
	if rc == nil {
		return
	}
	root := rc.rootOrSelf()
	if rc == root {
		return
	}
	var toDestroy []*SingleChunkCacher
	root.Lock()
	for fid, downloader := range root.downloaders {
		if downloader.scope != rc {
			continue
		}
		if !root.keepUntilEvicted && downloader.consumable() {
			delete(root.downloaders, fid)
			toDestroy = append(toDestroy, downloader)
			continue
		}
		if rc.budget != nil && rc.budget != root.budget {
			rc.budget.releaseSelf(downloader)
		}
		downloader.scope = root
		downloader.budget = root.budget
	}
	root.Unlock()
	for _, downloader := range toDestroy {
		downloader.destroy()
	}
}

func newSingleChunkCacher(parent *ReaderCache, scope *ReaderCache, fileId string, cipherKey []byte, isGzipped bool, chunkSize int, shouldCache bool) *SingleChunkCacher {
	if scope == nil {
		scope = parent
	}
	return &SingleChunkCacher{
		parent:         parent,
		scope:          scope,
		budget:         scope.budget,
		chunkFileId:    fileId,
		cipherKey:      cipherKey,
		isGzipped:      isGzipped,
		chunkSize:      chunkSize,
		shouldCache:    shouldCache,
		cacheStartedCh: make(chan struct{}),
		done:           make(chan struct{}),
	}
}

// startCaching downloads a chunk shared by concurrent readers.
// The caller must s.wg.Add(1) before publishing the cacher in the
// downloaders map, so a removal can never observe the fetch as absent.
func (s *SingleChunkCacher) startCaching() {
	defer func() {
		hasErr := s.hasCompletedError()
		if !hasErr {
			s.budget.complete(s)
		}
		close(s.done)
		s.wg.Done()
		if hasErr {
			s.parent.remove(s)
		}
	}()

	s.cacheStartedCh <- struct{}{}
	if err := s.budget.reserve(s); err != nil {
		s.setError(err)
		return
	}

	// Intentionally use context.Background(), not a request-specific context.
	// The downloaded chunk is a shared resource: multiple concurrent readers may
	// wait on this same download via s.done. A request-scoped context that got
	// cancelled would abort the download and error every other waiting reader.
	// The download always runs to completion once started; readers that cancel
	// individually drop out via readChunkAt's select on ctx.Done().
	urlStrings, err := s.parent.lookupFileIdFn(context.Background(), s.chunkFileId)
	if err != nil {
		s.setError(fmt.Errorf("operation LookupFileId %s failed, err: %v", s.chunkFileId, err))
		return
	}
	if len(urlStrings) == 0 {
		s.setError(fmt.Errorf("operation LookupFileId %s failed, err: urls not found", s.chunkFileId))
		return
	}

	data, fetchErr := s.fetchChunkData(context.Background(), urlStrings)
	if fetchErr != nil {
		data, fetchErr = s.retryFetchAfterCacheInvalidation(context.Background(), urlStrings, fetchErr)
	}

	// Now acquire lock to update state
	s.Lock()
	if fetchErr != nil {
		s.err = fetchErr
	} else {
		s.data = data
		if s.shouldCache {
			s.parent.chunkCache.SetChunk(s.chunkFileId, s.data)
		}
	}
	atomic.StoreInt64(&s.completedTimeNew, time.Now().UnixNano())
	s.Unlock()
}

func (s *SingleChunkCacher) setError(err error) {
	s.Lock()
	defer s.Unlock()
	s.err = err
	atomic.StoreInt64(&s.completedTimeNew, time.Now().UnixNano())
}

func (s *SingleChunkCacher) hasCompletedError() bool {
	if atomic.LoadInt64(&s.completedTimeNew) == 0 {
		return false
	}
	s.Lock()
	defer s.Unlock()
	return s.err != nil
}

func (s *SingleChunkCacher) fetchChunkData(ctx context.Context, urlStrings []string) ([]byte, error) {
	// Allocate buffer and download without holding the lock.
	// This allows multiple downloads to proceed in parallel.
	data := mem.Allocate(s.chunkSize)
	fetchFn := s.parent.fetchChunkDataFn
	if s.scope != nil && s.scope.fetchChunkDataFn != nil {
		fetchFn = s.scope.fetchChunkDataFn
	}
	_, fetchErr := fetchFn(ctx, data, urlStrings, s.cipherKey, s.isGzipped, true, 0, s.chunkFileId, refreshUrls(ctx, s.parent.cacheInvalidator, s.parent.lookupFileIdFn, s.chunkFileId))
	if fetchErr != nil {
		mem.Free(data)
		return nil, fetchErr
	}
	return data, nil
}

func (s *SingleChunkCacher) retryFetchAfterCacheInvalidation(ctx context.Context, oldUrlStrings []string, originalErr error) ([]byte, error) {
	var data []byte
	err := retryFetchWithFreshLocations(ctx, s.parent.cacheInvalidator, s.parent.lookupFileIdFn, s.chunkFileId, oldUrlStrings, originalErr, func(newUrls []string) error {
		var fetchErr error
		data, fetchErr = s.fetchChunkData(ctx, newUrls)
		return fetchErr
	})
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (s *SingleChunkCacher) destroy() {
	// wait for all reads to finish before destroying the data
	s.wg.Wait()
	s.Lock()
	if s.data != nil {
		mem.Free(s.data)
		s.data = nil
	}
	s.Unlock()
	s.budget.release(s)
}

// readChunkAt reads data from the cached chunk.
// It waits for the download to complete if it's still in progress.
// The ctx parameter allows the reader to cancel its wait (but the download continues
// for other readers - see comment in startCaching about shared resource semantics).
// The caller must s.wg.Add(1) under the ReaderCache lock before calling; this only releases it.
func (s *SingleChunkCacher) readChunkAt(ctx context.Context, buf []byte, offset int64) (n int, err error) {
	defer func() {
		s.wg.Done()
		atomic.AddInt32(&s.readers, -1)
		s.parent.removeConsumed(s)
	}()

	// Wait for download to complete, but allow reader cancellation.
	// Prioritize checking done first - if data is already available,
	// return it even if context is also cancelled.
	select {
	case <-s.done:
		// Download already completed, proceed immediately
	default:
		// Download not complete, wait for it or context cancellation
		select {
		case <-s.done:
			// Download completed
		case <-ctx.Done():
			// Reader cancelled while waiting - download continues for other readers
			return 0, ctx.Err()
		}
	}

	s.Lock()
	defer s.Unlock()

	if s.err != nil {
		return 0, s.err
	}

	if len(s.data) <= int(offset) {
		return 0, nil
	}

	n = copy(buf, s.data[offset:])
	if offset+int64(n) == int64(len(s.data)) {
		atomic.StoreInt32(&s.consumed, 1)
	}
	return n, nil
}
