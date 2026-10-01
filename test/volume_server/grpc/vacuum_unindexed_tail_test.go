package volume_server_grpc_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/test/volume_server/framework"
	"github.com/seaweedfs/seaweedfs/test/volume_server/matrix"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

func TestVacuumRPCPreservesUnindexedTail(t *testing.T) {
	if testing.Short() {
		t.Skip("process integration test")
	}
	c := framework.StartSingleVolumeCluster(t, matrix.P1())
	conn, client := framework.DialVolumeServer(t, c.VolumeGRPCAddress())
	defer conn.Close()
	const vid = 140
	framework.AllocateVolume(t, client, vid, "")
	payload := bytes.Repeat([]byte("preserved"), 100)
	for _, key := range []uint64{1, 2} {
		resp := framework.UploadBytes(t, framework.NewHTTPClient(), c.VolumeAdminURL(), framework.NewFileID(vid, key, 123)+"?fsync=true", payload)
		framework.ReadAllAndClose(t, resp)
		if resp.StatusCode != 201 {
			t.Fatalf("seed status %d", resp.StatusCode)
		}
	}
	c.CrashVolumeServer()
	datPath := filepath.Join(c.VolumeDataDirs()[0], "140.dat")
	idxPath := filepath.Join(c.VolumeDataDirs()[0], "140.idx")
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	index := read(idxPath)
	if len(index) != 2*types.NeedleMapEntrySize {
		t.Fatalf("index width mismatch: %d", len(index))
	}
	if err := os.Truncate(idxPath, int64(len(index)-types.NeedleMapEntrySize)); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(datPath, int64(len(read(datPath))-8)); err != nil {
		t.Fatal(err)
	}
	wantDat, wantIdx := read(datPath), read(idxPath)
	c.RestartVolumeServer()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.VacuumVolumeCompact(ctx, &volume_server_pb.VacuumVolumeCompactRequest{VolumeId: vid})
	if err != nil {
		t.Fatal(err)
	}
	for err == nil {
		_, err = stream.Recv()
	}
	if err == io.EOF || !strings.Contains(err.Error(), "refusing vacuum") {
		t.Fatalf("expected explicit unsafe-source refusal, got %v", err)
	}
	if _, err := client.VacuumVolumeCommit(ctx, &volume_server_pb.VacuumVolumeCommitRequest{VolumeId: vid}); err == nil {
		t.Fatal("commit unexpectedly succeeded after rejected copy")
	}
	c.RestartVolumeServer()
	if !bytes.Equal(read(datPath), wantDat) || !bytes.Equal(read(idxPath), wantIdx) {
		t.Fatal("vacuum or restart changed the original files")
	}
	resp := framework.ReadBytes(t, framework.NewHTTPClient(), c.VolumeAdminURL(), framework.NewFileID(vid, 1, 123))
	body := framework.ReadAllAndClose(t, resp)
	if resp.StatusCode != 200 || !bytes.Equal(body, payload) {
		t.Fatalf("intact indexed payload not preserved: status %d", resp.StatusCode)
	}
}
