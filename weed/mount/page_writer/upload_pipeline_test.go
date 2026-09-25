package page_writer

import (
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/util"
)

func TestUploadPipeline(t *testing.T) {

	uploadPipeline := NewUploadPipeline(util.NewLimitedConcurrentExecutor(2), 2*1024*1024, nil, 16, t.TempDir(), nil)

	uploadPipeline.LockForRead(0, 2162688)
	t.Cleanup(func() { uploadPipeline.UnlockForRead(0, 2162688); uploadPipeline.Shutdown() })

	writeRange(t, uploadPipeline, 0, 131072)
	writeRange(t, uploadPipeline, 131072, 262144)
	writeRange(t, uploadPipeline, 262144, 1025536)

	confirmRange(t, uploadPipeline, 0, 1025536)

	writeRange(t, uploadPipeline, 1025536, 1296896)

	confirmRange(t, uploadPipeline, 1025536, 1296896)

	writeRange(t, uploadPipeline, 1296896, 2162688)

	confirmRange(t, uploadPipeline, 1296896, 2162688)

	confirmRange(t, uploadPipeline, 1296896, 2162688)
}

// Both helpers use aligned byte offsets, never word indices.
func writeRange(t *testing.T, uploadPipeline *UploadPipeline, startOff, stopOff int64) {
	t.Helper()
	p := make([]byte, 4)
	for i := startOff; i < stopOff; i += 4 {
		util.Uint32toBytes(p, uint32(i))
		if n, err := uploadPipeline.SaveDataAt(p, i, false, 0); err != nil || n != len(p) {
			t.Fatalf("write at %d: n=%d err=%v", i, n, err)
		}
	}
}

func confirmRange(t *testing.T, uploadPipeline *UploadPipeline, startOff, stopOff int64) {
	t.Helper()
	for startOff < stopOff {
		end := min(stopOff, (startOff/uploadPipeline.ChunkSize+1)*uploadPipeline.ChunkSize)
		p := make([]byte, end-startOff)
		if actual := uploadPipeline.MaybeReadDataAt(p, startOff, 0); actual != end {
			t.Fatalf("read [%d,%d): coverage ended at %d", startOff, end, actual)
		}
		for i := startOff; i < end; i += 4 {
			if got := util.BytesToUint32(p[i-startOff:i-startOff+4]); got != uint32(i) {
				t.Fatalf("offset %d: got %d", i, got)
			}
		}
		startOff = end
	}
}

// Pressure-driven eviction must not seal a chunk with a leading or
// internal gap (issue #9330).
func TestEvictOneWritableChunk_SkipsGappyChunks(t *testing.T) {
	const cs int64 = 2 * 1024 * 1024
	// nil saveToStorage so the async upload SaveContent is a no-op.
	up := NewUploadPipeline(util.NewLimitedConcurrentExecutor(2), cs, nil, 16, "", nil)

	block := make([]byte, cs/4)

	// chunk 0: internal gap; chunk 1: leading gap; chunk 2: contiguous from 0.
	if _, err := up.SaveDataAt(block, 0, true, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, 3*cs/4, true, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs+cs/4, true, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs+cs/2, true, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, 2*cs, true, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, 2*cs+cs/4, true, 6); err != nil {
		t.Fatal(err)
	}

	if !up.EvictOneWritableChunk() {
		t.Fatalf("EvictOneWritableChunk returned false; expected contiguous chunk 2 to be evictable")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(2)]; stillWritable {
		t.Errorf("chunk 2 should have moved to sealed")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(0)]; !stillWritable {
		t.Errorf("chunk 0 (internal gap) must remain writable")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(1)]; !stillWritable {
		t.Errorf("chunk 1 (leading gap) must remain writable")
	}

	// Filling holes makes IsComplete true; maybeMoveToSealed auto-seals.
	if _, err := up.SaveDataAt(block, cs/4, true, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs/2, true, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs, true, 9); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs+3*cs/4, true, 10); err != nil {
		t.Fatal(err)
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(0)]; stillWritable {
		t.Errorf("chunk 0 should have auto-sealed after gap was filled")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(1)]; stillWritable {
		t.Errorf("chunk 1 should have auto-sealed after leading range was filled")
	}
}

// When every chunk is gappy, the fallback must still seal one so
// accountant.Reserve can wake; oldest-LastWriteTsNs wins.
func TestEvictOneWritableChunk_FallbackPicksOldestGappy(t *testing.T) {
	const cs int64 = 2 * 1024 * 1024
	up := NewUploadPipeline(util.NewLimitedConcurrentExecutor(2), cs, nil, 16, "", nil)

	block := make([]byte, cs/4)

	// Strictly increasing tsNs so chunk 0 is oldest; equal WrittenSize.
	if _, err := up.SaveDataAt(block, 0, true, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, 3*cs/4, true, 101); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs+cs/4, true, 200); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs+cs/2, true, 201); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, 2*cs, true, 300); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, 2*cs+3*cs/4, true, 301); err != nil {
		t.Fatal(err)
	}

	if !up.EvictOneWritableChunk() {
		t.Fatalf("fallback must seal a gappy chunk to free a Reserve slot")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(0)]; stillWritable {
		t.Errorf("oldest gappy chunk (0) should have been picked by the fallback")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(1)]; !stillWritable {
		t.Errorf("chunk 1 should remain writable")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(2)]; !stillWritable {
		t.Errorf("chunk 2 should remain writable")
	}
}

// Strict pass preempts the fallback even when a gappy chunk is older.
func TestEvictOneWritableChunk_PrefersStrictOverFallback(t *testing.T) {
	const cs int64 = 2 * 1024 * 1024
	up := NewUploadPipeline(util.NewLimitedConcurrentExecutor(2), cs, nil, 16, "", nil)

	block := make([]byte, cs/4)

	// chunk 0 (older, gappy), chunk 1 (newer, contiguous from 0).
	if _, err := up.SaveDataAt(block, 0, true, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, 3*cs/4, true, 101); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs, true, 200); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs+cs/4, true, 201); err != nil {
		t.Fatal(err)
	}

	if !up.EvictOneWritableChunk() {
		t.Fatalf("EvictOneWritableChunk returned false")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(1)]; stillWritable {
		t.Errorf("contiguous chunk 1 must be picked over older gappy chunk 0")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(0)]; !stillWritable {
		t.Errorf("gappy chunk 0 must remain writable")
	}
}

// ProactiveFlush also skips gappy chunks (issue #9330). No liveness
// fallback here — unlike EvictOneWritableChunk, returning false is just
// a missed optimization.
func TestProactiveFlush_SkipsGappyChunks(t *testing.T) {
	const cs int64 = 2 * 1024 * 1024
	up := NewUploadPipeline(util.NewLimitedConcurrentExecutor(2), cs, nil, 16, "", nil)

	block := make([]byte, cs/4)

	// chunk 0: internal gap; chunk 1: leading gap; chunk 2: contiguous from 0.
	if _, err := up.SaveDataAt(block, 0, true, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, 3*cs/4, true, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs+cs/4, true, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs+cs/2, true, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, 2*cs, true, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, 2*cs+cs/4, true, 6); err != nil {
		t.Fatal(err)
	}

	// nowNs >> chunk timestamps so age clears the idle/maxHold gates;
	// fillRatio < WrittenSize so nearlyFull is true. Leaves
	// IsContiguouslyWritten as the only differentiator.
	const (
		nowNs           int64 = 1_000_000_000
		idleThresholdNs int64 = 100
		maxHoldNs       int64 = 200
		fillRatio       int64 = cs / 8
	)
	if !up.ProactiveFlush(nowNs, idleThresholdNs, maxHoldNs, fillRatio, 0, false) {
		t.Fatalf("ProactiveFlush returned false; expected contiguous chunk 2 to be sealed")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(2)]; stillWritable {
		t.Errorf("chunk 2 should have moved to sealed")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(0)]; !stillWritable {
		t.Errorf("chunk 0 (internal gap) must remain writable")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(1)]; !stillWritable {
		t.Errorf("chunk 1 (leading gap) must remain writable")
	}

	if up.ProactiveFlush(nowNs, idleThresholdNs, maxHoldNs, fillRatio, 0, false) {
		t.Errorf("ProactiveFlush returned true with only gappy chunks left")
	}

	if _, err := up.SaveDataAt(block, cs/4, true, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs/2, true, 8); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs, true, 9); err != nil {
		t.Fatal(err)
	}
	if _, err := up.SaveDataAt(block, cs+3*cs/4, true, 10); err != nil {
		t.Fatal(err)
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(0)]; stillWritable {
		t.Errorf("chunk 0 should have auto-sealed after gap was filled")
	}
	if _, stillWritable := up.writableChunks[LogicChunkIndex(1)]; stillWritable {
		t.Errorf("chunk 1 should have auto-sealed after leading range was filled")
	}
}
