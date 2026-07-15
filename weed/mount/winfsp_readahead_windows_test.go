package mount

import (
	"bytes"
	"testing"
)

// fakeFile backs the fetch callback with a byte slice and counts calls.
type fakeFile struct {
	data         []byte
	fetches      int
	lastFetchLen int // len(dst) passed to the most recent fetch call
}

func (f *fakeFile) fetch(dst []byte, ofst int64) int {
	f.fetches++
	f.lastFetchLen = len(dst)
	if ofst >= int64(len(f.data)) {
		return 0
	}
	n := copy(dst, f.data[ofst:])
	return n
}

func mkData(n int) []byte {
	d := make([]byte, n)
	for i := range d {
		d[i] = byte(i % 251)
	}
	return d
}

func TestReadAheadSequentialServesFromWindow(t *testing.T) {
	c := newReadAheadCache()
	f := &fakeFile{data: mkData(3 << 20)}
	got := make([]byte, 4096)
	for off := int64(0); off < 2<<20; off += 4096 {
		n := c.Read(1, 100, got, off, f.fetch)
		if n != 4096 {
			t.Fatalf("off %d: n=%d", off, n)
		}
		if !bytes.Equal(got, f.data[off:off+4096]) {
			t.Fatalf("off %d: data mismatch", off)
		}
	}
	// 2MB sequentially in 4KB requests = 2 window refills, not 512 fetches
	if f.fetches != 2 {
		t.Fatalf("fetches = %d, want 2", f.fetches)
	}
}

func TestReadAheadRandomAccessBypasses(t *testing.T) {
	c := newReadAheadCache()
	f := &fakeFile{data: mkData(4 << 20)}
	got := make([]byte, 4096)
	offsets := []int64{0, 2 << 20, 1 << 20, 3 << 20}
	for _, off := range offsets {
		if n := c.Read(2, 101, got, off, f.fetch); n != 4096 {
			t.Fatalf("off %d: n=%d", off, n)
		}
		if !bytes.Equal(got, f.data[off:off+4096]) {
			t.Fatalf("off %d: mismatch", off)
		}
	}
	// first read refills (sequential from 0 by default seqEnd=0), rest random direct
	if f.fetches < 3 {
		t.Fatalf("expected mostly direct fetches, got %d", f.fetches)
	}
}

// TestReadAheadToleratesNearbyOutOfOrderOffset is a regression test mirroring
// reader_pattern.go's concurrency fix: WinFsp's default multi-threaded
// dispatcher can complete Read calls on the same handle out of order, so a
// request landing slightly behind the tracked frontier (b.seqEnd) but still
// close to it and outside the current window must still refill the window
// (treated as sequential-ish), not fall back to an unbuffered per-request
// fetch the way a bare ofst == b.seqEnd check would.
func TestReadAheadToleratesNearbyOutOfOrderOffset(t *testing.T) {
	c := newReadAheadCache()
	f := &fakeFile{data: mkData(4 << 20)}
	got := make([]byte, 4096)

	b := c.buffer(6, 105)
	b.mu.Lock()
	b.off = 2 << 20
	b.buf = append([]byte(nil), f.data[2<<20:3<<20]...)
	b.seqEnd = (2 << 20) + 8192
	b.mu.Unlock()

	// 100KB behind the frontier, outside [b.off, b.off+len(buf)) since it's
	// before b.off -- within readAheadOffsetTolerance (256KB).
	off := int64(2<<20) - 100000
	n := c.Read(6, 105, got, off, f.fetch)
	if n != 4096 {
		t.Fatalf("off %d: n=%d", off, n)
	}
	if !bytes.Equal(got, f.data[off:off+4096]) {
		t.Fatalf("off %d: data mismatch", off)
	}
	if f.fetches != 1 {
		t.Fatalf("fetches = %d, want 1", f.fetches)
	}
	if f.lastFetchLen != readAheadSize {
		t.Fatalf("lastFetchLen = %d, want %d (should refill the window, not bypass to a small direct fetch)", f.lastFetchLen, readAheadSize)
	}
}

// TestReadAheadStillBypassesFarOutOfOrderOffset guards against the
// tolerance fix over-widening: an offset far from the frontier (beyond
// readAheadOffsetTolerance) must still be treated as genuinely random and
// bypass the window, exactly like TestReadAheadRandomAccessBypasses.
func TestReadAheadStillBypassesFarOutOfOrderOffset(t *testing.T) {
	c := newReadAheadCache()
	f := &fakeFile{data: mkData(4 << 20)}
	got := make([]byte, 4096)

	b := c.buffer(7, 106)
	b.mu.Lock()
	b.off = 2 << 20
	b.buf = append([]byte(nil), f.data[2<<20:3<<20]...)
	b.seqEnd = (2 << 20) + 8192
	b.mu.Unlock()

	off := int64(0) // far beyond readAheadOffsetTolerance from the frontier
	n := c.Read(7, 106, got, off, f.fetch)
	if n != 4096 {
		t.Fatalf("off %d: n=%d", off, n)
	}
	if !bytes.Equal(got, f.data[off:off+4096]) {
		t.Fatalf("off %d: data mismatch", off)
	}
	if f.lastFetchLen != len(got) {
		t.Fatalf("lastFetchLen = %d, want %d (genuinely random offset should bypass, not refill a 1MB window)", f.lastFetchLen, len(got))
	}
}

func TestReadAheadInvalidateOnWrite(t *testing.T) {
	c := newReadAheadCache()
	f := &fakeFile{data: mkData(1 << 20)}
	got := make([]byte, 4096)
	c.Read(3, 102, got, 0, f.fetch) // fills window
	// mutate the file and invalidate
	copy(f.data[4096:8192], bytes.Repeat([]byte{0xAA}, 4096))
	c.Invalidate(102)
	n := c.Read(3, 102, got, 4096, f.fetch)
	if n != 4096 {
		t.Fatalf("n=%d", n)
	}
	if !bytes.Equal(got, f.data[4096:8192]) {
		t.Fatalf("stale data served after invalidation")
	}
}

func TestReadAheadEOF(t *testing.T) {
	c := newReadAheadCache()
	f := &fakeFile{data: mkData(10000)} // < 3 pages
	got := make([]byte, 4096)
	var total int
	for off := int64(0); ; {
		n := c.Read(4, 103, got, off, f.fetch)
		if n < 0 {
			t.Fatalf("error %d", n)
		}
		total += n
		off += int64(n)
		if n == 0 {
			break
		}
	}
	if total != 10000 {
		t.Fatalf("read %d bytes, want 10000", total)
	}
}

func TestReadAheadLargeRequestBypasses(t *testing.T) {
	c := newReadAheadCache()
	f := &fakeFile{data: mkData(2 << 20)}
	got := make([]byte, readAheadBypassSize)
	if n := c.Read(5, 104, got, 0, f.fetch); n != readAheadBypassSize {
		t.Fatalf("n=%d", n)
	}
	if f.fetches != 1 {
		t.Fatalf("large read should fetch directly once, got %d", f.fetches)
	}
}

func TestReadAheadReleaseFreesSlot(t *testing.T) {
	c := newReadAheadCache()
	f := &fakeFile{data: mkData(1 << 20)}
	got := make([]byte, 4096)
	for fh := uint64(0); fh < readAheadMaxBuffers; fh++ {
		c.Read(fh, 200+fh, got, 0, f.fetch)
	}
	if b := c.buffer(9999, 999); b != nil {
		t.Fatalf("expected cap to refuse new buffers")
	}
	c.Release(0)
	if b := c.buffer(9999, 999); b == nil {
		t.Fatalf("expected slot after release")
	}
}
