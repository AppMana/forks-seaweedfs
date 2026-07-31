package filer

import (
	"sync/atomic"
)

type ReaderPattern struct {
	isSequentialCounter int64
	readFrontier        int64 // highest (offset+size) observed across reads
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

// SeqTolerance: a read whose start is within this many bytes of the current read
// frontier still counts as sequential. Using a tolerance window rather than
// strict contiguity absorbs reordered/concurrent readahead (multiple ReadAt can
// be in flight at once) while still rejecting far random jumps.
const SeqTolerance = 8 << 20 // 8 MiB

// For streaming read: only cache the first chunk
// For random read: only fetch the requested range, instead of the whole chunk

func NewReaderPattern() *ReaderPattern {
	return &ReaderPattern{
		isSequentialCounter: 0,
		readFrontier:        0,
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
	// An explicit -readerCacheMode override makes the inferred classification
	// irrelevant; skip the bookkeeping entirely.
	if rp.forced != nil {
		return
	}

	// Advance the frontier to max(frontier, offset+size) and capture, in the same
	// CAS loop, the pre-image this read is judged against. Reading the frontier
	// inside the loop (rather than once up front) keeps `diff` below comparing
	// against the freshest value even if a concurrent readahead advances the
	// frontier while we loop. Lock-free, consistent with the rest of this type.
	end := offset + int64(size)
	var frontier int64
	for {
		frontier = atomic.LoadInt64(&rp.readFrontier)
		if end <= frontier || atomic.CompareAndSwapInt64(&rp.readFrontier, frontier, end) {
			break
		}
	}

	// near = this read starts within SeqTolerance of where reads had reached.
	// Hysteresis (the ±ModeChangeLimit counter) keeps a single outlier read from
	// flipping the mode.
	diff := offset - frontier
	if diff < 0 {
		diff = -diff
	}
	counter := atomic.LoadInt64(&rp.isSequentialCounter)
	if diff <= SeqTolerance {
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
