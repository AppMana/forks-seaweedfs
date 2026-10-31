package mount

import (
	"container/list"
	"strings"
	"sync"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/util"
)

const (
	// winfspCacheTTL is how long the WinFsp adapter keeps a file's lookup
	// reference after its last use, and how long cached attributes answer.
	winfspCacheTTL = time.Second

	// winfspMaxCachedPaths bounds the references the path cache holds.
	winfspMaxCachedPaths = 64 << 10

	// winfspMaxRecentPurges bounds the purges remembered for discarding
	// attributes read before one. An older read is discarded outright,
	// which only costs the next Getattr a miss.
	winfspMaxRecentPurges = 128
)

// winfspPathCache stands in for the dentry cache the kernel keeps for a FUSE
// mount. WinFsp addresses every operation by path and never sends FORGET, so
// the adapter itself decides when a lookup reference goes back. The cache
// owns one reference per entry and returns it when the entry goes unused for
// its retention, is purged, or is the least recently used past capacity.
//
// Each entry can also carry the attributes last read for its path, the
// adapter's stand-in for the kernel attribute cache. They answer for
// winfspCacheTTL unless a purge drops them first.
//
// Directories are kept for as long as the mount keeps an idle directory's
// listing (-dirIdleEvictSec), because the last Forget of a directory purges
// its cached listing. Files are kept for winfspCacheTTL.
//
// A reference on a busy inode, one with an open or draining handle or a
// pending async flush, is held back rather than returned when its entry goes:
// the flush commits metadata only while the saved path still maps to the
// inode, and the last Forget would unmap it. Held-back references are
// returned by a later sweep once the inode is idle.
type winfspPathCache[S any] struct {
	fileTTL  time.Duration
	dirTTL   time.Duration // 0 keeps directories until capacity pushes them out
	capacity int
	retain   func(inode uint64) bool
	forget   func(inode uint64)
	busy     func(inode uint64) bool

	mu        sync.Mutex
	entries   map[util.FullPath]*list.Element
	order     *list.List // of *winfspPathEntry[S], most recently used first
	heldBack  map[uint64]int
	lastSweep time.Time
	// gen counts purges. Attributes are read without the lock, so a purge
	// can land between the read and the store; recentPurges says whether one
	// covered the key, so only that store is discarded.
	gen          uint64
	recentPurges []winfspPurge
}

type winfspPurge struct {
	key    util.FullPath
	prefix bool
}

func (p winfspPurge) covers(key util.FullPath) bool {
	if p.key == key {
		return true
	}
	return p.prefix && strings.HasPrefix(string(key), strings.TrimSuffix(string(p.key), "/")+"/")
}

type winfspPathEntry[S any] struct {
	key     util.FullPath
	inode   uint64
	dir     bool
	expires time.Time // zero never expires

	stat        S
	statExpires time.Time // zero: no attributes cached
}

// newWinfspPathCache builds a cache whose references are taken with retain
// and returned with forget. busy reports an inode whose reference must not be
// returned yet.
func newWinfspPathCache[S any](fileTTL, dirTTL time.Duration, capacity int, retain func(uint64) bool, forget func(uint64), busy func(uint64) bool) *winfspPathCache[S] {
	return &winfspPathCache[S]{
		fileTTL:  fileTTL,
		dirTTL:   dirTTL,
		capacity: capacity,
		retain:   retain,
		forget:   forget,
		busy:     busy,
		entries:  make(map[util.FullPath]*list.Element),
		order:    list.New(),
		heldBack: make(map[uint64]int),
	}
}

func (c *winfspPathCache[S]) expiry(dir bool, now time.Time) time.Time {
	ttl := c.fileTTL
	if dir {
		ttl = c.dirTTL
	}
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

// hold makes the cache own a reference on inode under key, taking a new one
// with retain unless it already owns one there.
func (c *winfspPathCache[S]) hold(key util.FullPath, inode uint64, dir bool, now time.Time) {
	c.mu.Lock()
	if element, found := c.entries[key]; found && element.Value.(*winfspPathEntry[S]).inode == inode {
		c.useLocked(element, now)
		c.mu.Unlock()
		c.maybeSweep(now)
		return
	}
	if !c.retain(inode) {
		// Gone from the table already: there is nothing to keep alive.
		c.mu.Unlock()
		return
	}
	pending := c.putLocked(key, inode, dir, now)
	c.mu.Unlock()
	c.giveBack(pending)
	c.maybeSweep(now)
}

// adopt hands the caller's reference on inode to the cache under key.
func (c *winfspPathCache[S]) adopt(key util.FullPath, inode uint64, dir bool, now time.Time) {
	c.mu.Lock()
	var pending []uint64
	if element, found := c.entries[key]; found && element.Value.(*winfspPathEntry[S]).inode == inode {
		c.useLocked(element, now)
		pending = append(pending, inode) // already owned there; this one is spare
	} else {
		pending = c.putLocked(key, inode, dir, now)
	}
	c.mu.Unlock()
	c.giveBack(pending)
	c.maybeSweep(now)
}

// release returns a reference the caller owns, holding it back while the
// inode is busy.
func (c *winfspPathCache[S]) release(inode uint64) {
	c.giveBack([]uint64{inode})
}

// touch extends the retention of the entry under key, if there is one.
func (c *winfspPathCache[S]) touch(key util.FullPath, now time.Time) {
	c.mu.Lock()
	if element, found := c.entries[key]; found {
		c.useLocked(element, now)
	}
	c.mu.Unlock()
	c.maybeSweep(now)
}

// purge drops key, and with prefix everything beneath it: a renamed or
// removed directory takes its children's names with it.
func (c *winfspPathCache[S]) purge(key util.FullPath, prefix bool) []uint64 {
	c.mu.Lock()
	c.recordPurgeLocked(key, prefix)
	var pending []uint64
	if element, found := c.entries[key]; found {
		pending = append(pending, c.removeLocked(element))
	}
	if prefix {
		under := strings.TrimSuffix(string(key), "/") + "/"
		for k, element := range c.entries {
			if strings.HasPrefix(string(k), under) {
				pending = append(pending, c.removeLocked(element))
			}
		}
	}
	c.mu.Unlock()
	c.giveBack(pending)
	return pending
}

// forgetStat drops the attributes cached for key, keeping its reference.
func (c *winfspPathCache[S]) forgetStat(key util.FullPath) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordPurgeLocked(key, false)
	if element, found := c.entries[key]; found {
		element.Value.(*winfspPathEntry[S]).statExpires = time.Time{}
	}
}

// stat returns the attributes cached for key and the inode they belong to.
func (c *winfspPathCache[S]) stat(key util.FullPath, now time.Time) (uint64, S, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero S
	element, found := c.entries[key]
	if !found {
		return 0, zero, false
	}
	entry := element.Value.(*winfspPathEntry[S])
	if entry.statExpires.IsZero() || !now.Before(entry.statExpires) {
		return 0, zero, false
	}
	return entry.inode, entry.stat, true
}

// generation snapshots the purge count before attributes are read; setStat
// discards them if a purge covering their key ran since.
func (c *winfspPathCache[S]) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// setStat caches attributes read for inode under key, entering the path with
// a reference of its own if it is not cached yet.
func (c *winfspPathCache[S]) setStat(key util.FullPath, inode uint64, dir bool, stat S, gen uint64, now time.Time) {
	c.mu.Lock()
	if c.purgedSinceLocked(gen, key) {
		c.mu.Unlock()
		return
	}
	var pending []uint64
	element, found := c.entries[key]
	if !found || element.Value.(*winfspPathEntry[S]).inode != inode {
		if !c.retain(inode) {
			c.mu.Unlock()
			return
		}
		pending = c.putLocked(key, inode, dir, now)
		element = c.entries[key]
	}
	entry := element.Value.(*winfspPathEntry[S])
	entry.stat = stat
	entry.statExpires = now.Add(c.fileTTL)
	c.mu.Unlock()
	c.giveBack(pending)
}

func (c *winfspPathCache[S]) recordPurgeLocked(key util.FullPath, prefix bool) {
	c.gen++
	c.recentPurges = append(c.recentPurges, winfspPurge{key: key, prefix: prefix})
	if len(c.recentPurges) > winfspMaxRecentPurges {
		c.recentPurges = append(c.recentPurges[:0], c.recentPurges[len(c.recentPurges)-winfspMaxRecentPurges:]...)
	}
}

func (c *winfspPathCache[S]) purgedSinceLocked(gen uint64, key util.FullPath) bool {
	since := c.gen - gen
	if since == 0 {
		return false
	}
	if since > uint64(len(c.recentPurges)) {
		return true
	}
	for _, purge := range c.recentPurges[uint64(len(c.recentPurges))-since:] {
		if purge.covers(key) {
			return true
		}
	}
	return false
}

// sweep returns the references of entries unused past their retention, and
// held-back references whose inode is no longer busy.
func (c *winfspPathCache[S]) sweep(now time.Time) {
	c.mu.Lock()
	c.lastSweep = now
	var pending []uint64
	for element := c.order.Back(); element != nil; {
		previous := element.Prev()
		entry := element.Value.(*winfspPathEntry[S])
		if !entry.expires.IsZero() && now.After(entry.expires) {
			pending = append(pending, c.removeLocked(element))
		}
		element = previous
	}
	for inode, count := range c.heldBack {
		delete(c.heldBack, inode)
		for ; count > 0; count-- {
			pending = append(pending, inode)
		}
	}
	c.mu.Unlock()
	c.giveBack(pending)
}

// maybeSweep sweeps at most once per file retention, piggybacking on traffic.
func (c *winfspPathCache[S]) maybeSweep(now time.Time) {
	c.mu.Lock()
	due := now.Sub(c.lastSweep) >= c.fileTTL
	c.mu.Unlock()
	if due {
		c.sweep(now)
	}
}

func (c *winfspPathCache[S]) useLocked(element *list.Element, now time.Time) {
	entry := element.Value.(*winfspPathEntry[S])
	entry.expires = c.expiry(entry.dir, now)
	c.order.MoveToFront(element)
}

// putLocked stores an owned reference under key and returns the references
// it displaced: a previous inode under the same key, and the least recently
// used entries past capacity.
func (c *winfspPathCache[S]) putLocked(key util.FullPath, inode uint64, dir bool, now time.Time) (pending []uint64) {
	if element, found := c.entries[key]; found {
		pending = append(pending, c.removeLocked(element))
	}
	entry := &winfspPathEntry[S]{key: key, inode: inode, dir: dir, expires: c.expiry(dir, now)}
	c.entries[key] = c.order.PushFront(entry)
	for c.order.Len() > c.capacity {
		pending = append(pending, c.removeLocked(c.order.Back()))
	}
	return pending
}

func (c *winfspPathCache[S]) removeLocked(element *list.Element) uint64 {
	entry := element.Value.(*winfspPathEntry[S])
	c.order.Remove(element)
	delete(c.entries, entry.key)
	return entry.inode
}

// giveBack returns references outside the cache lock: Forget can purge a
// directory's cached listing, and busy reads the handle map.
func (c *winfspPathCache[S]) giveBack(inodes []uint64) {
	for _, inode := range inodes {
		if c.busy(inode) {
			c.mu.Lock()
			c.heldBack[inode]++
			c.mu.Unlock()
			continue
		}
		c.forget(inode)
	}
}

// size reports how many references the cache owns, held back included.
func (c *winfspPathCache[S]) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.order.Len()
	for _, count := range c.heldBack {
		n += count
	}
	return n
}
