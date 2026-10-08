package filer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// checkpointChunks is the shape of the CreateEntry that took down a filer:
// 301 plain 2 MiB chunks, the first one overwritten by a later write.
func checkpointChunks() []*filer_pb.FileChunk {
	const chunkSize = 2 << 20
	chunks := make([]*filer_pb.FileChunk, 0, 301)
	for i := 0; i < 300; i++ {
		chunks = append(chunks, &filer_pb.FileChunk{
			FileId:       fmt.Sprintf("68257,%x", i+1),
			Offset:       int64(i) * chunkSize,
			Size:         chunkSize,
			ModifiedTsNs: int64(i + 1),
		})
	}
	return append(chunks, &filer_pb.FileChunk{FileId: "68257,overwrite", Offset: 0, Size: chunkSize, ModifiedTsNs: 1000})
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func fileIds(chunks []*filer_pb.FileChunk) []string {
	ids := make([]string, 0, len(chunks))
	for _, c := range chunks {
		ids = append(ids, c.GetFileIdString())
	}
	return ids
}

// A mount whose gRPC stream closes while the filer compacts its CreateEntry
// cancels the request context. Plain chunks need no reads to compact, so the
// result must not depend on the context.
func TestCompactFileChunksIgnoresCanceledContextForPlainChunks(t *testing.T) {
	chunks := checkpointChunks()
	wantCompacted, wantGarbage := CompactFileChunks(context.Background(), nil, chunks)
	require.Len(t, wantGarbage, 1)

	compacted, garbage := CompactFileChunks(canceledContext(), nil, chunks)

	assert.Equal(t, fileIds(wantCompacted), fileIds(compacted))
	assert.Equal(t, fileIds(wantGarbage), fileIds(garbage))
}

// When a manifest cannot be resolved nothing is known to be covered, so no
// chunk may be reported as garbage: the caller deletes garbage chunks.
func TestCompactFileChunksKeepsEveryChunkWhenManifestResolveFails(t *testing.T) {
	failing := func(ctx context.Context, fileId string) ([]string, error) {
		return nil, errors.New("volume server unreachable")
	}
	chunks := []*filer_pb.FileChunk{
		{FileId: "7,manifest", IsChunkManifest: true, Offset: 0, Size: 100},
		{FileId: "7,data", Offset: 100, Size: 10, ModifiedTsNs: 1},
	}

	compacted, garbage := CompactFileChunks(context.Background(), failing, chunks)

	assert.Equal(t, fileIds(chunks), fileIds(compacted))
	assert.Empty(t, garbage)
}

func TestViewFromChunksIgnoresCanceledContextForPlainChunks(t *testing.T) {
	chunks := checkpointChunks()
	want := ViewFromChunks(context.Background(), nil, chunks, 0, math.MaxInt64)

	got := ViewFromChunks(canceledContext(), nil, chunks, 0, math.MaxInt64)

	require.NotNil(t, got)
	assert.Equal(t, want.Len(), got.Len())
	for w, g := want.Front(), got.Front(); w != nil && g != nil; w, g = w.Next, g.Next {
		assert.Equal(t, *w.Value, *g.Value)
	}
}

// A stream reader over a file whose manifest cannot be resolved must report
// the failure, not panic and not read as an empty file.
func TestChunkStreamReaderReportsManifestResolveFailure(t *testing.T) {
	resolveErr := errors.New("volume server unreachable")
	failing := func(ctx context.Context, fileId string) ([]string, error) {
		return nil, resolveErr
	}
	chunks := []*filer_pb.FileChunk{{FileId: "7,manifest", IsChunkManifest: true, Offset: 0, Size: 100}}

	reader := NewChunkStreamReaderFromLookup(context.Background(), failing, chunks)

	_, err := io.ReadAll(reader)
	require.Error(t, err)
	assert.ErrorIs(t, reader.SourceError(), resolveErr)
}
