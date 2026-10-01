package shell

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type failingDiskCheckServer struct {
	volume_server_pb.UnimplementedVolumeServerServer
	failIndex bool
	divergent bool
	copies    atomic.Int32
}

func (s *failingDiskCheckServer) VolumeStatus(context.Context, *volume_server_pb.VolumeStatusRequest) (*volume_server_pb.VolumeStatusResponse, error) {
	return nil, status.Error(codes.Unavailable, "injected status failure")
}

func (s *failingDiskCheckServer) CopyFile(_ *volume_server_pb.CopyFileRequest, stream volume_server_pb.VolumeServer_CopyFileServer) error {
	call := s.copies.Add(1)
	if s.failIndex {
		return status.Error(codes.DataLoss, "injected index failure")
	}
	if s.divergent {
		entry := make([]byte, types.NeedleIdSize+types.OffsetSize+types.SizeSize)
		types.NeedleIdToBytes(entry, types.NeedleId(call))
		types.OffsetToBytes(entry[types.NeedleIdSize:], types.ToOffset(8))
		types.SizeToBytes(entry[types.NeedleIdSize+types.OffsetSize:], types.Size(10))
		return stream.Send(&volume_server_pb.CopyFileResponse{FileContent: entry})
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

func TestCheckAllReadOnlyReplicas(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		apply, failIndex, wantError bool
		wantCopies                  int32
	}{
		{"report-matching", false, false, false, 2},
		{"report-divergent", false, false, false, 2},
		{"report-index-failure", false, true, true, 1},
		{"refuse-repair-without-writable-source", true, false, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var mutations atomic.Int32
			server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
				mutations.Add(1) // This read-only fixture needs only streaming CopyFile.
				return handler(ctx, req)
			}))
			fake := &failingDiskCheckServer{failIndex: tc.failIndex, divergent: tc.name == "report-divergent"}
			volume_server_pb.RegisterVolumeServerServer(server, fake)
			go server.Serve(listener)
			t.Cleanup(server.Stop)
			var replicas []*VolumeReplica
			for i := 0; i < 2; i++ {
				replicas = append(replicas, &VolumeReplica{
					location: &location{dataNode: &master_pb.DataNodeInfo{Id: "127.0.0.1:8080", GrpcPort: uint32(listener.Addr().(*net.TCPAddr).Port)}},
					info:     &master_pb.VolumeInformationMessage{Id: 43, ReadOnly: true},
				})
			}
			var output bytes.Buffer
			vcd := &volumeCheckDisk{writer: &output, slowMode: true, fixReadOnly: true, applyChanges: tc.apply,
				ewg: NewErrorWaitGroup(1), commandEnv: &CommandEnv{option: &ShellOptions{GrpcDialOption: grpc.WithTransportCredentials(insecure.NewCredentials())}}}
			vcd.checkReadOnlyVolumes(map[uint32][]*VolumeReplica{43: replicas})
			err = vcd.ewg.Wait()
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
			if fake.copies.Load() != tc.wantCopies {
				t.Fatalf("index reads=%d want=%d", fake.copies.Load(), tc.wantCopies)
			}
			if mutations.Load() != 0 {
				t.Fatal("read-only comparison attempted unary/mutating RPC")
			}
			if fake.divergent && !strings.Contains(output.String(), "TWO-SIDED") {
				t.Fatalf("divergence not reported: %s", output.String())
			}
		})
	}
}
