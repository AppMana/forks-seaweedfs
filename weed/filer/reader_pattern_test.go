package filer

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

// These tests characterize ReaderPattern's sequential/random detection under
// realistic FUSE read traffic, not just the idealized "one goroutine calling
// MonitorReadAt in strict offset order" case the original implementation
// assumed.
//
// Two distinct, independently real access patterns can defeat a bare
// lastOffset == offset equality check even when the underlying file access
// is not actually random:
//
//  1. Windowed reads whose start/stop offsets don't land exactly back to
//     back, e.g. kernel page-fault-around/readahead windowing over a file
//     accessed in roughly-forward but not byte-contiguous order (this is
//     what safetensors' mmap-based, dict-order tensor reads produce).
//  2. Concurrent, out-of-order-completing reads on the same handle (FUSE
//     serves Read() requests under a shared, not exclusive, per-handle
//     lock -- see weed/mount/weedfs_file_read.go -- so this is a normal,
//     expected condition, not an edge case).
//
// A detector that can't tell "not actually random" apart from "genuinely
// random" in either of these cases is a real bug: it silently disables
// whole-chunk caching (see reader_at.go's IsRandomMode() branches) for
// access that would otherwise have been served efficiently.

// TestMonitorReadAt_StrictSequential is the baseline: a single goroutine
// issuing back-to-back, exactly-contiguous reads must never be classified
// as random. This should pass both before and after the fix.
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
// readahead windowing: each request is a window of readSize bytes, and
// consecutive windows advance by a similar but not always identical amount
// (mirroring how safetensors' non-byte-contiguous tensor access, combined
// with the kernel's own independent readahead heuristic, produces windows
// whose boundaries don't align exactly). This is still, from the
// application's perspective, "roughly forward-moving, locally clustered"
// access -- not the shuffled-whole-file pattern of genuine random reads --
// and should not be classified as random.
func TestMonitorReadAt_WindowedNonContiguous(t *testing.T) {
	rp := NewReaderPattern()
	const windowSize = 64 * 1024
	offset := int64(0)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		rp.MonitorReadAt(offset, windowSize)
		// Advance by a window that overlaps or gaps slightly instead of
		// landing exactly at offset+windowSize -- this is what a
		// non-byte-contiguous but still forward-moving access pattern
		// produces once windowed by kernel readahead.
		jitter := int64(rng.Intn(4096)) - 2048
		offset += windowSize + jitter
	}
	if rp.IsRandomMode() {
		t.Fatalf("windowed, non-exactly-contiguous but locally-clustered access was classified as random")
	}
}

// TestMonitorReadAt_ConcurrentOutOfOrderCompletion simulates FUSE's shared
// (not exclusive) per-handle lock allowing multiple concurrent Read()
// requests on one handle. Each of N logical streams is itself a strictly
// sequential run of small requests, but because MonitorReadAt is called at
// request *completion* time -- and concurrent completions can arrive in any
// order -- the calls actually reaching MonitorReadAt can interleave across
// streams. This is not genuinely random access: every individual stream's
// own requests are perfectly contiguous. It should not be classified as
// random after the fix.
//
// The interleaving here is driven deterministically (fixed round-robin
// across streams) rather than via real goroutine scheduling, because
// relying on the Go scheduler to actually race N goroutines produces a
// flaky test: whether true interleaving happens at all is scheduler-timing
// dependent, so a run that happens not to interleave silently passes
// without exercising the bug. Deterministic interleaving reproduces the
// actual failure mode -- calls arriving at MonitorReadAt in an order that
// doesn't match any single stream's logical offset sequence -- reliably.
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

// TestMonitorReadAt_ConcurrentOutOfOrderCompletion_RealGoroutines is a
// companion, non-deterministic race-detector-friendly variant using real
// goroutines and -race, kept separate from the deterministic test above so
// CI failures are unambiguous about which property broke. Not required to
// reliably trip pre-fix (scheduler-dependent), but must never trip random
// mode post-fix even when it does interleave.
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
