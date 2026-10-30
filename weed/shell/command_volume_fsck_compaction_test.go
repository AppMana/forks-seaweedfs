package shell

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/operation"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle_map"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type fakeVolumeNeedle struct {
	key     uint64
	size    types.Size
	deleted bool
}

// fakeCompactingVolumeServer holds one volume whose .dat starts with
// garbage, so a vacuum moves every live needle to a lower offset and bumps
// the compaction revision, as the master's periodic vacuum does while
// volume.fsck walks the filer. Needle metadata reads check the offset holds
// the requested needle, as Volume.readNeedleMetaAt does.
type fakeCompactingVolumeServer struct {
	volume_server_pb.UnimplementedVolumeServerServer
	volumeId uint32

	lock          sync.Mutex
	revision      uint32
	garbage       bool
	needles       []fakeVolumeNeedle
	damaged       map[uint64]bool
	compactOnCopy bool
	copies        int
	deleted       []uint64
}

const fakeNeedleSpan = 1024

func (s *fakeCompactingVolumeServer) offsetOf(key uint64) (int64, bool) {
	position := int64(1)
	if s.garbage {
		position++
	}
	for _, n := range s.needles {
		if n.key == key {
			return position * fakeNeedleSpan, true
		}
		position++
	}
	return 0, false
}

// compact drops the garbage and every deleted needle, moving the rest.
func (s *fakeCompactingVolumeServer) compact() {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.compactLocked()
}

func (s *fakeCompactingVolumeServer) compactLocked() {
	live := s.needles[:0]
	for _, n := range s.needles {
		if !n.deleted {
			live = append(live, n)
		}
	}
	s.needles = live
	s.garbage = false
	s.revision++
}

func (s *fakeCompactingVolumeServer) damage(key uint64) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.damaged[key] = true
}

func (s *fakeCompactingVolumeServer) compactOnEveryCopy() {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.compactOnCopy = true
}

func (s *fakeCompactingVolumeServer) deleteNeedle(key uint64) {
	s.lock.Lock()
	defer s.lock.Unlock()
	for i := range s.needles {
		if s.needles[i].key == key {
			s.needles[i].deleted = true
		}
	}
}

func (s *fakeCompactingVolumeServer) ReadVolumeFileStatus(ctx context.Context, req *volume_server_pb.ReadVolumeFileStatusRequest) (*volume_server_pb.ReadVolumeFileStatusResponse, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if req.VolumeId != s.volumeId {
		return nil, fmt.Errorf("not found volume id %d", req.VolumeId)
	}
	return &volume_server_pb.ReadVolumeFileStatusResponse{VolumeId: s.volumeId, CompactionRevision: s.revision}, nil
}

func (s *fakeCompactingVolumeServer) CopyFile(req *volume_server_pb.CopyFileRequest, stream volume_server_pb.VolumeServer_CopyFileServer) error {
	s.lock.Lock()
	if req.VolumeId != s.volumeId || req.Ext != ".idx" {
		s.lock.Unlock()
		return fmt.Errorf("not found volume id %d%s", req.VolumeId, req.Ext)
	}
	if req.CompactionRevision != s.revision && req.CompactionRevision != ^uint32(0) {
		s.lock.Unlock()
		return fmt.Errorf("volume %d is compacted", req.VolumeId)
	}
	var idx bytes.Buffer
	for _, n := range s.needles {
		offset, _ := s.offsetOf(n.key)
		idx.Write(needle_map.ToBytes(types.NeedleId(n.key), types.ToOffset(offset), n.size))
		if n.deleted {
			idx.Write(needle_map.ToBytes(types.NeedleId(n.key), types.ToOffset(offset), types.TombstoneFileSize))
		}
	}
	s.copies++
	if s.compactOnCopy {
		// a client overwrites the first needle, appending it again, and the
		// vacuum then moves every needle after its old copy
		s.needles = append(s.needles[1:], s.needles[0])
		s.compactLocked()
	}
	s.lock.Unlock()
	return stream.Send(&volume_server_pb.CopyFileResponse{FileContent: idx.Bytes(), ModifiedTsNs: 1})
}

func (s *fakeCompactingVolumeServer) ReadNeedleMeta(ctx context.Context, req *volume_server_pb.ReadNeedleMetaRequest) (*volume_server_pb.ReadNeedleMetaResponse, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.damaged[req.NeedleId] {
		return nil, fmt.Errorf("read needle %d: input/output error", req.NeedleId)
	}
	if offset, found := s.offsetOf(req.NeedleId); found && offset == req.Offset {
		return &volume_server_pb.ReadNeedleMetaResponse{AppendAtNs: 1}, nil
	}
	for _, n := range s.needles {
		if offset, _ := s.offsetOf(n.key); offset == req.Offset {
			return nil, fmt.Errorf("needle identity mismatch at offset %d: found %d, expected %d", req.Offset, n.key, req.NeedleId)
		}
	}
	return nil, fmt.Errorf("read needle meta at offset %d: EOF", req.Offset)
}

func (s *fakeCompactingVolumeServer) BatchDelete(ctx context.Context, req *volume_server_pb.BatchDeleteRequest) (*volume_server_pb.BatchDeleteResponse, error) {
	s.lock.Lock()
	defer s.lock.Unlock()
	resp := &volume_server_pb.BatchDeleteResponse{}
	for _, fid := range req.FileIds {
		_, idCookie, err := operation.ParseFileId(fid)
		if err != nil {
			return nil, err
		}
		id, _, err := needle.ParseNeedleIdCookie(idCookie)
		if err != nil {
			return nil, err
		}
		parsed := uint64(id)
		status := http.StatusNotFound
		for i := range s.needles {
			if s.needles[i].key == parsed && !s.needles[i].deleted {
				s.needles[i].deleted = true
				s.deleted = append(s.deleted, parsed)
				status = http.StatusAccepted
			}
		}
		result := &volume_server_pb.DeleteResult{FileId: fid, Status: int32(status)}
		if status != http.StatusAccepted {
			result.Error = "not found"
		}
		resp.Results = append(resp.Results, result)
	}
	return resp, nil
}

func startFakeCompactingVolumeServer(t *testing.T, volumeId uint32, keys ...uint64) (*fakeCompactingVolumeServer, pb.ServerAddress) {
	t.Helper()
	s := &fakeCompactingVolumeServer{volumeId: volumeId, garbage: true, damaged: map[uint64]bool{}}
	for _, key := range keys {
		s.needles = append(s.needles, fakeVolumeNeedle{key: key, size: types.Size(100 + key)})
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	volume_server_pb.RegisterVolumeServerServer(srv, s)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return s, pb.ServerAddress(fmt.Sprintf("127.0.0.1:1.%d", lis.Addr().(*net.TCPAddr).Port))
}

// compactionFsck is a volume.fsck over volume 7 on replicas a and b served
// by fake volume servers. The filer references refKeys; duringWalk runs
// while the filer is walked, after both indexes were copied.
func compactionFsck(t *testing.T, refKeys []uint64, out *bytes.Buffer, duringWalk func()) (c *commandVolumeFsck, a, b *fakeCompactingVolumeServer, volumes map[string]map[uint32]VInfo) {
	t.Helper()
	const volumeId = uint32(7)
	keys := []uint64{1, 2, 3, 4, 5, 6}
	a, addrA := startFakeCompactingVolumeServer(t, volumeId, keys...)
	b, addrB := startFakeCompactingVolumeServer(t, volumeId, keys...)
	volumes = map[string]map[uint32]VInfo{
		"dnA": {volumeId: {server: addrA, collection: "b"}},
		"dnB": {volumeId: {server: addrB, collection: "b"}},
	}
	verbose, force, skipEc, verify, findMissing := false, false, false, false, false
	collection := ""
	c = &commandVolumeFsck{
		env:                      &CommandEnv{option: &ShellOptions{GrpcDialOption: grpc.WithTransportCredentials(insecure.NewCredentials())}},
		writer:                   out,
		tempFolder:               t.TempDir(),
		verbose:                  &verbose,
		forcePurging:             &force,
		skipEcVolumes:            &skipEc,
		verifyNeedle:             &verify,
		findMissingChunksInFiler: &findMissing,
		collection:               &collection,
		walkFiler: func(path string, genFn func(context.Context, *filer_pb.FullEntry, chan interface{}) error, saveFn func(chan interface{}) error) error {
			ch := make(chan interface{}, len(refKeys))
			saved := make(chan error, 1)
			go func() { saved <- saveFn(ch) }()
			for _, key := range refKeys {
				entry := &filer_pb.FullEntry{Dir: "/buckets/b", Entry: &filer_pb.Entry{Name: fmt.Sprintf("f%d", key), Chunks: []*filer_pb.FileChunk{{
					Fid:  &filer_pb.FileId{VolumeId: volumeId, FileKey: key},
					Size: 10,
				}}}}
				if err := genFn(context.Background(), entry, ch); err != nil {
					t.Fatal(err)
				}
			}
			if duringWalk != nil {
				duringWalk()
			}
			close(ch)
			return <-saved
		},
		volumeServers: func(uint32) ([]pb.ServerAddress, bool) {
			return []pb.ServerAddress{addrA, addrB}, true
		},
	}
	for dataNodeId, vids := range volumes {
		for vid, vinfo := range vids {
			if err := c.collectOneVolumeFileIds(dataNodeId, vid, vinfo); err != nil {
				t.Fatalf("copy index of %s: %v", dataNodeId, err)
			}
		}
	}
	return
}

func sortedNeedleIds(keys []uint64) string {
	sorted := append([]uint64(nil), keys...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return fmt.Sprint(sorted)
}

// Compaction during the filer walk requires a fresh index copy.
// Needles deleted and vacuumed in the meantime must not be purged.
func TestFsckRereadsNeedleMetadataAfterVolumeCompaction(t *testing.T) {
	var out bytes.Buffer
	var a, b *fakeCompactingVolumeServer
	c, a, b, volumes := compactionFsck(t, []uint64{1, 2, 4}, &out, func() {
		a.deleteNeedle(6)
		b.deleteNeedle(6)
		a.compact()
	})

	err := c.checkVolumes(volumes, fsckChecks{applyPurging: true, cutoffFromAtNs: time.Now().UnixNano(), maxOrphanPercent: 100})
	if err != nil {
		t.Fatalf("fsck: %v\n%s", err, out.String())
	}
	if got := sortedNeedleIds(a.deleted); got != "[3 5]" {
		t.Fatalf("replica a purged %s, want [3 5]\n%s", got, out.String())
	}
	if got := sortedNeedleIds(b.deleted); got != "[3 5]" {
		t.Fatalf("replica b purged %s, want [3 5]\n%s", got, out.String())
	}
	if a.copies != 2 || b.copies != 1 {
		t.Fatalf("index copies a=%d b=%d, want the compacted replica copied again only\n%s", a.copies, b.copies, out.String())
	}
	if !strings.Contains(out.String(), "compacted") {
		t.Fatalf("the re-copy must be reported:\n%s", out.String())
	}
}

// A read that fails while the volume's compaction revision is unchanged is
// not explained by a vacuum: the scan stays incomplete and nothing is purged.
func TestFsckFailsClosedOnMetadataReadFailureWithoutCompaction(t *testing.T) {
	var out bytes.Buffer
	var a *fakeCompactingVolumeServer
	c, a, b, volumes := compactionFsck(t, []uint64{1, 2, 4}, &out, func() {
		a.damage(3)
	})

	err := c.checkVolumes(volumes, fsckChecks{applyPurging: true, cutoffFromAtNs: time.Now().UnixNano(), maxOrphanPercent: 100})
	if err == nil || !strings.Contains(err.Error(), "incomplete fsck for volume 7") {
		t.Fatalf("fsck error %v, want incomplete\n%s", err, out.String())
	}
	if len(a.deleted)+len(b.deleted) != 0 {
		t.Fatalf("purged a=%v b=%v from an incomplete scan", a.deleted, b.deleted)
	}
	if a.copies != 1 {
		t.Fatalf("index copied %d times; an unchanged revision gives no reason to copy it again", a.copies)
	}
}

// After a compaction the index is copied again, but a read that still fails
// at the fresh offset, with no further compaction, fails the run closed.
func TestFsckFailsClosedWhenReadFailsAfterRecopy(t *testing.T) {
	var out bytes.Buffer
	var a *fakeCompactingVolumeServer
	c, a, b, volumes := compactionFsck(t, []uint64{1, 2, 4}, &out, func() {
		a.compact()
		a.damage(5)
	})

	err := c.checkVolumes(volumes, fsckChecks{applyPurging: true, cutoffFromAtNs: time.Now().UnixNano(), maxOrphanPercent: 100})
	if err == nil || !strings.Contains(err.Error(), "incomplete fsck for volume 7") {
		t.Fatalf("fsck error %v, want incomplete\n%s", err, out.String())
	}
	if len(a.deleted)+len(b.deleted) != 0 {
		t.Fatalf("purged a=%v b=%v from an incomplete scan", a.deleted, b.deleted)
	}
	if a.copies != 2 {
		t.Fatalf("index copied %d times, want 2", a.copies)
	}
}

// A volume vacuumed every time its index is copied never yields a usable
// index: the re-copies are bounded and the run fails closed.
func TestFsckBoundsIndexRecopiesUnderRepeatedCompaction(t *testing.T) {
	var out bytes.Buffer
	var a *fakeCompactingVolumeServer
	c, a, b, volumes := compactionFsck(t, []uint64{1, 2, 4}, &out, func() {
		a.compactOnEveryCopy()
		a.compact()
	})

	err := c.checkVolumes(volumes, fsckChecks{applyPurging: true, cutoffFromAtNs: time.Now().UnixNano(), maxOrphanPercent: 100})
	if err == nil || !strings.Contains(err.Error(), "incomplete fsck for volume 7") {
		t.Fatalf("fsck error %v, want incomplete\n%s", err, out.String())
	}
	if len(a.deleted)+len(b.deleted) != 0 {
		t.Fatalf("purged a=%v b=%v from an incomplete scan", a.deleted, b.deleted)
	}
	if a.copies != 1+fsckIndexRecopyAttempts {
		t.Fatalf("index copied %d times, want %d", a.copies, 1+fsckIndexRecopyAttempts)
	}
}
