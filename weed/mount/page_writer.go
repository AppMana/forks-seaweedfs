package mount

import (
	"fmt"
	"math"
	"sync/atomic"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/mount/page_writer"
)

type PageWriter struct {
	fh            *FileHandle
	collection    string
	replication   string
	chunkSize     int64
	writerPattern *WriterPattern

	randomWriter  page_writer.DirtyPages
	lastWriteTsNs atomic.Int64
}

var (
	_ = page_writer.DirtyPages(&PageWriter{})
)

func newPageWriter(fh *FileHandle, chunkSize int64) *PageWriter {
	pw := &PageWriter{
		fh:            fh,
		chunkSize:     chunkSize,
		writerPattern: NewWriterPattern(chunkSize),
		randomWriter:  newMemoryChunkPages(fh, chunkSize),
	}
	return pw
}

func (pw *PageWriter) AddPage(offset int64, data []byte, isSequential bool, tsNs int64) error {
	if len(data) == 0 {
		return nil
	}
	// Chunk conflict resolution uses timestamps, not append order. A client
	// with a slow clock must still supersede the chunks it has already seen.
	// Keep this separate from POSIX mtime and O(1) in the number of chunks.
	var floor int64
	if pw.fh.entryChunkGroup != nil {
		var err error
		floor, err = pw.fh.entryChunkGroup.WriteTimestampFloor()
		if err != nil {
			return fmt.Errorf("resolve write timestamp floor: %w", err)
		}
	}
	for {
		previous := pw.lastWriteTsNs.Load()
		minimum := max(floor, previous)
		if minimum == math.MaxInt64 {
			return fmt.Errorf("chunk write timestamp exhausted")
		}
		next := max(tsNs, minimum+1)
		if pw.lastWriteTsNs.CompareAndSwap(previous, next) {
			tsNs = next
			break
		}
	}

	glog.V(4).Infof("%v AddPage [%d, %d)", pw.fh.fh, offset, offset+int64(len(data)))

	chunkIndex := offset / pw.chunkSize
	for i := chunkIndex; len(data) > 0; i++ {
		writeSize := min(int64(len(data)), (i+1)*pw.chunkSize-offset)
		if err := pw.addToOneChunk(i, offset, data[:writeSize], isSequential, tsNs); err != nil {
			return err
		}
		offset += writeSize
		data = data[writeSize:]
	}
	return nil
}

func (pw *PageWriter) addToOneChunk(chunkIndex, offset int64, data []byte, isSequential bool, tsNs int64) error {
	return pw.randomWriter.AddPage(offset, data, isSequential, tsNs)
}

func (pw *PageWriter) FlushData() error {
	return pw.randomWriter.FlushData()
}

func (pw *PageWriter) HasWrites() bool {
	return pw.randomWriter.HasWrites()
}

func (pw *PageWriter) ReadDirtyDataAt(data []byte, offset int64, tsNs int64) (maxStop int64) {
	glog.V(4).Infof("ReadDirtyDataAt %v [%d, %d)", pw.fh.inode, offset, offset+int64(len(data)))

	chunkIndex := offset / pw.chunkSize
	for i := chunkIndex; len(data) > 0; i++ {
		readSize := min(int64(len(data)), (i+1)*pw.chunkSize-offset)

		maxStop = pw.randomWriter.ReadDirtyDataAt(data[:readSize], offset, tsNs)

		offset += readSize
		data = data[readSize:]
	}

	return
}

func (pw *PageWriter) LockForRead(startOffset, stopOffset int64) {
	pw.randomWriter.LockForRead(startOffset, stopOffset)
}

func (pw *PageWriter) UnlockForRead(startOffset, stopOffset int64) {
	pw.randomWriter.UnlockForRead(startOffset, stopOffset)
}

func (pw *PageWriter) Destroy() {
	pw.randomWriter.Destroy()
}

func (pw *PageWriter) EvictOneWritableChunk() bool {
	return pw.randomWriter.EvictOneWritableChunk()
}

func (pw *PageWriter) ProactiveFlush(nowNs, idleThresholdNs, maxHoldNs, fillRatio int64, frontierLag int, isSequential bool) bool {
	return pw.randomWriter.ProactiveFlush(nowNs, idleThresholdNs, maxHoldNs, fillRatio, frontierLag, isSequential)
}

func max(x, y int64) int64 {
	if x > y {
		return x
	}
	return y
}
func min(x, y int64) int64 {
	if x < y {
		return x
	}
	return y
}
