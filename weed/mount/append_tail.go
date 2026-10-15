package mount

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// A writer that appends and flushes often (a log flushed per line, a database
// syncing its journal) adds one small chunk per flush, and the entry carries
// every chunk: each commit rewrites a chunk list one longer than the last, so
// metadata bytes grow with the square of the flush count and a read fans out
// to one needle per flush. Flush therefore folds the file's tail of small,
// contiguous chunks into one chunk, the way a binary counter carries: once the
// tail holds more than appendTailMaxRun of them, the newest chunks merge with
// each older one they together outweigh. Every byte is rewritten at most once
// per doubling of its chunk, and the tail stays within appendTailMaxRun plus
// one chunk per doubling up to the chunk size limit.
//
// The pattern is recognised from the chunk list rather than from O_APPEND:
// many writers (WinFsp clients among them) append through positioned writes.
const appendTailMaxRun = 16

// appendTailFoldBudget bounds the read and upload of one fold. A fold is
// optional, so a flush that would wait longer commits the list unfolded.
var appendTailFoldBudget = 5 * time.Second

type tailReadFunc func(ctx context.Context, group []*filer_pb.FileChunk, offset int64, size int) ([]byte, error)

// appendTailGroup returns the tail chunks to fold into one, in offset order, or
// nil. Candidates are small chunks (under half the limit), contiguous up to the
// end of the chunk list, that nothing else reaches into, and that the filer
// already holds: the filer deletes the chunks a commit drops only if it had
// them, so folding one it never saw would leave its blob unreferenced.
func appendTailGroup(chunks, manifests []*filer_pb.FileChunk, committed func(*filer_pb.FileChunk) bool, limit int64) []*filer_pb.FileChunk {
	if len(chunks) <= appendTailMaxRun {
		return nil
	}
	sorted := append([]*filer_pb.FileChunk(nil), chunks...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Offset < sorted[j].Offset })
	small := func(c *filer_pb.FileChunk) bool {
		return !c.IsChunkManifest && c.Size > 0 && int64(c.Size) < limit/2
	}

	n := len(sorted)
	start := n
	for start > 0 {
		c := sorted[start-1]
		if !small(c) || (start < n && c.Offset+int64(c.Size) != sorted[start].Offset) {
			break
		}
		start--
	}
	// A chunk before the run, or a manifest, that ends inside it still owns
	// some of its bytes; start the run past its end.
	var reach int64
	for _, m := range manifests {
		reach = max(reach, m.Offset+int64(m.Size))
	}
	for _, c := range sorted[:start] {
		reach = max(reach, c.Offset+int64(c.Size))
	}
	for start < n && reach > sorted[start].Offset {
		reach = max(reach, sorted[start].Offset+int64(sorted[start].Size))
		start++
	}

	end := n
	for end > start && !committed(sorted[end-1]) {
		end--
	}
	first := end
	for first > start && committed(sorted[first-1]) {
		first--
	}
	run := sorted[first:end]
	if len(run) <= appendTailMaxRun {
		return nil
	}

	k := len(run) - 1
	total := int64(run[k].Size)
	for k > 0 {
		size := int64(run[k-1].Size)
		if size > total || total+size > limit {
			break
		}
		total += size
		k--
	}
	if k == len(run)-1 {
		return nil
	}
	return run[k:]
}

// coalesceAppendTail folds the appended tail of chunks (see appendTailGroup)
// into one chunk saved with save. Any failure returns chunks unchanged.
func coalesceAppendTail(ctx context.Context, chunks, manifests []*filer_pb.FileChunk, committed func(*filer_pb.FileChunk) bool, limit int64, read tailReadFunc, save filer.SaveDataAsChunkFunctionType) []*filer_pb.FileChunk {
	group := appendTailGroup(chunks, manifests, committed, limit)
	if group == nil {
		return chunks
	}
	folded, err := foldAppendTail(ctx, chunks, group, read, save)
	if err != nil {
		glog.V(1).Infof("fold %d appended chunks: %v", len(group), err)
		return chunks
	}
	return folded
}

func foldAppendTail(ctx context.Context, chunks, group []*filer_pb.FileChunk, read tailReadFunc, save filer.SaveDataAsChunkFunctionType) ([]*filer_pb.FileChunk, error) {
	offset := group[0].Offset
	var size, tsNs int64
	members := make(map[*filer_pb.FileChunk]bool, len(group))
	for _, c := range group {
		size += int64(c.Size)
		tsNs = max(tsNs, c.ModifiedTsNs)
		members[c] = true
	}
	data, err := read(ctx, group, offset, int(size))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("read %d of %d bytes", len(data), size)
	}
	// The fold keeps the newest member's timestamp, so a later write over this
	// range still wins against it.
	merged, err := save(bytes.NewReader(data), "", offset, tsNs, uint64(size))
	if err != nil {
		return nil, fmt.Errorf("save: %w", err)
	}
	if ctx.Err() != nil {
		return nil, fmt.Errorf("saved %s after the fold was abandoned: %w", merged.GetFileIdString(), ctx.Err())
	}
	if merged.Size != uint64(size) {
		return nil, fmt.Errorf("saved %d of %d bytes as %s", merged.Size, size, merged.GetFileIdString())
	}

	out := make([]*filer_pb.FileChunk, 0, len(chunks)-len(group)+1)
	placed := false
	for _, c := range chunks {
		if !members[c] {
			out = append(out, c)
		} else if !placed {
			out = append(out, merged)
			placed = true
		}
	}
	return out, nil
}

// coalesceAppendedTail is coalesceAppendTail against this mount's volumes,
// judged committed by the handle's last filer-acknowledged entry, and bounded
// by appendTailFoldBudget so a slow cluster delays no flush.
func (wfs *WFS) coalesceAppendedTail(fileFullPath util.FullPath, fh *FileHandle, chunks, manifests []*filer_pb.FileChunk) []*filer_pb.FileChunk {
	base := fh.baseEntry.Load()
	if base == nil {
		return chunks
	}
	held := make(map[string]bool, len(base.GetChunks()))
	for _, c := range base.GetChunks() {
		held[c.GetFileIdString()] = true
	}
	committed := func(c *filer_pb.FileChunk) bool { return held[c.GetFileIdString()] }
	group := appendTailGroup(chunks, manifests, committed, wfs.option.ChunkSizeLimit)
	if group == nil {
		return chunks
	}

	ctx, cancel := context.WithTimeout(context.Background(), appendTailFoldBudget)
	defer cancel()
	type result struct {
		chunks []*filer_pb.FileChunk
		err    error
	}
	done := make(chan result, 1)
	go func() {
		folded, err := foldAppendTail(ctx, chunks, group, wfs.readChunkRange, wfs.saveDataAsChunk(fileFullPath))
		if err != nil {
			// a fold that finishes after the flush gave up on it leaves its blob
			// unreferenced; name it so volume.fsck's purge is explainable
			glog.V(0).Infof("%s: fold %d appended chunks: %v", fileFullPath, len(group), err)
		}
		done <- result{folded, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return chunks
		}
		glog.V(3).Infof("%s: folded %d appended chunks into one", fileFullPath, len(group))
		return r.chunks
	case <-ctx.Done():
		return chunks
	}
}

// readChunkRange reads size bytes at offset from chunks that cover the range
// without overlapping.
func (wfs *WFS) readChunkRange(ctx context.Context, group []*filer_pb.FileChunk, offset int64, size int) ([]byte, error) {
	reader := filer.NewChunkStreamReaderFromLookup(ctx, wfs.LookupFn(), group)
	defer reader.Close()
	buf := make([]byte, size)
	n, err := reader.ReadAt(buf, offset)
	if n == size {
		return buf, nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return nil, err
}
