package weed_server

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/stats"
	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func newVolumeCopyTestStoreKind(t *testing.T, dir string, kind storage.NeedleMapKind) *storage.Store {
	t.Helper()
	store := storage.NewStore(
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		"127.0.0.1", 0, 0, "", "test-store",
		[]string{dir}, []int32{10}, []util.MinFreeSpace{{}},
		dir, kind,
		[]types.DiskType{types.HardDriveType}, [][]string{nil},
		0, stats.DefaultDiskIOProbeConfig(),
	)
	store.Locations[0].AvailableSpace.Store(^uint64(0))
	t.Cleanup(store.Close)
	return store
}

// newSourceWithDeletes builds a source volume that has served deletes since it
// was loaded: its counters come from runtime puts and deletes, while its .idx
// carries one tombstone row per delete.
func newSourceWithDeletes(t *testing.T, kind storage.NeedleMapKind, vid needle.VolumeId, puts, deletes int) *storage.Store {
	t.Helper()
	store := newVolumeCopyTestStoreKind(t, t.TempDir(), kind)
	if err := store.AddVolume(vid, "", kind, "000", "", 0,
		needle.GetCurrentVersion(), 0, types.HardDriveType, 0); err != nil {
		t.Fatalf("add source volume: %v", err)
	}
	for i := 1; i <= puts; i++ {
		n := &needle.Needle{Id: types.NeedleId(i), Cookie: 7, Data: []byte(fmt.Sprintf("payload %d", i))}
		n.Checksum = needle.NewCRC(n.Data)
		if _, err := store.WriteVolumeNeedle(vid, n, false, false); err != nil {
			t.Fatalf("write needle %d: %v", i, err)
		}
	}
	for i := 1; i <= deletes; i++ {
		n := &needle.Needle{Id: types.NeedleId(i), Cookie: 7}
		if _, err := store.DeleteVolumeNeedle(vid, n); err != nil {
			t.Fatalf("delete needle %d: %v", i, err)
		}
	}
	return store
}

func copyVolumeFrom(t *testing.T, source volume_server_pb.VolumeServerServer, kind storage.NeedleMapKind, vid needle.VolumeId) (error, *storage.Store) {
	t.Helper()
	targetStore := newVolumeCopyTestStoreKind(t, t.TempDir(), kind)
	return copyVolumeOnto(t, source, targetStore, vid), targetStore
}

func copyVolumeOnto(t *testing.T, source volume_server_pb.VolumeServerServer, targetStore *storage.Store, vid needle.VolumeId) error {
	t.Helper()
	port := serveGrpc(t, func(server *grpc.Server) {
		volume_server_pb.RegisterVolumeServerServer(server, source)
	})
	target := &VolumeServer{
		store:          targetStore,
		grpcDialOption: grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	return target.VolumeCopy(&volume_server_pb.VolumeCopyRequest{
		VolumeId:       uint32(vid),
		SourceDataNode: fmt.Sprintf("127.0.0.1:%d.%d", port-10000, port),
	}, &fakeVolumeCopyStream{})
}

func TestVolumeCopySucceedsForSourceWithDeletes(t *testing.T) {
	for _, kind := range []storage.NeedleMapKind{storage.NeedleMapInMemory, storage.NeedleMapLevelDb} {
		t.Run(fmt.Sprintf("kind=%d", kind), func(t *testing.T) {
			const vid = needle.VolumeId(46)
			sourceStore := newSourceWithDeletes(t, kind, vid, 20, 7)
			source := &volumeCopyStatusServer{delegate: &VolumeServer{store: sourceStore}}

			err, targetStore := copyVolumeFrom(t, source, kind, vid)
			if err != nil {
				t.Fatalf("VolumeCopy of a volume with deletes failed: %v", err)
			}
			targetVolume := targetStore.GetVolume(vid)
			if targetVolume == nil {
				t.Fatal("copied volume was not mounted")
			}
			sourceVolume := sourceStore.GetVolume(vid)
			if got, want := targetVolume.FileCount(), sourceVolume.FileCount(); got != want {
				t.Fatalf("target FileCount = %d, source FileCount = %d", got, want)
			}
			if got, want := targetVolume.DeletedCount(), sourceVolume.DeletedCount(); got != want {
				t.Fatalf("target DeletedCount = %d, source DeletedCount = %d", got, want)
			}
		})
	}
}

// truncatingCopyFileServer streams every file except .dat in full; the .dat
// loses its final byte, as an interrupted transfer would leave it.
type truncatingCopyFileServer struct {
	volumeCopyStatusServer
	datSize uint64
}

func (s *truncatingCopyFileServer) CopyFile(req *volume_server_pb.CopyFileRequest, stream volume_server_pb.VolumeServer_CopyFileServer) error {
	if req.Ext != ".dat" {
		return s.delegate.CopyFile(req, stream)
	}
	return s.delegate.CopyFile(req, &truncatingCopyFileStream{VolumeServer_CopyFileServer: stream, remaining: s.datSize - 1})
}

type truncatingCopyFileStream struct {
	volume_server_pb.VolumeServer_CopyFileServer
	remaining uint64
}

func (s *truncatingCopyFileStream) Send(resp *volume_server_pb.CopyFileResponse) error {
	if uint64(len(resp.FileContent)) > s.remaining {
		resp.FileContent = resp.FileContent[:s.remaining]
	}
	s.remaining -= uint64(len(resp.FileContent))
	return s.VolumeServer_CopyFileServer.Send(resp)
}

func TestVolumeCopyRejectsTruncatedDatFromSourceWithDeletes(t *testing.T) {
	const vid = needle.VolumeId(47)
	sourceStore := newSourceWithDeletes(t, storage.NeedleMapLevelDb, vid, 20, 7)
	datSize, _, _ := sourceStore.GetVolume(vid).FileStat()
	source := &truncatingCopyFileServer{
		volumeCopyStatusServer: volumeCopyStatusServer{delegate: &VolumeServer{store: sourceStore}},
		datSize:                datSize,
	}

	err, targetStore := copyVolumeFrom(t, source, storage.NeedleMapLevelDb, vid)
	if err == nil || !strings.Contains(err.Error(), "dat file size") {
		t.Fatalf("VolumeCopy error = %v, want dat file size mismatch", err)
	}
	if targetStore.GetVolume(vid) != nil {
		t.Fatal("truncated copy was mounted")
	}
}

// failingCopyFileServer streams the first half of the .dat and then fails the
// stream, as a source whose connection times out mid-copy does.
type failingCopyFileServer struct {
	volumeCopyStatusServer
	datSize uint64
}

func (s *failingCopyFileServer) CopyFile(req *volume_server_pb.CopyFileRequest, stream volume_server_pb.VolumeServer_CopyFileServer) error {
	if req.Ext != ".dat" {
		return s.delegate.CopyFile(req, stream)
	}
	if err := s.delegate.CopyFile(req, &truncatingCopyFileStream{VolumeServer_CopyFileServer: stream, remaining: s.datSize / 2}); err != nil {
		return err
	}
	return errors.New("connection timed out")
}

func readTestNeedle(t *testing.T, store *storage.Store, vid needle.VolumeId, id int) string {
	t.Helper()
	n := &needle.Needle{Id: types.NeedleId(id), Cookie: 7}
	if _, err := store.ReadVolumeNeedle(vid, n, nil, nil); err != nil {
		t.Fatalf("read needle %d of volume %d: %v", id, vid, err)
	}
	return string(n.Data)
}

func TestVolumeCopyKeepsExistingReplicaWhenSourceStreamFails(t *testing.T) {
	for _, kind := range []storage.NeedleMapKind{storage.NeedleMapInMemory, storage.NeedleMapLevelDb} {
		t.Run(fmt.Sprintf("kind=%d", kind), func(t *testing.T) {
			const vid = needle.VolumeId(48)
			sourceStore := newSourceWithDeletes(t, kind, vid, 20, 7)
			datSize, _, _ := sourceStore.GetVolume(vid).FileStat()
			source := &failingCopyFileServer{
				volumeCopyStatusServer: volumeCopyStatusServer{delegate: &VolumeServer{store: sourceStore}},
				datSize:                datSize,
			}
			targetStore := newSourceWithDeletes(t, kind, vid, 5, 0)
			existing := targetStore.GetVolume(vid)
			existingDatSize, existingIdxSize, _ := existing.FileStat()
			existingDat, existingIdx := existing.FileName(".dat"), existing.FileName(".idx")

			err := copyVolumeOnto(t, source, targetStore, vid)
			if err == nil || !strings.Contains(err.Error(), "connection timed out") {
				t.Fatalf("VolumeCopy error = %v, want the source stream failure", err)
			}

			v := targetStore.GetVolume(vid)
			if v == nil {
				t.Fatal("existing replica was unmounted by a failed copy")
			}
			if got := v.FileCount(); got != 5 {
				t.Fatalf("existing replica FileCount = %d, want 5", got)
			}
			if datSize, idxSize, _ := v.FileStat(); datSize != existingDatSize || idxSize != existingIdxSize {
				t.Fatalf("existing replica sizes = %d/%d, want %d/%d", datSize, idxSize, existingDatSize, existingIdxSize)
			}
			for _, name := range []string{existingDat, existingIdx} {
				if _, statErr := os.Stat(name); statErr != nil {
					t.Fatalf("existing replica file: %v", statErr)
				}
			}
			if got := readTestNeedle(t, targetStore, vid, 3); got != "payload 3" {
				t.Fatalf("existing replica needle 3 = %q", got)
			}
		})
	}
}

func TestVolumeCopyReplacesExistingReplica(t *testing.T) {
	for _, kind := range []storage.NeedleMapKind{storage.NeedleMapInMemory, storage.NeedleMapLevelDb} {
		t.Run(fmt.Sprintf("kind=%d", kind), func(t *testing.T) {
			const vid = needle.VolumeId(49)
			sourceStore := newSourceWithDeletes(t, kind, vid, 20, 7)
			source := &volumeCopyStatusServer{delegate: &VolumeServer{store: sourceStore}}
			targetDir := t.TempDir()
			targetStore := newVolumeCopyTestStoreKind(t, targetDir, kind)
			if err := targetStore.AddVolume(vid, "", kind, "000", "", 0,
				needle.GetCurrentVersion(), 0, types.HardDriveType, 0); err != nil {
				t.Fatalf("add target volume: %v", err)
			}
			for i := 1; i <= 3; i++ {
				n := &needle.Needle{Id: types.NeedleId(100 + i), Cookie: 7, Data: []byte("stale replica")}
				n.Checksum = needle.NewCRC(n.Data)
				if _, err := targetStore.WriteVolumeNeedle(vid, n, false, false); err != nil {
					t.Fatalf("write target needle: %v", err)
				}
			}

			if err := copyVolumeOnto(t, source, targetStore, vid); err != nil {
				t.Fatalf("VolumeCopy onto an existing replica: %v", err)
			}

			v := targetStore.GetVolume(vid)
			if v == nil {
				t.Fatal("copied volume was not mounted")
			}
			sourceVolume := sourceStore.GetVolume(vid)
			if got, want := v.FileCount(), sourceVolume.FileCount(); got != want {
				t.Fatalf("target FileCount = %d, source FileCount = %d", got, want)
			}
			if got := readTestNeedle(t, targetStore, vid, 12); got != "payload 12" {
				t.Fatalf("copied needle 12 = %q", got)
			}
			if _, err := targetStore.ReadVolumeNeedle(vid, &needle.Needle{Id: 101, Cookie: 7}, nil, nil); err == nil {
				t.Fatal("needle of the replaced replica is still readable")
			}
			entries, err := os.ReadDir(targetDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.IsDir() && entry.Name() != "49.ldb" {
					t.Fatalf("leftover directory %s after the copy", entry.Name())
				}
			}
		})
	}
}
