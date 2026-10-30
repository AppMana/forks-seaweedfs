package weed_server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/stats"
	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

type volumeCopyStatusServer struct {
	volume_server_pb.UnimplementedVolumeServerServer
	delegate       *VolumeServer
	failStatusCall int32
	statusErr      error
	statusCalls    atomic.Int32
}

func (s *volumeCopyStatusServer) VolumeStatus(ctx context.Context, req *volume_server_pb.VolumeStatusRequest) (*volume_server_pb.VolumeStatusResponse, error) {
	if s.statusCalls.Add(1) == s.failStatusCall {
		return nil, s.statusErr
	}
	return s.delegate.VolumeStatus(ctx, req)
}

func (s *volumeCopyStatusServer) ReadVolumeFileStatus(ctx context.Context, req *volume_server_pb.ReadVolumeFileStatusRequest) (*volume_server_pb.ReadVolumeFileStatusResponse, error) {
	return s.delegate.ReadVolumeFileStatus(ctx, req)
}

func (s *volumeCopyStatusServer) CopyFile(req *volume_server_pb.CopyFileRequest, stream volume_server_pb.VolumeServer_CopyFileServer) error {
	return s.delegate.CopyFile(req, stream)
}

func newVolumeCopyTestStore(t *testing.T, dir string) *storage.Store {
	t.Helper()
	store := storage.NewStore(
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		"127.0.0.1", 0, 0, "", "test-store",
		[]string{dir}, []int32{10}, []util.MinFreeSpace{{}},
		dir, storage.NeedleMapInMemory,
		[]types.DiskType{types.HardDriveType}, [][]string{nil},
		0, stats.DefaultDiskIOProbeConfig(),
	)
	store.Locations[0].AvailableSpace.Store(^uint64(0))
	t.Cleanup(store.Close)
	return store
}

func runVolumeCopyWithStatusFailure(t *testing.T, failStatusCall int32) (error, *storage.Store) {
	t.Helper()
	const vid = needle.VolumeId(43)

	sourceStore := newVolumeCopyTestStore(t, t.TempDir())
	if err := sourceStore.AddVolume(vid, "", storage.NeedleMapInMemory, "000", "", 0,
		needle.GetCurrentVersion(), 0, types.HardDriveType, 0); err != nil {
		t.Fatalf("add source volume: %v", err)
	}
	source := &volumeCopyStatusServer{
		delegate:       &VolumeServer{store: sourceStore},
		failStatusCall: failStatusCall,
		statusErr:      errors.New("source volume status unavailable"),
	}
	port := serveGrpc(t, func(server *grpc.Server) {
		volume_server_pb.RegisterVolumeServerServer(server, source)
	})

	targetStore := newVolumeCopyTestStore(t, t.TempDir())
	target := &VolumeServer{
		store:                         targetStore,
		grpcDialOption:                grpc.WithTransportCredentials(insecure.NewCredentials()),
		AllowUntrustedRemoteEndpoints: true,
	}
	err := target.VolumeCopy(&volume_server_pb.VolumeCopyRequest{
		VolumeId:       uint32(vid),
		SourceDataNode: fmt.Sprintf("127.0.0.1:%d.%d", port-10000, port),
	}, &fakeVolumeCopyStream{})
	return err, targetStore
}

func TestVolumeCopyContinuesWhenInitialStatusUnavailable(t *testing.T) {
	err, targetStore := runVolumeCopyWithStatusFailure(t, 1)
	if err != nil {
		t.Fatalf("VolumeCopy should continue when the initial status is unavailable: %v", err)
	}
	if targetStore.GetVolume(43) == nil {
		t.Fatal("copied volume was not mounted")
	}
}

func TestVolumeCopyFailsWhenFinalStatusUnavailable(t *testing.T) {
	err, targetStore := runVolumeCopyWithStatusFailure(t, 2)
	if err == nil || !strings.Contains(err.Error(), "status after copy") {
		t.Fatalf("VolumeCopy error = %v, want final status error", err)
	}
	if targetStore.GetVolume(43) != nil {
		t.Fatal("volume was mounted after final status validation failed")
	}
	select {
	case message := <-targetStore.NewVolumesChan:
		t.Fatalf("volume was announced after final status validation failed: %+v", message)
	default:
	}
	dataBaseFileName := storage.VolumeFileName(targetStore.Locations[0].Directory, "", 43)
	indexBaseFileName := storage.VolumeFileName(targetStore.Locations[0].IdxDirectory, "", 43)
	for _, fileName := range []string{
		dataBaseFileName + ".dat",
		indexBaseFileName + ".idx",
		dataBaseFileName + ".vif",
		dataBaseFileName + ".note",
	} {
		if _, statErr := os.Stat(fileName); !os.IsNotExist(statErr) {
			t.Fatalf("copy artifact %s remains after final status validation failed: %v", fileName, statErr)
		}
	}
}

func TestVolumeCopyKeepsExistingReplicaWhenDestinationFull(t *testing.T) {
	const vid = needle.VolumeId(44)

	sourceStore := newVolumeCopyTestStore(t, t.TempDir())
	if err := sourceStore.AddVolume(vid, "", storage.NeedleMapInMemory, "000", "", 0,
		needle.GetCurrentVersion(), 0, types.HardDriveType, 0); err != nil {
		t.Fatalf("add source volume: %v", err)
	}
	source := &volumeCopyStatusServer{
		delegate:       &VolumeServer{store: sourceStore},
		failStatusCall: 1,
		statusErr:      errors.New("source volume status unavailable"),
	}
	port := serveGrpc(t, func(server *grpc.Server) {
		volume_server_pb.RegisterVolumeServerServer(server, source)
	})

	targetStore := newVolumeCopyTestStore(t, t.TempDir())
	if err := targetStore.AddVolume(vid, "", storage.NeedleMapInMemory, "000", "", 0,
		needle.GetCurrentVersion(), 0, types.HardDriveType, 0); err != nil {
		t.Fatalf("add target volume: %v", err)
	}
	targetStore.Locations[0].AvailableSpace.Store(0)

	target := &VolumeServer{
		store:                         targetStore,
		grpcDialOption:                grpc.WithTransportCredentials(insecure.NewCredentials()),
		AllowUntrustedRemoteEndpoints: true,
	}
	err := target.VolumeCopy(&volume_server_pb.VolumeCopyRequest{
		VolumeId:       uint32(vid),
		SourceDataNode: fmt.Sprintf("127.0.0.1:%d.%d", port-10000, port),
	}, &fakeVolumeCopyStream{})
	if err == nil {
		t.Fatal("VolumeCopy should fail when no destination location is available")
	}
	if targetStore.GetVolume(vid) == nil {
		t.Fatal("existing replica was destroyed before a destination was reserved")
	}
}

func TestVolumeCopyReplacesReplicaAtSlotLimit(t *testing.T) {
	const vid = needle.VolumeId(45)

	sourceStore := newVolumeCopyTestStore(t, t.TempDir())
	if err := sourceStore.AddVolume(vid, "", storage.NeedleMapInMemory, "000", "", 0,
		needle.GetCurrentVersion(), 0, types.HardDriveType, 0); err != nil {
		t.Fatalf("add source volume: %v", err)
	}
	source := &volumeCopyStatusServer{
		delegate:       &VolumeServer{store: sourceStore},
		failStatusCall: 1,
		statusErr:      errors.New("source volume status unavailable"),
	}
	port := serveGrpc(t, func(server *grpc.Server) {
		volume_server_pb.RegisterVolumeServerServer(server, source)
	})

	targetStore := newVolumeCopyTestStore(t, t.TempDir())
	if err := targetStore.AddVolume(vid, "", storage.NeedleMapInMemory, "000", "", 0,
		needle.GetCurrentVersion(), 0, types.HardDriveType, 0); err != nil {
		t.Fatalf("add target volume: %v", err)
	}
	targetStore.Locations[0].MaxVolumeCount = 1

	target := &VolumeServer{
		store:                         targetStore,
		grpcDialOption:                grpc.WithTransportCredentials(insecure.NewCredentials()),
		AllowUntrustedRemoteEndpoints: true,
	}
	err := target.VolumeCopy(&volume_server_pb.VolumeCopyRequest{
		VolumeId:       uint32(vid),
		SourceDataNode: fmt.Sprintf("127.0.0.1:%d.%d", port-10000, port),
	}, &fakeVolumeCopyStream{})
	if err != nil {
		t.Fatalf("VolumeCopy should replace the replica at the slot limit: %v", err)
	}
	if targetStore.GetVolume(vid) == nil {
		t.Fatal("replaced volume was not mounted")
	}
}

// fakeVolumeCopyStream is a no-op VolumeServer_VolumeCopyServer; VolumeCopy
// errors out before sending anything in this test.
type fakeVolumeCopyStream struct {
	grpc.ServerStream
	sendErr error
}

func (s *fakeVolumeCopyStream) Send(*volume_server_pb.VolumeCopyResponse) error { return s.sendErr }
func (s *fakeVolumeCopyStream) Context() context.Context                        { return context.Background() }
func (s *fakeVolumeCopyStream) SetHeader(metadata.MD) error                     { return nil }
func (s *fakeVolumeCopyStream) SendHeader(metadata.MD) error                    { return nil }
func (s *fakeVolumeCopyStream) SetTrailer(metadata.MD)                          {}
func (s *fakeVolumeCopyStream) SendMsg(any) error                               { return nil }
func (s *fakeVolumeCopyStream) RecvMsg(any) error                               { return nil }

// TestVolumeCopy_KeepsExistingReplicaWhenSourceUnreachable verifies the
// verify-before-destroy invariant: a pre-existing healthy local replica must
// NOT be deleted when the source cannot be reached. The pre-fix code deleted
// the destination up front (and, on retry, could lose the volume entirely);
// the fix defers the delete until the source ReadVolumeFileStatus succeeds.
func TestVolumeCopy_KeepsExistingReplicaWhenSourceUnreachable(t *testing.T) {
	dir := t.TempDir()
	store := storage.NewStore(
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		"127.0.0.1", 0, 0, "", "test-store",
		[]string{dir}, []int32{10}, []util.MinFreeSpace{{}},
		dir, storage.NeedleMapInMemory,
		[]types.DiskType{types.HardDriveType}, [][]string{nil},
		0, stats.DiskIOProbeConfig{},
	)

	const vid = needle.VolumeId(42)
	if err := store.AddVolume(vid, "", storage.NeedleMapInMemory, "000", "", 0, needle.GetCurrentVersion(), 0, types.HardDriveType, 0); err != nil {
		t.Fatalf("AddVolume: %v", err)
	}
	if store.GetVolume(vid) == nil {
		t.Fatalf("setup: volume %d should exist", vid)
	}

	vs := &VolumeServer{
		store:                         store,
		grpcDialOption:                grpc.WithTransportCredentials(insecure.NewCredentials()),
		AllowUntrustedRemoteEndpoints: true,
	}

	// 127.0.0.1:1 is unreachable, so ReadVolumeFileStatus on the source fails.
	req := &volume_server_pb.VolumeCopyRequest{
		VolumeId:       uint32(vid),
		SourceDataNode: "127.0.0.1:1",
	}
	err := vs.VolumeCopy(req, &fakeVolumeCopyStream{})
	if err == nil {
		t.Fatalf("VolumeCopy should fail when the source is unreachable")
	}

	if store.GetVolume(vid) == nil {
		t.Fatalf("existing replica %d was destroyed before the source was verified", vid)
	}
}

// The copy and tail handlers dial a caller-supplied source address. With the
// default posture (AllowUntrustedRemoteEndpoints unset) a source on a blocked
// address must be rejected before any dial; the opt-out flag restores the old
// behavior for operators whose sources legitimately sit on those ranges.
func TestCopyTailHandlersRejectUntrustedSources(t *testing.T) {
	for _, source := range []string{"127.0.0.1:1.10001", "169.254.169.254:0.80"} {
		vs := &VolumeServer{
			grpcDialOption: grpc.WithTransportCredentials(insecure.NewCredentials()),
		}
		if err := vs.VolumeCopy(&volume_server_pb.VolumeCopyRequest{VolumeId: 1, SourceDataNode: source}, &fakeVolumeCopyStream{}); err == nil {
			t.Errorf("VolumeCopy accepted source %q", source)
		}
		if _, err := vs.VolumeEcShardsCopy(context.Background(), &volume_server_pb.VolumeEcShardsCopyRequest{VolumeId: 1, SourceDataNode: source}); err == nil {
			t.Errorf("VolumeEcShardsCopy accepted source %q", source)
		}
		if _, err := vs.VolumeTailReceiver(context.Background(), &volume_server_pb.VolumeTailReceiverRequest{VolumeId: 1, SourceVolumeServer: source}); err == nil {
			t.Errorf("VolumeTailReceiver accepted source %q", source)
		}
	}
}

// When a VolumeCopy fails after streaming its .dat (which settles the
// reservation via commitCopy as bytes land), removing the staged files must
// also restore the committed headroom on the destination DiskLocation so an
// immediate retry does not fail with "no space left".
func TestVolumeCopyRestoresCommittedHeadroomAfterPostStreamFailure(t *testing.T) {
	const vid = needle.VolumeId(46)

	sourceStore := newVolumeCopyTestStore(t, t.TempDir())
	if err := sourceStore.AddVolume(vid, "", storage.NeedleMapInMemory, "000", "", 0,
		needle.GetCurrentVersion(), 0, types.HardDriveType, 0); err != nil {
		t.Fatalf("add source volume: %v", err)
	}
	srcVol := sourceStore.GetVolume(vid)
	if srcVol == nil {
		t.Fatal("missing source volume")
	}
	n := &needle.Needle{
		Id:   1,
		Data: make([]byte, 64*1024),
	}
	n.Checksum = needle.NewCRC(n.Data)
	if _, err := sourceStore.WriteVolumeNeedle(vid, n, true, true); err != nil {
		t.Fatalf("write source needle: %v", err)
	}
	datSize, _, _ := srcVol.FileStat()

	source := &volumeCopyStatusServer{
		delegate:       &VolumeServer{store: sourceStore},
		failStatusCall: 2, // fail the post-copy VolumeStatus check on the first attempt
		statusErr:      errors.New("source volume status unavailable after copy"),
	}
	port := serveGrpc(t, func(server *grpc.Server) {
		volume_server_pb.RegisterVolumeServerServer(server, source)
	})

	targetDir := t.TempDir()
	diskStat := stats.NewDiskStatus(targetDir)
	// Leave headroom for slightly more than 1x datSize, but less than 2x datSize.
	budget := datSize + datSize/2
	var minFreeBytes uint64
	if diskStat.Free > budget {
		minFreeBytes = diskStat.Free - budget
	} else {
		t.Skipf("insufficient free disk space (%d) for test budget (%d)", diskStat.Free, budget)
	}

	targetStore := storage.NewStore(
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		"127.0.0.1", 0, 0, "", "test-store",
		[]string{targetDir}, []int32{10},
		[]util.MinFreeSpace{{Type: util.AsBytes, Bytes: minFreeBytes, Raw: fmt.Sprintf("%dB", minFreeBytes)}},
		targetDir, storage.NeedleMapInMemory,
		[]types.DiskType{types.HardDriveType}, [][]string{nil},
		0, stats.DefaultDiskIOProbeConfig(),
	)
	t.Cleanup(targetStore.Close)
	targetStore.Locations[0].AvailableSpace.Store(budget)

	target := &VolumeServer{
		store:                         targetStore,
		grpcDialOption:                grpc.WithTransportCredentials(insecure.NewCredentials()),
		AllowUntrustedRemoteEndpoints: true,
	}
	req := &volume_server_pb.VolumeCopyRequest{
		VolumeId:       uint32(vid),
		SourceDataNode: fmt.Sprintf("127.0.0.1:%d.%d", port-10000, port),
	}

	// First copy streams .dat (settling the reservation via commitCopy) and then
	// fails on post-copy VolumeStatus.
	if err := target.VolumeCopy(req, &fakeVolumeCopyStream{}); err == nil {
		t.Fatal("expected first VolumeCopy to fail on post-copy VolumeStatus")
	}
	if got := targetStore.Locations[0].ReservedSpace.Load(); got != 0 {
		t.Fatalf("ReservedSpace after failed copy = %d, want 0", got)
	}
	if got := targetStore.Locations[0].AvailableSpace.Load(); got <= datSize {
		t.Fatalf("AvailableSpace after failed copy = %d, want > %d so retry can proceed", got, datSize)
	}

	// Immediate retry must succeed without waiting for the 1-minute disk space probe.
	if err := target.VolumeCopy(req, &fakeVolumeCopyStream{}); err != nil {
		t.Fatalf("retry VolumeCopy after failed attempt failed: %v", err)
	}
	if targetStore.GetVolume(vid) == nil {
		t.Fatal("copied volume was not mounted on retry")
	}
}

