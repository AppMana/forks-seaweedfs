package filer

import (
	"context"
	"errors"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/stretchr/testify/require"
)

func TestChunkGroupWriteFloorResolvesManifest(t *testing.T) {
	child := &filer_pb.FileChunk{FileId: "child", Size: 4, ModifiedTsNs: 900}
	fixture := newManifestReadFixture(t, map[string][]*filer_pb.FileChunk{"manifest": {child}}, nil)
	group, err := NewChunkGroup(fixture.lookup, nil, []*filer_pb.FileChunk{resolveTestManifest("manifest", 0)}, 1, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = group.Close() })
	floor, err := group.WriteTimestampFloor()
	require.NoError(t, err)
	require.Equal(t, int64(900), floor)
	require.NoError(t, group.AddChunk(&filer_pb.FileChunk{FileId: "next", Size: 4, ModifiedTsNs: 901}))
	floor, err = group.WriteTimestampFloor()
	require.NoError(t, err)
	require.Equal(t, int64(901), floor)
	require.NoError(t, group.SetChunks(nil))
	floor, err = group.WriteTimestampFloor()
	require.NoError(t, err)
	require.Zero(t, floor)
}

func TestChunkGroupWriteFloorRejectsUnresolvedManifest(t *testing.T) {
	want := errors.New("manifest unavailable")
	group, err := NewChunkGroup(func(context.Context, string) ([]string, error) { return nil, want }, nil,
		[]*filer_pb.FileChunk{resolveTestManifest("missing", 0)}, 1, nil, nil)
	require.Error(t, err)
	t.Cleanup(func() { _ = group.Close() })
	_, err = group.WriteTimestampFloor()
	require.Error(t, err)
}
