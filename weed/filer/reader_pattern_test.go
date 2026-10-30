package filer

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

// TestMonitorReadAt_StrictSequential verifies that contiguous sequential reads
// remain in sequential mode.
func TestMonitorReadAt_StrictSequential(t *testing.T) {
	rp := NewReaderPattern()
	offset := int64(0)
	const readSize = 4096
	for i := 0; i < 100; i++ {
		rp.MonitorReadAt(offset, readSize)
		offset += readSize
	}
	if rp.IsRandomMode() {
		t.Fatalf("strictly sequential access was classified as random")
	}
}

// TestMonitorReadAt_WindowedNonContiguous simulates kernel fault-around /
// readahead windowing where consecutive windows advance forward with small
// overlaps or gaps.
func TestMonitorReadAt_WindowedNonContiguous(t *testing.T) {
	rp := NewReaderPattern()
	const windowSize = 64 * 1024
	offset := int64(0)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		rp.MonitorReadAt(offset, windowSize)
		jitter := int64(rng.Intn(4096)) - 2048
		offset += windowSize + jitter
	}
	if rp.IsRandomMode() {
		t.Fatalf("windowed, non-exactly-contiguous but locally-clustered access was classified as random")
	}
}

// TestMonitorReadAt_ConcurrentOutOfOrderCompletion simulates concurrent Read()
// completions arriving interleaved across streams within a handle's readahead
// window.
func TestMonitorReadAt_ConcurrentOutOfOrderCompletion(t *testing.T) {
	rp := NewReaderPattern()
	const readSize = 4096
	const numStreams = 8
	const readsPerStream = 50

	type call struct {
		offset int64
	}
	// Each stream is one concurrent in-flight page-fault window: internally
	// strictly contiguous, but the streams cover distinct nearby regions
	// within a single handle's active working set -- bounded by FUSE's
	// concurrent in-flight request limit and the kernel's readahead window
	// (2MB here, see weed/command/mount_std.go's MaxReadAhead), not
	// scattered arbitrarily across a multi-GB file. Total span across all
	// streams here is numStreams*readsPerStream*readSize ≈ 1.6MB, comfortably
	// inside that window.
	streams := make([][]call, numStreams)
	for s := 0; s < numStreams; s++ {
		offset := int64(s) * readsPerStream * readSize
		for i := 0; i < readsPerStream; i++ {
			streams[s] = append(streams[s], call{offset: offset})
			offset += readSize
		}
	}
	// Interleave one completion at a time across streams. This is the
	// granularity that actually stresses the detector: a run of several
	// same-stream completions in a row would re-match the shared
	// lastReadStopOffset on every call after the first, masking the bug
	// (net effect per switch would be dominated by in-run matches, not the
	// single cross-stream mismatch) -- single-call interleaving is what
	// forces every call to compare against a different stream's offset.
	const runLength = 1
	cursor := make([]int, numStreams)
	for {
		progressed := false
		for s := 0; s < numStreams; s++ {
			for r := 0; r < runLength && cursor[s] < len(streams[s]); r++ {
				rp.MonitorReadAt(streams[s][cursor[s]].offset, readSize)
				cursor[s]++
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}

	if rp.IsRandomMode() {
		t.Fatalf("interleaved-but-per-stream-sequential access was classified as random")
	}
}

// TestMonitorReadAt_ConcurrentOutOfOrderCompletion_RealGoroutines exercises
// MonitorReadAt concurrently across real goroutines under the race detector.
func TestMonitorReadAt_ConcurrentOutOfOrderCompletion_RealGoroutines(t *testing.T) {
	rp := NewReaderPattern()
	const readSize = 4096
	const numGoroutines = 8
	const readsPerGoroutine = 50

	// Regions stay within a single handle's active working set (same scale
	// as the deterministic test above), not scattered arbitrarily far
	// apart -- see that test's comment for why the spacing matters.
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(base int64) {
			defer wg.Done()
			<-start
			offset := base
			for i := 0; i < readsPerGoroutine; i++ {
				rp.MonitorReadAt(offset, readSize)
				offset += readSize
			}
		}(int64(g) * readsPerGoroutine * readSize)
	}
	close(start)
	wg.Wait()

	if rp.IsRandomMode() {
		t.Fatalf("concurrent but per-goroutine-sequential access was classified as random")
	}
}

// TestMonitorReadAt_GenuinelyRandom is the control case: offsets shuffled
// across the whole file, no locality at all. This must correctly trip
// random mode both before and after the fix -- the fix must not make the
// detector blind to real random access.
func TestMonitorReadAt_GenuinelyRandom(t *testing.T) {
	rp := NewReaderPattern()
	const fileSize = 100 * 1024 * 1024
	const readSize = 4096
	rng := rand.New(rand.NewSource(2))

	offsets := make([]int64, 0, 200)
	for i := 0; i < 200; i++ {
		offsets = append(offsets, rng.Int63n(fileSize))
	}
	for _, offset := range offsets {
		rp.MonitorReadAt(offset, readSize)
	}
	if !rp.IsRandomMode() {
		t.Fatalf("genuinely random, non-local access was NOT classified as random")
	}
}

// ---------------------------------------------------------------------------
// Upstream 4.36 (PR: read-frontier + SeqTolerance) landed its own fix for the
// same false-random misclassification this fork had patched independently.
// The fork's implementation was dropped in favor of upstream's during the 4.36
// merge; both test suites are kept because they cover different ground -- the
// fork's exercise concurrent/mmap-shaped traffic with real goroutines, and
// upstream's pin the tolerance boundary and hysteresis arithmetic.
// ---------------------------------------------------------------------------

const mb = 1 << 20

func TestReaderPatternSequentialAndHysteresis(t *testing.T) {
	rp := NewReaderPattern()
	rp.MonitorReadAt(0, mb)    // near(0 vs frontier 0) -> +1
	rp.MonitorReadAt(mb, mb)   // +2
	rp.MonitorReadAt(2*mb, mb) // +3 (capped)
	if rp.IsRandomMode() {
		t.Fatal("a stream from offset 0 must not be random")
	}
	// a single far outlier does not flip to random (hysteresis): +3 -> +2
	rp.MonitorReadAt(900*mb, mb)
	if rp.IsRandomMode() {
		t.Fatal("a single outlier read must not flip to random mode")
	}
}

func TestReaderPatternFarFirstReadIsRandom(t *testing.T) {
	rp := NewReaderPattern()
	rp.MonitorReadAt(500*mb, mb) // far from frontier 0 -> -1
	if !rp.IsRandomMode() {
		t.Fatal("a far first read should be random")
	}
}

func TestReaderPatternToleranceAbsorbsReorder(t *testing.T) {
	rp := NewReaderPattern()
	rp.MonitorReadAt(0, mb)    // +1, frontier 1MB
	rp.MonitorReadAt(6*mb, mb) // |6-1|=5MB <= 8MB tolerance -> near, +2, frontier 7MB
	rp.MonitorReadAt(3*mb, mb) // |3-7|=4MB <= 8MB -> near, +3
	if rp.IsRandomMode() {
		t.Fatal("reordered reads within tolerance must stay sequential")
	}
}

func TestReaderPatternRandomRecoveryNeedsHysteresis(t *testing.T) {
	rp := NewReaderPattern()
	rp.MonitorReadAt(100*mb, mb) // far -> -1
	rp.MonitorReadAt(300*mb, mb) // far -> -2
	rp.MonitorReadAt(500*mb, mb) // far -> -3 (capped), frontier 501MB
	// a single near read must not immediately flip back to sequential: -3 -> -2
	rp.MonitorReadAt(501*mb, mb)
	if !rp.IsRandomMode() {
		t.Fatal("one near read must not flip back from deep random mode")
	}
}

// The max-frontier (vs the old atomic.Swap of the last read's stop offset) is the
// load-bearing difference of this detector: the frontier must never move backward,
// or a backward read would lower the baseline and make later far reads look near.
func TestReaderPatternFrontierNeverRegresses(t *testing.T) {
	rp := NewReaderPattern()
	rp.MonitorReadAt(500*mb, mb) // far first read -> -1, frontier 501MB
	rp.MonitorReadAt(0, mb)      // a backward read must not pull the frontier back
	if got := atomic.LoadInt64(&rp.readFrontier); got != 501*mb {
		t.Fatalf("frontier regressed to %d; the max-frontier must never move backward", got)
	}
	// Behavioral consequence: reads far below the preserved frontier stay random.
	// Had the frontier regressed to ~1MB, this would wrongly read as sequential.
	if !rp.IsRandomMode() {
		t.Fatal("reads far below the preserved frontier must remain random")
	}
}

// SeqTolerance is the central new tuning knob; pin both sides of the inclusive
// boundary so an off-by-one or a '<' vs '<=' change can't slip through silently.
func TestReaderPatternToleranceBoundary(t *testing.T) {
	atBoundary := NewReaderPattern()
	atBoundary.MonitorReadAt(SeqTolerance, 0) // diff == SeqTolerance -> near (inclusive), +1
	if atBoundary.IsRandomMode() {
		t.Fatal("a read exactly at frontier+SeqTolerance must count as sequential")
	}
	pastBoundary := NewReaderPattern()
	pastBoundary.MonitorReadAt(SeqTolerance+1, 0) // diff == SeqTolerance+1 -> far, -1
	if !pastBoundary.IsRandomMode() {
		t.Fatal("a read one byte past frontier+SeqTolerance must count as random")
	}
}

// The escape from random mode: sustained near reads must climb the counter back
// out of the negative floor and re-enter sequential (whole-chunk cache) mode.
func TestReaderPatternRecoversFromRandom(t *testing.T) {
	rp := NewReaderPattern()
	rp.MonitorReadAt(100*mb, mb) // far -> -1
	rp.MonitorReadAt(300*mb, mb) // far -> -2
	rp.MonitorReadAt(500*mb, mb) // far -> -3 (capped), frontier 501MB
	if !rp.IsRandomMode() {
		t.Fatal("three far reads must be random")
	}
	// contiguous near reads: -3 -> -2 -> -1 -> 0 -> +1, back out of random mode
	for i := int64(0); i < 4; i++ {
		rp.MonitorReadAt(501*mb+i*mb, mb)
	}
	if rp.IsRandomMode() {
		t.Fatal("sustained near reads must recover sequential mode")
	}
}

// A ranged request's first read lands far from the frontier, but its
// remaining buffer reads are contiguous. The random verdict must stick for
// them — otherwise the tail of every range >256KiB pays a whole-chunk fetch.
func TestReaderPatternRangedReadStaysRandom(t *testing.T) {
	rp := NewReaderPattern()
	rp.MonitorReadAt(500*mb, 256*1024) // far first read -> -ModeChangeLimit
	for i := 1; i <= 2; i++ {
		rp.MonitorReadAt(500*mb+int64(i)*256*1024, 256*1024)
		if !rp.IsRandomMode() {
			t.Fatalf("contiguous read %d of a ranged request flipped back to sequential", i+1)
		}
	}
	for i := 3; i < 10; i++ {
		rp.MonitorReadAt(500*mb+int64(i)*256*1024, 256*1024)
	}
	if rp.IsRandomMode() {
		t.Fatal("sustained sequential reads should restore sequential mode")
	}
}
