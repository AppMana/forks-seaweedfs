package needle

import (
	"bytes"
	"testing"
)

// TestEagerPreGrow is a regression guard for the volume-side amplifier in
// https://github.com/seaweedfs/seaweedfs/issues/6541.
//
// ParseUpload reads the body into a buffer sized from r.ContentLength, the
// amount upload admission reserved for the request, so the body is allocated
// once instead of through a chain of doubling grows.
//
// The cases pin the sizing (exactly Content-Length, however large, up to
// sizeLimit) and the validity gates: negative / zero / oversized
// Content-Length and buffers that already have enough capacity do not Grow.
func TestEagerPreGrow(t *testing.T) {
	const sizeLimit = int64(256 * 1024 * 1024)

	cases := []struct {
		name       string
		startCap   int
		cl         int64
		wantMinCap int
		wantNoop   bool // post: cap stays at startCap
	}{
		{
			name:       "small content-length grows exactly",
			cl:         1 * 1024 * 1024,
			wantMinCap: 1 * 1024 * 1024,
		},
		{
			name:       "4 MiB chunk grows to its content-length",
			cl:         4 * 1024 * 1024,
			wantMinCap: 4 * 1024 * 1024,
		},
		{
			name:       "large content-length grows to it in one allocation",
			cl:         200 * 1024 * 1024,
			wantMinCap: 200 * 1024 * 1024,
		},
		{
			name:     "content-length above sizeLimit is rejected",
			cl:       sizeLimit + 1,
			wantNoop: true,
		},
		{
			name:     "zero content-length is a no-op",
			cl:       0,
			wantNoop: true,
		},
		{
			name:     "negative content-length (chunked encoding) is a no-op",
			cl:       -1,
			wantNoop: true,
		},
		{
			name:       "buffer with sufficient cap is left alone",
			startCap:   4 * 1024 * 1024,
			cl:         1 * 1024 * 1024,
			wantMinCap: 4 * 1024 * 1024,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bb := &bytes.Buffer{}
			if c.startCap > 0 {
				bb.Grow(c.startCap)
			}
			startCap := bb.Cap()

			eagerPreGrow(bb, c.cl, sizeLimit)

			if c.wantNoop {
				if got := bb.Cap(); got != startCap {
					t.Fatalf("eagerPreGrow(cl=%d, sizeLimit=%d) should not have grown: "+
						"cap=%d, want %d (regression: validity gate removed?)",
						c.cl, sizeLimit, got, startCap)
				}
				return
			}
			if got := bb.Cap(); got < c.wantMinCap {
				t.Fatalf("after eagerPreGrow(cl=%d): cap=%d, want >=%d", c.cl, got, c.wantMinCap)
			}
			// A grow from empty allocates Content-Length rounded up to whole
			// 8 KiB pages, never a doubling.
			if c.startCap == 0 {
				if got := int64(bb.Cap()); got > c.cl+8*1024 {
					t.Fatalf("after eagerPreGrow(cl=%d): cap=%d, want at most %d",
						c.cl, got, c.cl+8*1024)
				}
			}
		})
	}
}
