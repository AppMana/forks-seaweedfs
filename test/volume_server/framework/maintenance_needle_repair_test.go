package framework

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/test/volume_server/matrix"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMissingNeedleRepairRPC(t *testing.T) {
	baseline := os.Getenv("WEED_REPAIR_BASELINE")
	if testing.Short() || baseline == "" {
		t.Skip("requires real binaries and WEED_REPAIR_BASELINE for old-server refusal")
	}
	source := StartSingleVolumeCluster(t, matrix.P1())
	target := StartSingleVolumeCluster(t, matrix.P1())
	sc, sourceClient := DialVolumeServer(t, source.VolumeGRPCAddress())
	defer sc.Close()
	tc, targetClient := DialVolumeServer(t, target.VolumeGRPCAddress())
	defer tc.Close()
	AllocateVolume(t, sourceClient, 41, "repair")
	AllocateVolume(t, targetClient, 41, "repair")
	httpClient := NewHTTPClient()
	fid, other := NewFileID(41, 17, 123), NewFileID(41, 18, 456)
	want := []byte("original payload recovered only into absent target")
	intact := []byte("different intact target entry must survive")
	for _, item := range []struct {
		url, fid string
		data     []byte
	}{{source.VolumeAdminURL(), fid, want}, {target.VolumeAdminURL(), other, intact}} {
		resp := UploadBytes(t, httpClient, item.url, item.fid+"?fsync=true", item.data)
		ReadAllAndClose(t, resp)
		if resp.StatusCode != 201 {
			t.Fatalf("seed status=%d", resp.StatusCode)
		}
	}
	idx, err := os.ReadFile(filepath.Join(source.VolumeDataDirs()[0], "repair_41.idx"))
	if err != nil {
		t.Fatal(err)
	}
	if len(idx) != types.NeedleIdSize+types.OffsetSize+types.SizeSize {
		t.Fatalf("index width mismatch: run with matching binary build tags, row=%d", len(idx))
	}
	offset := types.BytesToOffset(idx[types.NeedleIdSize:]).ToActualOffset()
	size := types.BytesToSize(idx[types.NeedleIdSize+types.OffsetSize:])
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	blob, err := sourceClient.ReadNeedleBlob(ctx, &volume_server_pb.ReadNeedleBlobRequest{VolumeId: 41, Offset: offset, Size: int32(size)})
	if err != nil {
		t.Fatal(err)
	}
	req := &volume_server_pb.WriteNeedleBlobRequest{VolumeId: 41, NeedleId: 17, Size: int32(size), NeedleBlob: blob.NeedleBlob}
	read := func(fid string, want []byte, code int) {
		t.Helper()
		resp := ReadBytes(t, httpClient, target.VolumeAdminURL(), fid)
		body := ReadAllAndClose(t, resp)
		if resp.StatusCode != code || (code == 200 && !bytes.Equal(body, want)) {
			t.Fatalf("read %s: status=%d bytes=%q", fid, resp.StatusCode, body)
		}
	}
	read(fid, nil, 404)
	target.RestartVolumeServerWithBinary(baseline)
	_, err = targetClient.WriteNeedleBlobIfAbsent(ctx, req)
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("old server must refuse: %v", err)
	}
	read(fid, nil, 404)
	read(other, intact, 200)
	target.RestartVolumeServerWithBinary(target.weedBinary)
	if _, err = targetClient.WriteNeedleBlobIfAbsent(ctx, req); err != nil {
		t.Fatal(err)
	}
	read(fid, want, 200)
	read(other, intact, 200)
	if _, err = targetClient.WriteNeedleBlobIfAbsent(ctx, req); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("retry must refuse: %v", err)
	}
	target.RestartVolumeServer()
	read(fid, want, 200)
	read(other, intact, 200)
}
