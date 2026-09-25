package page_writer

import (
	"fmt"
	"math/rand"
	"testing"
)

// Keep this benchmark portable to vanilla upstream. Each operation includes
// the completion query used by SaveDataAt; distinct timestamps forbid merging
// away the per-write history just to make the benchmark faster.
func BenchmarkIntervalAppendAndCompletion(b *testing.B) {
	for _, count := range []int{4096, 16384} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(count * 4))
			for n := 0; n < b.N; n++ {
				list := newChunkWrittenIntervalList()
				for i := 0; i < count; i++ {
					list.MarkWritten(int64(i*4), int64((i+1)*4), int64(i+1))
					if list.IsComplete(int64(count*4)) != (i == count-1) {
						b.Fatal("incorrect completion")
					}
				}
			}
		})
	}
}

// Independent byte-level oracle: retain overwritten timestamps and gaps, not
// just total length. Completion must remain correct after filling old holes.
func TestIntervalHistoryMatchesByteOracle(t *testing.T) {
	const size = 257
	for seed := int64(0); seed < 10; seed++ {
		rng := rand.New(rand.NewSource(seed))
		list := newChunkWrittenIntervalList()
		var want [size]int64
		for step := int64(1); step <= 1000; step++ {
			start := rng.Intn(size)
			end := start + rng.Intn(size-start) + 1
			list.MarkWritten(int64(start), int64(end), step)
			for i := start; i < end; i++ {
				want[i] = step
			}
			var got [size]int64
			for p := list.head.next; p != list.tail; p = p.next {
				if p.next.prev != p || p.prev.next != p {
					t.Fatal("broken interval links")
				}
				for i := p.StartOffset; i < p.stopOffset; i++ {
					got[i] = p.TsNs
				}
			}
			if got != want {
				t.Fatalf("seed=%d step=%d timestamp history differs", seed, step)
			}
			prefix, last := 0, 0
			for prefix < size && want[prefix] != 0 {
				prefix++
			}
			for i, ts := range want {
				if ts != 0 {
					last = i + 1
				}
			}
			for _, boundary := range []int{size} {
				if list.IsComplete(int64(boundary)) != (prefix >= boundary) {
					t.Fatalf("seed=%d step=%d boundary=%d", seed, step, boundary)
				}
			}
			if list.IsContiguouslyWritten() != (prefix > 0 && prefix == last) {
				t.Fatalf("seed=%d step=%d contiguity", seed, step)
			}
		}
	}
}
