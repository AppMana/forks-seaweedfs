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

// checkpointChunks builds 301 plain 2 MiB chunks with the first chunk
// overwritten by a later write.
func checkpointChunks() []*filer_pb.FileChunk {
	const chunkSize = 2 << 20
	chunks := make([]*filer_pb.FileChunk, 0, 301)
	for i := 0; i < 300; i++ {
		chunks = append(chunks, &filer_pb.FileChunk{
			FileId:       fmt.Sprintf("1,%x", i+1),
			Offset:       int64(i) * chunkSize,
			Size:         chunkSize,
			ModifiedTsNs: int64(i + 1),
		})
	}
	return append(chunks, &filer_pb.FileChunk{FileId: "1,overwrite", Offset: 0, Size: chunkSize, ModifiedTsNs: 1000})
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// A mount whose gRPC stream closes while the filer compacts its CreateEntry
// cancels the request context. The request must fail with the cancellation,
// not panic, and must report no garbage: the caller deletes garbage chunks.
func TestCompactFileChunksFailsWithoutGarbageUnderCanceledContext(t *testing.T) {
	chunks := checkpointChunks()
	_, wantGarbage, err := CompactFileChunks(context.Background(), nil, chunks)
	require.NoError(t, err)
	require.Len(t, wantGarbage, 1)

	compacted, garbage, err := CompactFileChunks(canceledContext(), nil, chunks)

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, compacted)
	assert.Empty(t, garbage)
}

// When a manifest cannot be resolved nothing is known to be covered, so no
// chunk may be reported as garbage: the caller deletes garbage chunks.
func TestCompactFileChunksReportsNoGarbageWhenManifestResolveFails(t *testing.T) {
	resolveErr := errors.New("volume server unreachable")
	failing := func(ctx context.Context, fileId string) ([]string, error) {
		return nil, resolveErr
	}
	chunks := []*filer_pb.FileChunk{
		{FileId: "7,manifest", IsChunkManifest: true, Offset: 0, Size: 100},
		{FileId: "7,data", Offset: 100, Size: 10, ModifiedTsNs: 1},
	}

	_, garbage, err := CompactFileChunks(context.Background(), failing, chunks)

	require.Error(t, err)
	assert.Empty(t, garbage)
}

func TestViewFromChunksFailsUnderCanceledContext(t *testing.T) {
	chunks := checkpointChunks()

	got, err := ViewFromChunks(canceledContext(), nil, chunks, 0, math.MaxInt64)

	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, got)
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
