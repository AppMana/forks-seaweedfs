package shell

import (
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type failingDiskCheckServer struct {
	volume_server_pb.UnimplementedVolumeServerServer
	failIndex bool
	copies    atomic.Int32
}

func (s *failingDiskCheckServer) VolumeStatus(context.Context, *volume_server_pb.VolumeStatusRequest) (*volume_server_pb.VolumeStatusResponse, error) {
	return nil, status.Error(codes.Unavailable, "injected status failure")
}

func (s *failingDiskCheckServer) CopyFile(_ *volume_server_pb.CopyFileRequest, _ volume_server_pb.VolumeServer_CopyFileServer) error {
	s.copies.Add(1)
	if s.failIndex {
		return status.Error(codes.DataLoss, "injected index failure")
	}
	return nil
}

func TestCheckWritableVolumesPropagatesFailures(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		slow, failIndex, wantError bool
	}{
		{"status-failure", false, false, true},
		{"index-failure", true, true, true},
		{"matching-empty-indices", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			fake := &failingDiskCheckServer{failIndex: tc.failIndex}
			volume_server_pb.RegisterVolumeServerServer(server, fake)
			go server.Serve(listener)
			t.Cleanup(server.Stop)
			port := uint32(listener.Addr().(*net.TCPAddr).Port)
			now := time.Now()
			replicas := map[uint32][]*VolumeReplica{}
			for _, vid := range []uint32{41, 42} {
				for _, count := range []uint64{2, 1} {
					replicas[vid] = append(replicas[vid], &VolumeReplica{
						location: &location{dataNode: &master_pb.DataNodeInfo{Id: "127.0.0.1:8080", GrpcPort: port}},
						info:     &master_pb.VolumeInformationMessage{Id: vid, FileCount: count, ModifiedAtSecond: now.Unix()},
					})
				}
			}
			vcd := &volumeCheckDisk{writer: io.Discard, now: now, slowMode: tc.slow,
				commandEnv: &CommandEnv{option: &ShellOptions{GrpcDialOption: grpc.WithTransportCredentials(insecure.NewCredentials())}}}
			err = vcd.checkWritableVolumes(replicas)
			if !tc.wantError {
				if err != nil || fake.copies.Load() != 4 {
					t.Fatalf("matching replicas: err=%v index reads=%d, want nil/4", err, fake.copies.Load())
				}
				return
			}
			if err == nil {
				t.Fatal("failed replica checks reported success")
			}
			for _, vid := range []string{"41", "42"} {
				if !strings.Contains(err.Error(), vid) {
					t.Errorf("missing volume %s in error: %v", vid, err)
				}
			}
			if !tc.slow && fake.copies.Load() != 0 {
				t.Fatal("attempted index synchronization after failed status preflight")
			}
		})
	}
}
