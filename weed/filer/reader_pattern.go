package filer

import (
	"sync/atomic"
)

type ReaderPattern struct {
	isSequentialCounter int64
	highWaterMark       int64
	// forced, when non-nil, makes IsRandomMode() return this value directly,
	// bypassing isSequentialCounter/MonitorReadAt entirely. Set via
	// NewReaderPatternWithMode -- see ReaderCacheMode for how this is
	// threaded from the weed mount -readerCacheMode flag down to here.
	forced *bool
}

// ReaderCacheMode names the values accepted by the -readerCacheMode weed
// mount flag (see weed/command/mount.go) and threaded through
// ChunkGroup/ChunkReadAt down to NewReaderPatternWithMode. "auto" preserves
// the inferred (MonitorReadAt-driven) behavior; "sequential"/"random" force
// IsRandomMode() to a fixed value, letting a known workload (e.g. an
// mmap-heavy ML checkpoint mount) declare its access pattern explicitly
// instead of relying on the heuristic to infer it correctly.
type ReaderCacheMode string

const (
	ReaderCacheModeAuto       ReaderCacheMode = "auto"
	ReaderCacheModeSequential ReaderCacheMode = "sequential"
	ReaderCacheModeRandom     ReaderCacheMode = "random"
)

const ModeChangeLimit = 3

// offsetTolerance bounds how far behind the high-water-mark a request's
// offset may fall without counting against sequential classification. FUSE
// serves reads under a shared (not exclusive) per-handle lock, so multiple
// requests can be in flight concurrently on one handle, and the kernel's
// own page-fault-around/readahead windowing (bounded by the mount's
// max_readahead, 2MB in this fork's mount options) means completions
// reaching MonitorReadAt are not exactly-contiguous even for logically
// forward-moving access. A request whose offset is within this tolerance of
// the frontier is "arrived out of order, not actually random"; one that
// falls further behind (or jumps far ahead past a gap) is genuinely
// non-contiguous access. Sized to comfortably cover one mount's readahead
// window; see the plan's verification step for tuning this against a live
// trace of real request offsets/sizes from a production mmap workload.
const offsetTolerance = 4 * 1024 * 1024

// For streaming read: only cache the first chunk
// For random read: only fetch the requested range, instead of the whole chunk

func NewReaderPattern() *ReaderPattern {
	return &ReaderPattern{
		isSequentialCounter: 0,
		highWaterMark:       0,
	}
}

// NewReaderPatternWithMode is like NewReaderPattern but honors an explicit
// ReaderCacheMode override. Unrecognized values (including the empty
// string, for callers that don't know about this concept) behave like
// ReaderCacheModeAuto.
func NewReaderPatternWithMode(mode ReaderCacheMode) *ReaderPattern {
	rp := NewReaderPattern()
	switch mode {
	case ReaderCacheModeSequential:
		forced := false
		rp.forced = &forced
	case ReaderCacheModeRandom:
		forced := true
		rp.forced = &forced
	}
	return rp
}

func (rp *ReaderPattern) MonitorReadAt(offset int64, size int) {
	if rp.forced != nil {
		return
	}
	stop := offset + int64(size)

	// Classify against the frontier as it stood *before* this request's own
	// contribution -- comparing against the post-update mark would make any
	// forward jump trivially "at the frontier" (a jump's own stop always
	// becomes the new mark), which would never flag a genuinely
	// non-contiguous forward skip. Load-then-CAS to capture the pre-update
	// value even though we advance the mark unconditionally below.
	var prevMark int64
	for {
		prevMark = atomic.LoadInt64(&rp.highWaterMark)
		if stop <= prevMark {
			break
		}
		if atomic.CompareAndSwapInt64(&rp.highWaterMark, prevMark, stop) {
			break
		}
	}

	counter := atomic.LoadInt64(&rp.isSequentialCounter)

	// A request counts as sequential-ish if it starts within the tolerance
	// window of the frontier as it stood before this request -- either
	// direction: slightly behind (a completion that arrived out of order
	// but is still within the locally-active working set) or slightly
	// ahead (continuing the frontier, or a small forward skip from
	// windowing). A request that starts well behind the frontier, or jumps
	// far ahead of it, is genuinely non-contiguous access.
	if offset >= prevMark-offsetTolerance && offset <= prevMark+offsetTolerance {
		if counter < ModeChangeLimit {
			atomic.AddInt64(&rp.isSequentialCounter, 1)
		}
	} else {
		if counter > -ModeChangeLimit {
			atomic.AddInt64(&rp.isSequentialCounter, -1)
		}
	}
}

func (rp *ReaderPattern) IsRandomMode() bool {
	if rp.forced != nil {
		return *rp.forced
	}
	return atomic.LoadInt64(&rp.isSequentialCounter) < 0
}
