package weed_server

import (
	"bytes"
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/security"
	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func TestRepairNeedleRPCSafety(t *testing.T) {
	for _, tc := range []struct {
		name string
		want codes.Code
	}{
		{"absent", codes.OK}, {"existing", codes.AlreadyExists},
		{"denied", codes.PermissionDenied}, {"maintenance", codes.FailedPrecondition},
		{"missing-volume", codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs, _ := newMaintenanceModeServer(t, 7, "")
			require.NoError(t, vs.store.State.Update(&volume_server_pb.VolumeServerState{Version: vs.store.State.Proto().Version, Maintenance: tc.name == "maintenance"}))
			require.NoError(t, vs.store.AddVolume(9, "", storage.NeedleMapInMemory, "000", "", 0, needle.GetCurrentVersion(), 0, types.HardDriveType, 0))
			n := &needle.Needle{Id: 17, Cookie: 123, Data: []byte("preserved source payload")}
			n.Checksum = needle.NewCRC(n.Data)
			_, err := vs.store.WriteVolumeNeedle(9, n, true, true)
			require.NoError(t, err)
			donor := vs.store.GetVolume(9)
			idx, err := os.ReadFile(donor.IndexFileName() + ".idx")
			require.NoError(t, err)
			offset := types.BytesToOffset(idx[types.NeedleIdSize:]).ToActualOffset()
			blob, err := donor.ReadNeedleBlob(offset, n.Size)
			require.NoError(t, err)
			if tc.name == "existing" {
				old := &needle.Needle{Id: 17, Cookie: 456, Data: []byte("intact target payload")}
				old.Checksum = needle.NewCRC(old.Data)
				_, err := vs.store.WriteVolumeNeedle(7, old, true, true)
				require.NoError(t, err)
			}
			if tc.name == "denied" {
				vs.guard = security.NewGuard([]string{"192.0.2.1"}, "", 0, "", 0)
			}
			target := vs.store.GetVolume(7)
			before, err := os.ReadFile(target.DataFileName() + ".dat")
			require.NoError(t, err)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			server := grpc.NewServer()
			volume_server_pb.RegisterVolumeServerServer(server, vs)
			go server.Serve(listener)
			t.Cleanup(server.Stop)
			conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			require.NoError(t, err)
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := &volume_server_pb.WriteNeedleBlobRequest{VolumeId: 7, NeedleId: 17, Size: int32(n.Size), NeedleBlob: blob}
			if tc.name == "missing-volume" {
				req.VolumeId = 999
			}
			err = conn.Invoke(ctx, "/volume_server_pb.VolumeServer/WriteNeedleBlobIfAbsent", req, &volume_server_pb.WriteNeedleBlobResponse{})
			require.Equal(t, tc.want, status.Code(err), "%v", err)
			if tc.want != codes.OK {
				after, err := os.ReadFile(target.DataFileName() + ".dat")
				require.NoError(t, err)
				require.True(t, bytes.Equal(before, after), "rejected RPC changed intact data")
			} else {
				err = conn.Invoke(ctx, "/volume_server_pb.VolumeServer/WriteNeedleBlobIfAbsent", req, &volume_server_pb.WriteNeedleBlobResponse{})
				require.Equal(t, codes.AlreadyExists, status.Code(err), "retry must not overwrite")
			}
		})
	}
}
