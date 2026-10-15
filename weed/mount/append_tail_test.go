package mount

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// blobStore stands in for the volume servers: save keeps a blob, read returns
// the bytes of contiguous chunks.
type blobStore struct {
	blobs map[string][]byte
	next  uint64
	saved int64
}

func newBlobStore() *blobStore { return &blobStore{blobs: map[string][]byte{}} }

func (s *blobStore) put(data []byte, offset, tsNs int64) *filer_pb.FileChunk {
	s.next++
	fid := fmt.Sprintf("1,%x00000000", s.next)
	s.blobs[fid] = append([]byte(nil), data...)
	s.saved += int64(len(data))
	return &filer_pb.FileChunk{
		FileId: fid, Offset: offset, Size: uint64(len(data)), ModifiedTsNs: tsNs,
		Fid: &filer_pb.FileId{VolumeId: 1, FileKey: s.next},
	}
}

func (s *blobStore) save(reader io.Reader, _ string, offset, tsNs int64, _ uint64) (*filer_pb.FileChunk, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	return s.put(data, offset, tsNs), nil
}

func (s *blobStore) read(_ context.Context, group []*filer_pb.FileChunk, offset int64, size int) ([]byte, error) {
	var out []byte
	for _, c := range group {
		if c.Offset != offset+int64(len(out)) {
			return nil, fmt.Errorf("chunk %s at %d is not contiguous", c.FileId, c.Offset)
		}
		out = append(out, s.blobs[c.FileId][:c.Size]...)
	}
	if len(out) != size {
		return nil, fmt.Errorf("read %d of %d bytes", len(out), size)
	}
	return out, nil
}

// content reassembles the file from non-overlapping chunks.
func (s *blobStore) content(chunks []*filer_pb.FileChunk) []byte {
	sorted := append([]*filer_pb.FileChunk(nil), chunks...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Offset < sorted[j].Offset })
	var out []byte
	for _, c := range sorted {
		out = append(out, s.blobs[c.FileId][:c.Size]...)
	}
	return out
}

func committedIn(chunks []*filer_pb.FileChunk) func(*filer_pb.FileChunk) bool {
	ids := map[string]bool{}
	for _, c := range chunks {
		ids[c.FileId] = true
	}
	return func(c *filer_pb.FileChunk) bool { return ids[c.FileId] }
}

// flushOnce runs the chunk-list steps of a flush the way
// flushMetadataToFilerLocked does and returns what it would commit.
func flushOnce(t *testing.T, store *blobStore, chunks, committed []*filer_pb.FileChunk, limit int64) []*filer_pb.FileChunk {
	t.Helper()
	manifests, data := filer.SeparateManifestChunks(chunks)
	compacted, _, err := filer.CompactFileChunks(context.Background(), nil, data)
	if err != nil {
		t.Fatal(err)
	}
	compacted = coalesceAppendTail(context.Background(), compacted, manifests, committedIn(committed), limit, store.read, store.save)
	return append(compacted, manifests...)
}

// A log flushed after every line adds one small chunk per flush. The entry
// carries every chunk, so without folding the tail its size, and each commit
// of it, grows with the number of flushes.
func TestAppendFlushesKeepChunkCountBounded(t *testing.T) {
	const (
		limit   = 2 << 20
		flushes = 3000
	)
	store := newBlobStore()
	var chunks, committed []*filer_pb.FileChunk
	var want []byte
	maxChunks := 0
	for i := 0; i < flushes; i++ {
		line := []byte(fmt.Sprintf("%06d %s\n", i, bytes.Repeat([]byte{'x'}, 150+i%300)))
		chunks = append(chunks, store.put(line, int64(len(want)), int64(i+1)))
		want = append(want, line...)
		chunks = flushOnce(t, store, chunks, committed, limit)
		committed = append([]*filer_pb.FileChunk(nil), chunks...)
		if len(chunks) > maxChunks {
			maxChunks = len(chunks)
		}
	}
	if got := store.content(chunks); !bytes.Equal(got, want) {
		t.Fatalf("content differs after %d flushes: %d bytes, want %d", flushes, len(got), len(want))
	}
	// the tail run plus one chunk per doubling up to the chunk limit
	if maxChunks > appendTailMaxRun+24 {
		t.Fatalf("entry reached %d chunks over %d flushes", maxChunks, flushes)
	}
	// every byte rewritten at most once per doubling
	if amplification := float64(store.saved) / float64(len(want)); amplification > 16 {
		t.Fatalf("saved %d bytes for %d appended (%.1fx)", store.saved, len(want), amplification)
	}
	t.Logf("%d flushes, %d bytes: max %d chunks, %d final, %.1fx bytes saved",
		flushes, len(want), maxChunks, len(chunks), float64(store.saved)/float64(len(want)))
}

// Chunks the filer has not acknowledged stay out of a fold: the filer deletes
// only chunks it held that a commit drops, so folding an unacknowledged chunk
// would leave its blob unreferenced.
func TestAppendTailFoldsOnlyCommittedChunks(t *testing.T) {
	store := newBlobStore()
	var chunks []*filer_pb.FileChunk
	var off int64
	for i := 0; i < appendTailMaxRun+4; i++ {
		c := store.put(bytes.Repeat([]byte{'a'}, 100), off, int64(i+1))
		chunks = append(chunks, c)
		off += 100
	}
	committed := append([]*filer_pb.FileChunk(nil), chunks[:len(chunks)-2]...)
	out := flushOnce(t, store, chunks, committed, 2<<20)
	if len(out) != 3 {
		t.Fatalf("got %d chunks, want the folded committed run plus 2 unacknowledged", len(out))
	}
	for _, c := range chunks[len(chunks)-2:] {
		if !committedIn(out)(c) {
			t.Fatalf("unacknowledged chunk %s was folded", c.FileId)
		}
	}
	if !bytes.Equal(store.content(out), store.content(chunks)) {
		t.Fatal("content changed")
	}
}

// Only a tail nothing else overlaps is folded: a chunk that covers part of it
// keeps those bytes, so a fold would either resurrect them or need the views.
func TestAppendTailLeavesOverlappedRunAlone(t *testing.T) {
	store := newBlobStore()
	var chunks []*filer_pb.FileChunk
	for i := 0; i < appendTailMaxRun+4; i++ {
		chunks = append(chunks, store.put(bytes.Repeat([]byte{'a'}, 100), int64(i*100), int64(i+1)))
	}
	// a later overwrite straddling the whole run at its start
	chunks = append(chunks, store.put(bytes.Repeat([]byte{'b'}, 150), 50, 1000))
	out := flushOnce(t, store, chunks, chunks, 2<<20)
	for _, c := range out {
		if c.Offset < 200 && c.Size > 150 {
			t.Fatalf("folded across the overwrite: chunk at %d size %d", c.Offset, c.Size)
		}
	}
	views, err := filer.ViewFromChunks(context.Background(), nil, out, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if views.Front().Value.FileId != chunks[0].FileId || views.Front().Value.ViewSize != 50 {
		t.Fatal("first 50 bytes no longer come from the first chunk")
	}
}

// A failed read or upload leaves the list as it was: the fold is an
// optimization and must never fail or alter a flush.
func TestAppendTailFoldFailureKeepsChunks(t *testing.T) {
	store := newBlobStore()
	var chunks []*filer_pb.FileChunk
	for i := 0; i < appendTailMaxRun+4; i++ {
		chunks = append(chunks, store.put([]byte("0123456789"), int64(i*10), int64(i+1)))
	}
	failing := func(io.Reader, string, int64, int64, uint64) (*filer_pb.FileChunk, error) {
		return nil, errors.New("volume server down")
	}
	out := coalesceAppendTail(context.Background(), chunks, nil, committedIn(chunks), 2<<20, store.read, failing)
	if len(out) != len(chunks) {
		t.Fatalf("got %d chunks after a failed fold, want %d", len(out), len(chunks))
	}
}
