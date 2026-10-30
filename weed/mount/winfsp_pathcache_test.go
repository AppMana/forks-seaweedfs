package mount

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/util"
)

// fakeRefs counts lookup references the way the inode table does.
type fakeRefs struct {
	mu     sync.Mutex
	counts map[uint64]int
	busy   map[uint64]bool
}

func newFakeRefs() *fakeRefs {
	return &fakeRefs{counts: map[uint64]int{}, busy: map[uint64]bool{}}
}

func (f *fakeRefs) lookup(inode uint64) {
	f.mu.Lock()
	f.counts[inode]++
	f.mu.Unlock()
}

func (f *fakeRefs) retain(inode uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts[inode] == 0 {
		return false
	}
	f.counts[inode]++
	return true
}

func (f *fakeRefs) forget(inode uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.counts[inode] == 0 {
		panic(fmt.Sprintf("forget of inode %d with no references", inode))
	}
	f.counts[inode]--
	if f.counts[inode] == 0 {
		delete(f.counts, inode)
	}
}

func (f *fakeRefs) isBusy(inode uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy[inode]
}

func (f *fakeRefs) setBusy(inode uint64, busy bool) {
	f.mu.Lock()
	f.busy[inode] = busy
	f.mu.Unlock()
}

func (f *fakeRefs) count(inode uint64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[inode]
}

func (f *fakeRefs) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.counts {
		n += c
	}
	return n
}

func newTestPathCache(refs *fakeRefs, capacity int) *winfspPathCache[int] {
	return newWinfspPathCache[int](time.Second, time.Minute, capacity, refs.retain, refs.forget, refs.isBusy)
}

func TestWinfspPathCacheHoldsOneReferencePerEntry(t *testing.T) {
	refs := newFakeRefs()
	c := newTestPathCache(refs, 16)
	now := time.Now()

	refs.lookup(10) // the caller's own reference
	c.hold("/a", 10, false, now)
	c.hold("/a", 10, false, now)
	if got := refs.count(10); got != 2 {
		t.Fatalf("references = %d after holding twice, want the caller's and one for the cache", got)
	}
	refs.forget(10)

	// Adopting a reference the cache already has for the key returns it.
	refs.lookup(10)
	c.adopt("/a", 10, false, now)
	if got := refs.count(10); got != 1 {
		t.Fatalf("references = %d after adopting a duplicate, want 1", got)
	}

	// A different inode under the key replaces the old one.
	refs.lookup(11)
	c.adopt("/a", 11, false, now)
	if refs.count(10) != 0 || refs.count(11) != 1 {
		t.Fatalf("references = %d and %d after replacing, want 0 and 1", refs.count(10), refs.count(11))
	}

	// A gone inode is not entered.
	c.hold("/gone", 99, false, now)
	if c.size() != 1 {
		t.Fatalf("cache size = %d, want 1", c.size())
	}
}

func TestWinfspPathCacheExpiresUnusedEntries(t *testing.T) {
	refs := newFakeRefs()
	c := newTestPathCache(refs, 16)
	start := time.Now()
	refs.lookup(1001)
	c.adopt("/file", 1001, false, start)
	refs.lookup(1002)
	c.adopt("/dir", 1002, true, start)

	// Use keeps an entry alive past its original retention.
	c.touch("/file", start.Add(900*time.Millisecond))
	c.sweep(start.Add(1500 * time.Millisecond))
	if refs.count(1001) != 1 {
		t.Fatal("a file used within its retention was returned")
	}
	c.sweep(start.Add(2 * time.Second))
	if refs.count(1001) != 0 {
		t.Fatal("an unused file outlived its retention")
	}
	if refs.count(1002) != 1 {
		t.Fatal("a directory went with the file retention")
	}
	c.sweep(start.Add(2 * time.Minute))
	if refs.count(1002) != 0 {
		t.Fatal("an unused directory outlived its retention")
	}
}

func TestWinfspPathCacheEvictsLeastRecentlyUsedPastCapacity(t *testing.T) {
	refs := newFakeRefs()
	c := newTestPathCache(refs, 3)
	now := time.Now()
	for i := uint64(1); i <= 3; i++ {
		refs.lookup(i)
		c.adopt(util.FullPath(fmt.Sprintf("/%d", i)), i, false, now)
	}
	c.touch("/1", now)
	refs.lookup(4)
	c.adopt("/4", 4, false, now)
	if refs.count(2) != 0 {
		t.Fatal("the least recently used entry was not pushed out")
	}
	for _, i := range []uint64{1, 3, 4} {
		if refs.count(i) != 1 {
			t.Fatalf("inode %d lost its reference", i)
		}
	}
}

func TestWinfspPathCachePurgesSubtrees(t *testing.T) {
	refs := newFakeRefs()
	c := newTestPathCache(refs, 16)
	now := time.Now()
	for i, key := range []util.FullPath{"/d", "/d/a", "/d/a/b", "/dd", "/e"} {
		refs.lookup(uint64(i + 1))
		c.adopt(key, uint64(i+1), false, now)
	}
	c.purge("/d", true)
	if refs.total() != 2 || refs.count(4) != 1 || refs.count(5) != 1 {
		t.Fatalf("after purging /d: %v, want only /dd and /e", refs.counts)
	}
	c.purge("/e", false)
	c.purge("/missing", true)
	if refs.total() != 1 {
		t.Fatalf("after purging /e: %v", refs.counts)
	}
	c.purge("/", true)
	if refs.total() != 0 || c.size() != 0 {
		t.Fatalf("after purging the root: %v", refs.counts)
	}
}

// A busy inode keeps its reference after its entry goes, until a sweep finds
// it idle.
func TestWinfspPathCacheHoldsBackReferencesOfBusyInodes(t *testing.T) {
	refs := newFakeRefs()
	c := newTestPathCache(refs, 1)
	now := time.Now()
	refs.lookup(7)
	c.adopt("/busy", 7, false, now)
	refs.setBusy(7, true)

	c.purge("/busy", false)
	refs.lookup(7)
	c.release(7)
	refs.lookup(7)
	c.adopt("/again", 7, false, now)
	refs.lookup(8)
	c.adopt("/other", 8, false, now) // pushes /again out past capacity
	c.sweep(now.Add(time.Hour))
	if got := refs.count(7); got != 3 {
		t.Fatalf("busy inode has %d references, want all 3 held back", got)
	}
	if c.size() != 3 {
		t.Fatalf("cache owns %d references, want the 3 held back", c.size())
	}

	refs.setBusy(7, false)
	c.sweep(now.Add(2 * time.Hour))
	if refs.total() != 0 || c.size() != 0 {
		t.Fatalf("references left after the inode went idle: %v", refs.counts)
	}
}

func TestWinfspPathCacheConcurrentUse(t *testing.T) {
	refs := newFakeRefs()
	c := newTestPathCache(refs, 64)
	start := time.Now()
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				inode := uint64(i%200 + 1)
				key := util.FullPath(fmt.Sprintf("/k%d", inode))
				now := start.Add(time.Duration(i) * time.Millisecond)
				refs.lookup(inode)
				switch (worker + i) % 5 {
				case 0:
					c.adopt(key, inode, i%2 == 0, now)
				case 1:
					c.hold(key, inode, false, now)
					refs.forget(inode)
				case 2:
					c.touch(key, now)
					refs.forget(inode)
				case 3:
					c.purge(key, i%3 == 0)
					refs.forget(inode)
				default:
					c.release(inode)
				}
			}
		}(worker)
	}
	wg.Wait()
	c.sweep(start.Add(time.Hour * 24))
	if refs.total() != 0 || c.size() != 0 {
		t.Fatalf("references left after the final sweep: %d (cache %d)", refs.total(), c.size())
	}
}

func TestWinfspPathCacheServesAttributesWithinTheirTTL(t *testing.T) {
	refs := newFakeRefs()
	c := newTestPathCache(refs, 16)
	now := time.Now()

	// Attributes for a path not cached yet enter it with a reference.
	refs.lookup(5)
	c.setStat("/f", 5, false, 42, c.generation(), now)
	refs.forget(5)
	if refs.count(5) != 1 {
		t.Fatalf("references = %d, want the cache's own", refs.count(5))
	}
	if inode, stat, ok := c.stat("/f", now.Add(999*time.Millisecond)); !ok || inode != 5 || stat != 42 {
		t.Fatalf("stat = %d, %d, %v within the TTL", inode, stat, ok)
	}
	if _, _, ok := c.stat("/f", now.Add(time.Second)); ok {
		t.Fatal("attributes answered past their TTL")
	}

	c.setStat("/f", 5, false, 43, c.generation(), now)
	c.forgetStat("/f")
	if _, _, ok := c.stat("/f", now); ok {
		t.Fatal("attributes answered after forgetStat")
	}
	if refs.count(5) != 1 {
		t.Fatal("forgetStat returned the reference")
	}
}

// Attributes read before a purge of their key must not be stored after it.
func TestWinfspPathCacheDiscardsAttributesReadBeforeAPurge(t *testing.T) {
	refs := newFakeRefs()
	c := newTestPathCache(refs, 16)
	now := time.Now()
	refs.lookup(5)
	c.adopt("/d/f", 5, false, now)

	for _, purge := range []func(){
		func() { c.forgetStat("/d/f") },
		func() { c.purge("/d/f", false) },
		func() { c.purge("/d", true) },
	} {
		gen := c.generation()
		purge()
		refs.lookup(5)
		c.setStat("/d/f", 5, false, 1, gen, now)
		refs.forget(5)
		if _, _, ok := c.stat("/d/f", now); ok {
			t.Fatal("attributes read before a purge were stored")
		}
	}

	// A purge of another key does not.
	gen := c.generation()
	c.forgetStat("/d/g")
	c.purge("/dd", true)
	refs.lookup(5)
	c.setStat("/d/f", 5, false, 2, gen, now)
	refs.forget(5)
	if _, stat, ok := c.stat("/d/f", now); !ok || stat != 2 {
		t.Fatal("an unrelated purge discarded attributes")
	}

	// Past the remembered window every older read is discarded.
	gen = c.generation()
	for i := 0; i <= winfspMaxRecentPurges; i++ {
		c.forgetStat(util.FullPath(fmt.Sprintf("/other/%d", i)))
	}
	c.forgetStat("/d/f")
	c.setStat("/d/f", 5, false, 3, gen, now)
	if _, _, ok := c.stat("/d/f", now); ok {
		t.Fatal("a read older than the purge window was stored")
	}
}
