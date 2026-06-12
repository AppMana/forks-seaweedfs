package mount

import (
	"sync"
	"sync/atomic"
)

// Per-handle sequential read-ahead for the WinFsp adapter.
//
// Without kernel data caching (FileInfoTimeout=-1 is unsafe on shared
// volumes, see winfsp_host_windows.go), every application ReadFile
// crosses kernel->user and walks the full WFS read path; at 4KB that
// path dominates (~100µs/op, ~30 MB/s). Buffering one read-ahead window
// per open handle turns 256 WFS traversals per MB into one; the per-op
// cost becomes a memcpy.
//
// Consistency: the buffer lives only within one open handle and never
// outlives it (close-to-open semantics unchanged). Writes, truncates
// and renames on the same inode bump a generation counter that
// invalidates all buffers for that inode, so same-mount readers never
// see stale data. Cross-node staleness remains governed by the weed
// entry TTLs exactly as before (the buffer only holds data the WFS
// read path itself just returned).
const (
	readAheadSize       = 1 << 20 // 1MB window
	readAheadMaxBuffers = 64      // global cap; beyond it reads bypass
	// requests at or above this size gain nothing from buffering
	readAheadBypassSize = 256 << 10
)

type readAheadBuffer struct {
	mu     sync.Mutex
	inode  uint64
	gen    uint64 // inode generation the window was read under
	off    int64  // file offset of buf[0]
	buf    []byte // valid window is buf[:len(buf)]
	seqEnd int64  // end offset of the previous read (sequential detector)
}

type readAheadCache struct {
	mu       sync.Mutex
	byFh     map[uint64]*readAheadBuffer
	inodeGen sync.Map // inode -> *atomic.Uint64
}

func newReadAheadCache() *readAheadCache {
	return &readAheadCache{byFh: make(map[uint64]*readAheadBuffer)}
}

func (c *readAheadCache) generation(inode uint64) *atomic.Uint64 {
	if v, ok := c.inodeGen.Load(inode); ok {
		return v.(*atomic.Uint64)
	}
	v, _ := c.inodeGen.LoadOrStore(inode, &atomic.Uint64{})
	return v.(*atomic.Uint64)
}

// Invalidate marks all buffered windows for inode stale (called on
// write/truncate/rename paths).
func (c *readAheadCache) Invalidate(inode uint64) {
	c.generation(inode).Add(1)
}

// Release drops the buffer for a closing handle.
func (c *readAheadCache) Release(fh uint64) {
	c.mu.Lock()
	delete(c.byFh, fh)
	c.mu.Unlock()
}

// buffer returns (creating if needed) the read-ahead state for fh, or
// nil when the global cap is reached.
func (c *readAheadCache) buffer(fh, inode uint64) *readAheadBuffer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.byFh[fh]; ok {
		return b
	}
	if len(c.byFh) >= readAheadMaxBuffers {
		return nil
	}
	b := &readAheadBuffer{inode: inode}
	c.byFh[fh] = b
	return b
}

// Read serves dst at ofst from the read-ahead window when possible.
// fetch reads from the underlying filesystem (must behave like the
// adapter's direct WFS read: returns n bytes read, negative errno on
// failure). Returns the cgofuse-style result.
func (c *readAheadCache) Read(fh, inode uint64, dst []byte, ofst int64, fetch func(dst []byte, ofst int64) int) int {
	if len(dst) >= readAheadBypassSize {
		return fetch(dst, ofst)
	}
	b := c.buffer(fh, inode)
	if b == nil {
		return fetch(dst, ofst)
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	gen := c.generation(inode).Load()
	if b.gen != gen {
		b.buf = nil // window predates a mutation on this inode
	}

	// fast path: fully inside the window
	if b.buf != nil && ofst >= b.off && ofst+int64(len(dst)) <= b.off+int64(len(b.buf)) {
		copy(dst, b.buf[ofst-b.off:])
		b.seqEnd = ofst + int64(len(dst))
		return len(dst)
	}

	sequential := ofst == b.seqEnd || (b.buf != nil && ofst >= b.off && ofst < b.off+int64(len(b.buf)))
	b.seqEnd = ofst + int64(len(dst))
	if !sequential {
		// random access: do not pollute the window
		return fetch(dst, ofst)
	}

	// refill the window at ofst
	if cap(b.buf) < readAheadSize {
		b.buf = make([]byte, readAheadSize)
	}
	window := b.buf[:readAheadSize]
	n := fetch(window, ofst)
	if n < 0 {
		b.buf = nil
		return n
	}
	b.buf = window[:n]
	b.off = ofst
	b.gen = gen
	if n == 0 {
		return 0
	}
	served := len(dst)
	if served > n {
		served = n
	}
	copy(dst, b.buf[:served])
	return served
}
