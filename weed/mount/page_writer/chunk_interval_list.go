package page_writer

import (
	"math"
)

// ChunkWrittenInterval mark one written interval within one page chunk
type ChunkWrittenInterval struct {
	StartOffset int64
	stopOffset  int64
	TsNs        int64
	prev        *ChunkWrittenInterval
	next        *ChunkWrittenInterval
}

func (interval *ChunkWrittenInterval) Size() int64 {
	return interval.stopOffset - interval.StartOffset
}

func (interval *ChunkWrittenInterval) isComplete(chunkSize int64) bool {
	return interval.stopOffset-interval.StartOffset == chunkSize
}

// ChunkWrittenIntervalList mark written intervals within one page chunk
type ChunkWrittenIntervalList struct {
	head *ChunkWrittenInterval
	tail *ChunkWrittenInterval
	// Written coverage only grows, even when a newer write replaces timestamps.
	coveredPrefix int64
}

func newChunkWrittenIntervalList() *ChunkWrittenIntervalList {
	list := &ChunkWrittenIntervalList{
		head: &ChunkWrittenInterval{
			StartOffset: -1,
			stopOffset:  -1,
		},
		tail: &ChunkWrittenInterval{
			StartOffset: math.MaxInt64,
			stopOffset:  math.MaxInt64,
		},
	}
	list.head.next = list.tail
	list.tail.prev = list.head
	return list
}

func (list *ChunkWrittenIntervalList) MarkWritten(startOffset, stopOffset, tsNs int64) {
	if startOffset >= stopOffset {
		return
	}
	interval := &ChunkWrittenInterval{
		StartOffset: startOffset,
		stopOffset:  stopOffset,
		TsNs:        tsNs,
	}
	list.addInterval(interval)
	if startOffset <= list.coveredPrefix && stopOffset > list.coveredPrefix {
		list.coveredPrefix = stopOffset
		for next := interval.next; next != list.tail && next.StartOffset <= list.coveredPrefix; next = next.next {
			if next.stopOffset > list.coveredPrefix {
				list.coveredPrefix = next.stopOffset
			}
		}
	}
}

// IsComplete reports whether every byte of [0, chunkSize) has been
// written, possibly via multiple adjacent or overlapping intervals.
// addInterval does not merge adjacent intervals — a chunk filled by
// two 1 MiB writes ends up as {[0,1M], [1M,2M]}, not one [0,2M] —
// so checking list.size()==1 misses the "filled by adjacent writes"
// case, leaving the chunk pinned in writableChunks even though all
// its bytes are present. That latent bug became a hard deadlock once
// -writeBufferSizeMB started reserving a global slot per writable
// chunk: the chunks never got sealed, no uploader ran, no slot was
// ever released, and the FUSE writer blocked in Reserve forever
// (seaweedfs issue #8777 / PR #9066). MarkWritten tracks the covered
// prefix incrementally so checking after every small write stays cheap.
func (list *ChunkWrittenIntervalList) IsComplete(chunkSize int64) bool {
	return list.coveredPrefix >= chunkSize
}
func (list *ChunkWrittenIntervalList) WrittenSize() (writtenByteCount int64) {
	for t := list.head; t != nil; t = t.next {
		writtenByteCount += t.Size()
	}
	return
}

// IsContiguouslyWritten reports whether the written bytes form one
// unbroken run starting at offset 0. Pressure-driven sealing uses this
// to avoid racing in-flight FUSE writeback on a gap range — sealing a
// gappy chunk would emit volume chunks with no coverage for the gap and
// reads would silently zero-fill it (issue #9330).
func (list *ChunkWrittenIntervalList) IsContiguouslyWritten() bool {
	first := list.head.next
	if first == list.tail || first.StartOffset != 0 {
		return false
	}
	return list.coveredPrefix == list.tail.prev.stopOffset
}

func (list *ChunkWrittenIntervalList) addInterval(interval *ChunkWrittenInterval) {
	// Preserve distinct timestamps, but avoid scanning the entire history for
	// sequential writes (including appends beyond a gap).
	if last := list.tail.prev; last == list.head || last.stopOffset <= interval.StartOffset {
		last.next = interval
		interval.prev = last
		interval.next = list.tail
		list.tail.prev = interval
		return
	}

	//t := list.head
	//for ; t.next != nil; t = t.next {
	//	if t.TsNs > interval.TsNs {
	//		println("writes is out of order", t.TsNs-interval.TsNs, "ns")
	//	}
	//}

	p := list.head
	for ; p.next != nil && p.next.stopOffset <= interval.StartOffset; p = p.next {
	}
	q := list.tail
	for ; q.prev != nil && q.prev.StartOffset >= interval.stopOffset; q = q.prev {
	}

	// left side
	// interval after p.next start
	if p.next.StartOffset < interval.StartOffset {
		t := &ChunkWrittenInterval{
			StartOffset: p.next.StartOffset,
			stopOffset:  interval.StartOffset,
			TsNs:        p.next.TsNs,
		}
		p.next = t
		t.prev = p
		t.next = interval
		interval.prev = t
	} else {
		p.next = interval
		interval.prev = p
	}

	// right side
	// interval ends before p.prev
	if interval.stopOffset < q.prev.stopOffset {
		t := &ChunkWrittenInterval{
			StartOffset: interval.stopOffset,
			stopOffset:  q.prev.stopOffset,
			TsNs:        q.prev.TsNs,
		}
		q.prev = t
		t.next = q
		interval.next = t
		t.prev = interval
	} else {
		q.prev = interval
		interval.next = q
	}

}

func (list *ChunkWrittenIntervalList) size() int {
	var count int
	for t := list.head; t != nil; t = t.next {
		count++
	}
	return count - 2
}
