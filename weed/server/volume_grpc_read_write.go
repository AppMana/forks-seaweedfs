package weed_server

import (
	"context"
	"errors"
	"fmt"

	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/stats"
	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// WriteNeedleBlobIfAbsent deliberately is not a flag on WriteNeedleBlob:
// an older server would ignore a new field and could overwrite intact data.
func (vs *VolumeServer) WriteNeedleBlobIfAbsent(ctx context.Context, req *volume_server_pb.WriteNeedleBlobRequest) (*volume_server_pb.WriteNeedleBlobResponse, error) {
	if err := vs.checkGrpcAdminAuth(ctx); err != nil {
		return nil, err
	}
	if err := vs.CheckMaintenanceMode(); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "missing repair request")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	v := vs.store.GetVolume(needle.VolumeId(req.VolumeId))
	if v == nil {
		return nil, status.Errorf(codes.NotFound, "volume %d not found", req.VolumeId)
	}
	if err := v.WriteNeedleBlobIfAbsent(types.NeedleId(req.NeedleId), req.NeedleBlob, types.Size(req.Size)); err != nil {
		stats.VolumeServerFileWriteFailures.Inc()
		if errors.Is(err, storage.ErrNeedleAlreadyExists) {
			return nil, status.Error(codes.AlreadyExists, err.Error())
		}
		return nil, err
	}
	return &volume_server_pb.WriteNeedleBlobResponse{}, nil
}

func (vs *VolumeServer) ReadNeedleBlob(ctx context.Context, req *volume_server_pb.ReadNeedleBlobRequest) (resp *volume_server_pb.ReadNeedleBlobResponse, err error) {
	if err := func() error {
		resp = &volume_server_pb.ReadNeedleBlobResponse{}
		v := vs.store.GetVolume(needle.VolumeId(req.VolumeId))
		if v == nil {
			return fmt.Errorf("not found volume id %d", req.VolumeId)
		}

		resp.NeedleBlob, err = v.ReadNeedleBlob(req.Offset, types.Size(req.Size))
		if err != nil {
			return fmt.Errorf("read needle blob offset %d size %d: %v", req.Offset, req.Size, err)
		}

		return nil
	}(); err != nil {
		stats.VolumeServerFileReadFailures.Inc()
		return nil, err
	}

	return resp, nil
}

func (vs *VolumeServer) ReadNeedleMeta(ctx context.Context, req *volume_server_pb.ReadNeedleMetaRequest) (resp *volume_server_pb.ReadNeedleMetaResponse, err error) {
	resp = &volume_server_pb.ReadNeedleMetaResponse{}
	volumeId := needle.VolumeId(req.VolumeId)

	if err := func() error {
		n := &needle.Needle{
			Id:    types.NeedleId(req.NeedleId),
			Flags: 0x08,
		}
		size := req.Size
		offset := req.Offset

		hasVolume := vs.store.HasVolume(volumeId)
		if !hasVolume {
			return fmt.Errorf("not found volume id %d and read needle metadata at ec shards is not supported", req.VolumeId)
		}
		err = vs.store.ReadVolumeNeedleMetaAt(volumeId, n, offset, size)
		if err != nil {
			return err
		}

		resp.Cookie = uint32(n.Cookie)
		resp.LastModified = n.LastModified
		resp.Crc = n.Checksum.Value()
		if n.HasTtl() {
			resp.Ttl = n.Ttl.String()
		}
		resp.AppendAtNs = n.AppendAtNs

		return nil
	}(); err != nil {
		stats.VolumeServerFileReadFailures.Inc()
		return nil, err
	}

	return resp, nil
}

func (vs *VolumeServer) WriteNeedleBlob(ctx context.Context, req *volume_server_pb.WriteNeedleBlobRequest) (resp *volume_server_pb.WriteNeedleBlobResponse, err error) {
	if err := vs.CheckMaintenanceMode(); err != nil {
		return nil, err
	}

	resp = &volume_server_pb.WriteNeedleBlobResponse{}

	if err := func() error {
		v := vs.store.GetVolume(needle.VolumeId(req.VolumeId))
		if v == nil {
			return fmt.Errorf("not found volume id %d", req.VolumeId)
		}
		if err = v.WriteNeedleBlob(types.NeedleId(req.NeedleId), req.NeedleBlob, types.Size(req.Size)); err != nil {
			return fmt.Errorf("write blob needle %d size %d: %v", req.NeedleId, req.Size, err)
		}

		return nil
	}(); err != nil {
		stats.VolumeServerFileWriteFailures.Inc()
		return nil, err
	}

	return resp, nil
}
