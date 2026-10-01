package shell

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type repairIndexClient struct {
	volume_server_pb.VolumeServerClient
	info  *volume_server_pb.ReadVolumeFileStatusResponse
	data  []byte
	calls int
	fault string
	chunk int
}

func (c *repairIndexClient) ReadVolumeFileStatus(context.Context, *volume_server_pb.ReadVolumeFileStatusRequest, ...grpc.CallOption) (*volume_server_pb.ReadVolumeFileStatusResponse, error) {
	c.calls++
	if c.fault == "status-error" {
		return nil, errors.New("status unavailable")
	}
	result := proto.Clone(c.info).(*volume_server_pb.ReadVolumeFileStatusResponse)
	if c.calls > 1 && c.fault == "compacted" {
		result.CompactionRevision++
	}
	return result, nil
}
func (c *repairIndexClient) CopyFile(_ context.Context, r *volume_server_pb.CopyFileRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[volume_server_pb.CopyFileResponse], error) {
	if r.StopOffset != c.info.IdxFileSize || r.IgnoreSourceFileNotFound || r.IsEcVolume || r.CompactionRevision != c.info.CompactionRevision {
		return nil, errors.New("unsafe index request")
	}
	if c.fault == "copy-error" {
		return nil, errors.New("copy unavailable")
	}
	return &repairIndexStream{data: c.data, fail: c.fault == "stream-error", chunk: c.chunk}, nil
}

type repairIndexStream struct {
	grpc.ClientStream
	data  []byte
	fail  bool
	chunk int
}

func (s *repairIndexStream) Recv() (*volume_server_pb.CopyFileResponse, error) {
	if s.fail {
		return nil, errors.New("stream interrupted")
	}
	if len(s.data) == 0 {
		return nil, io.EOF
	}
	n := 3
	if s.chunk > 0 {
		n = s.chunk
	}
	if len(s.data) < n {
		n = len(s.data)
	}
	result := &volume_server_pb.CopyFileResponse{FileContent: s.data[:n]}
	s.data = s.data[n:]
	return result, nil
}

func TestRepairIndexBatchedRowsPreserveMatchedSize(t *testing.T) {
	const row = types.NeedleIdSize + types.OffsetSize + types.SizeSize
	data := make([]byte, row*2)
	types.NeedleIdToBytes(data, 17)
	types.OffsetToBytes(data[types.NeedleIdSize:], types.ToOffset(128))
	types.SizeToBytes(data[types.NeedleIdSize+types.OffsetSize:], 25)
	types.NeedleIdToBytes(data[row:], 18)
	types.OffsetToBytes(data[row+types.NeedleIdSize:], types.ToOffset(256))
	types.SizeToBytes(data[row+types.NeedleIdSize+types.OffsetSize:], 4096)
	for _, chunk := range []int{3, row, row + 1, len(data)} {
		c := &repairIndexClient{data: data, chunk: chunk, info: &volume_server_pb.ReadVolumeFileStatusResponse{VolumeId: 41, Version: 3, IdxFileSize: uint64(len(data))}}
		_, offset, size, found, err := repairIndex(context.Background(), c, needle.NewFileId(41, 17, 123))
		if err != nil || !found || size != 25 || offset.ToActualOffset() != 128 {
			t.Errorf("chunk=%d found=%v size=%d offset=%d err=%v", chunk, found, size, offset.ToActualOffset(), err)
		}
	}
}

func TestRepairIndexNeverConfusesErrorsOrTombstonesWithAbsence(t *testing.T) {
	fid := needle.NewFileId(41, 17, 123)
	row := func(size types.Size) []byte {
		b := make([]byte, types.NeedleIdSize+types.OffsetSize+types.SizeSize)
		types.NeedleIdToBytes(b, fid.Key)
		types.OffsetToBytes(b[types.NeedleIdSize:], types.ToOffset(128))
		types.SizeToBytes(b[types.NeedleIdSize+types.OffsetSize:], size)
		return b
	}
	for _, state := range []string{"live", "absent", "deleted", "status-error", "copy-error", "stream-error", "truncated", "extra", "compacted", "version", "identity"} {
		t.Run(state, func(t *testing.T) {
			data := row(25)
			if state == "absent" {
				data = nil
			}
			if state == "deleted" {
				data = append(data, row(-1)...)
			}
			c := &repairIndexClient{data: data, fault: state, info: &volume_server_pb.ReadVolumeFileStatusResponse{VolumeId: 41, Version: 3, IdxFileSize: uint64(len(data)), CompactionRevision: 2}}
			switch state {
			case "truncated":
				c.data = c.data[:len(c.data)-1]
			case "extra":
				c.data = append(c.data, 0)
			case "version":
				c.info.Version = 0
			case "identity":
				c.info.VolumeId = 42
			}
			_, _, size, found, err := repairIndex(context.Background(), c, fid)
			valid := state == "live" || state == "absent" || state == "deleted"
			if (err == nil) != valid {
				t.Fatalf("state=%s err=%v", state, err)
			}
			if valid && found != (state != "absent") {
				t.Fatalf("state=%s found=%v", state, found)
			}
			if state == "deleted" && size != -1 {
				t.Fatalf("lost final tombstone: %d", size)
			}
		})
	}
}
