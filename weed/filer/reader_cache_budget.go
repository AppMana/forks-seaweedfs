package filer

import (
	"container/list"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/seaweedfs/seaweedfs/weed/util/mem"
)

var errPrefetchBudgetBusy = errors.New("prefetch skipped: reader cache budget is busy")

const DefaultReaderCacheMemoryLimit = 256 << 20
const DefaultSessionReaderCacheMemoryLimit = 64 << 20

type ReaderCacheBudget struct {
	sync.Mutex
	parent       *ReaderCacheBudget
	limit        int64
	used         int64
	reservations map[*SingleChunkCacher]int64
	idle         list.List
	idleEntries  map[*SingleChunkCacher]*list.Element
	changed      chan struct{}
}

func NewReaderCacheBudget(limit int64) *ReaderCacheBudget {
	if limit <= 0 {
		limit = DefaultReaderCacheMemoryLimit
	}
	return &ReaderCacheBudget{
		limit:        limit,
		reservations: make(map[*SingleChunkCacher]int64),
		idleEntries:  make(map[*SingleChunkCacher]*list.Element),
		changed:      make(chan struct{}),
	}
}

// NewScopedReaderCacheBudget creates a per-request / per-session budget scoped
// under a parent whole-cache budget. When the child scope reaches its own
// limit, it evicts its own idle chunks first before contending on the parent
// whole-cache budget.
func NewScopedReaderCacheBudget(parent *ReaderCacheBudget, limit int64) *ReaderCacheBudget {
	if parent == nil && limit <= 0 {
		return nil
	}
	if limit <= 0 {
		if parent != nil {
			limit = parent.limit
		} else {
			limit = DefaultSessionReaderCacheMemoryLimit
		}
	}
	if parent != nil && limit > parent.limit {
		limit = parent.limit
	}
	return &ReaderCacheBudget{
		parent:       parent,
		limit:        limit,
		reservations: make(map[*SingleChunkCacher]int64),
		idleEntries:  make(map[*SingleChunkCacher]*list.Element),
		changed:      make(chan struct{}),
	}
}

// NewScope creates a child budget scoped under b.
func (b *ReaderCacheBudget) NewScope(limit int64) *ReaderCacheBudget {
	return NewScopedReaderCacheBudget(b, limit)
}

// Limit returns the byte limit of this budget scope.
func (b *ReaderCacheBudget) Limit() int64 {
	if b == nil {
		return 0
	}
	return b.limit
}

func (b *ReaderCacheBudget) reserve(s *SingleChunkCacher) error {
	if s.chunkSize < 0 {
		return fmt.Errorf("invalid chunk size %d", s.chunkSize)
	}
	if b == nil {
		return nil
	}
	size := int64(mem.AllocationSize(s.chunkSize))
	if !b.canFit(s.chunkSize) {
		return fmt.Errorf("chunk buffer needs %d bytes, exceeding reader cache budget %d; increase the readerCacheSizeMB budget", size, b.effectiveLimit())
	}
	if err := b.reserveSelf(s, size); err != nil {
		return err
	}
	if b.parent != nil {
		if err := b.parent.reserve(s); err != nil {
			b.releaseSelf(s)
			return err
		}
	}
	return nil
}

func (b *ReaderCacheBudget) effectiveLimit() int64 {
	if b == nil {
		return 0
	}
	limit := b.limit
	if b.parent != nil && b.parent.effectiveLimit() < limit {
		limit = b.parent.effectiveLimit()
	}
	return limit
}

func (b *ReaderCacheBudget) reserveSelf(s *SingleChunkCacher, size int64) error {
	if size > b.limit {
		return fmt.Errorf("chunk buffer needs %d bytes, exceeding reader cache budget %d; increase the readerCacheSizeMB budget", size, b.limit)
	}
	for {
		b.Lock()
		if size <= b.limit-b.used {
			b.used += size
			b.reservations[s] = size
			b.Unlock()
			return nil
		}
		// Prefer evicting an unpinned chunk that has already been read (FIFO from
		// Front). If only unread prefetches are available, evict the newest
		// (furthest-ahead) unread prefetch from Back, and never let a background
		// prefetch evict an unread prefetch of the same request scope. A pinned
		// chunk is taken only by a demand read and only when no download is in
		// flight to free space: then nothing else will, and streams that each
		// hold a pin would wait on one another forever.
		var victim *SingleChunkCacher
		var entry *list.Element
		pinnedVictim := false
		for e := b.idle.Front(); e != nil; e = e.Next() {
			c := e.Value.(*SingleChunkCacher)
			if atomic.LoadInt32(&c.pins) == 0 && atomic.LoadInt32(&c.readers) == 0 && !c.isUnreadPrefetch() {
				victim, entry = c, e
				break
			}
		}
		if entry == nil {
			var sameScopeVictim *SingleChunkCacher
			var sameScopeEntry *list.Element
			for e := b.idle.Back(); e != nil; e = e.Prev() {
				c := e.Value.(*SingleChunkCacher)
				if atomic.LoadInt32(&c.pins) != 0 || atomic.LoadInt32(&c.readers) != 0 {
					continue
				}
				if s == nil || c.scope != s.scope {
					victim, entry = c, e
					break
				}
				if sameScopeEntry == nil {
					sameScopeVictim, sameScopeEntry = c, e
				}
			}
			if entry == nil && sameScopeEntry != nil {
				victim, entry = sameScopeVictim, sameScopeEntry
			}
		}
		if entry == nil && (s == nil || !s.isPrefetch) && len(b.reservations) == b.idle.Len() {
			for e := b.idle.Front(); e != nil; e = e.Next() {
				c := e.Value.(*SingleChunkCacher)
				if atomic.LoadInt32(&c.readers) == 0 {
					victim, entry = c, e
					pinnedVictim = true
					break
				}
			}
		}
		if entry != nil {
			b.idle.Remove(entry)
			delete(b.idleEntries, victim)
			b.Unlock()
			if pinnedVictim {
				victim.parent.remove(victim)
			} else if !victim.parent.removeUnpinned(victim) {
				// The victim was pinned between selection and removal: keep it
				// in the idle list so it becomes evictable again once unpinned,
				// then retry the selection.
				b.Lock()
				if _, ok := b.reservations[victim]; ok && b.idleEntries[victim] == nil {
					b.idleEntries[victim] = b.idle.PushBack(victim)
				}
				b.Unlock()
			}
			continue
		}
		if s != nil && s.isPrefetch && b.idle.Len() > 0 {
			b.Unlock()
			return errPrefetchBudgetBusy
		}
		changed := b.changed
		b.Unlock()
		<-changed
	}
}

// canFit reports whether a whole-chunk buffer of this size can ever be
// reserved. A chunk bigger than the budget cannot be read through the
// whole-chunk path at all, so callers must fall back to range fetches.
func (b *ReaderCacheBudget) canFit(size int) bool {
	if b == nil {
		return true
	}
	if int64(mem.AllocationSize(size)) > b.limit {
		return false
	}
	return b.parent.canFit(size)
}

func (b *ReaderCacheBudget) complete(s *SingleChunkCacher) {
	if b == nil {
		return
	}
	b.Lock()
	if _, found := b.reservations[s]; found && b.idleEntries[s] == nil {
		b.idleEntries[s] = b.idle.PushBack(s)
		close(b.changed)
		b.changed = make(chan struct{})
	}
	b.Unlock()
	if b.parent != nil {
		b.parent.complete(s)
	}
}

func (b *ReaderCacheBudget) releaseSelf(s *SingleChunkCacher) {
	if b == nil {
		return
	}
	b.Lock()
	defer b.Unlock()
	if size, found := b.reservations[s]; found {
		b.used -= size
		delete(b.reservations, s)
		if entry := b.idleEntries[s]; entry != nil {
			b.idle.Remove(entry)
			delete(b.idleEntries, s)
		}
		close(b.changed)
		b.changed = make(chan struct{})
	}
}

func (b *ReaderCacheBudget) release(s *SingleChunkCacher) {
	if b == nil {
		return
	}
	b.releaseSelf(s)
	if b.parent != nil {
		b.parent.release(s)
	}
}

func (b *ReaderCacheBudget) notifyChanged() {
	if b == nil {
		return
	}
	b.Lock()
	close(b.changed)
	b.changed = make(chan struct{})
	b.Unlock()
	if b.parent != nil {
		b.parent.notifyChanged()
	}
}
