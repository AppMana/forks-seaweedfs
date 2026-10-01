package framework

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/test/volume_server/matrix"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestMaintenanceCLIPreservesLiveVolume(t *testing.T) {
	if testing.Short() {
		t.Skip("isolated process integration")
	}
	c := StartSingleVolumeCluster(t, matrix.P1())
	conn, volume := DialVolumeServer(t, c.VolumeGRPCAddress())
	defer conn.Close()
	AllocateVolume(t, volume, 141, "qualification")
	AllocateVolume(t, volume, 142, "qualification")
	fid := NewFileID(141, 1, 12345)
	payload := bytes.Repeat([]byte("retained-original-data"), 4096)
	resp := UploadBytes(t, NewHTTPClient(), c.VolumeAdminURL(), fid+"?fsync=true", payload)
	ReadAllAndClose(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	readLive := func() {
		t.Helper()
		resp := ReadBytes(t, NewHTTPClient(), c.VolumeAdminURL(), fid)
		got := ReadAllAndClose(t, resp)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(got, payload) {
			t.Fatal("live payload lost or altered")
		}
	}
	masterConn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", c.masterGrpcPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer masterConn.Close()
	master := master_pb.NewSeaweedClient(masterConn)
	// Wait for actual master inventory, not a fixed heartbeat sleep.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		list, err := master.VolumeList(ctx, &master_pb.VolumeListRequest{})
		if err != nil {
			t.Fatal(err)
		}
		found := map[uint32]bool{}
		for _, dc := range list.TopologyInfo.DataCenterInfos {
			for _, rack := range dc.RackInfos {
				for _, node := range rack.DataNodeInfos {
					for _, disk := range node.DiskInfos {
						for _, v := range disk.VolumeInfos {
							if v.ModifiedAtSecond > 0 && v.ModifiedAtSecond < time.Now().Unix() {
								found[v.Id] = true
							}
						}
					}
				}
			}
		}
		if found[141] && found[142] {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("volumes not registered")
		case <-time.After(100 * time.Millisecond):
		}
	}
	shell := func(command string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, c.weedBinary, "-config_dir="+c.configDir, "shell", "-master="+c.MasterAddress())
		cmd.Dir = c.baseDir
		cmd.Stdin = strings.NewReader("lock; " + command + "; unlock\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", command, err, out)
		}
	}
	emptyDat := filepath.Join(c.volumeDataDirs[0], "qualification_142.dat")
	shell("volume.deleteEmpty -collectionPattern=qualification -quietFor=0s")
	if _, err := os.Stat(emptyDat); err != nil {
		t.Fatalf("simulation deleted empty volume: %v", err)
	}
	readLive()
	shell("volume.deleteEmpty -collectionPattern=qualification -quietFor=0s -apply")
	if _, err := os.Stat(emptyDat); !os.IsNotExist(err) {
		t.Fatalf("empty volume was not deleted: %v", err)
	}
	readLive()
	revision := func() uint32 {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		status, err := volume.ReadVolumeFileStatus(ctx, &volume_server_pb.ReadVolumeFileStatusRequest{VolumeId: 141})
		if err != nil {
			t.Fatal(err)
		}
		return status.CompactionRevision
	}
	beforeVacuum := revision()
	shell("volume.vacuum -collection=qualification -volumeId=141 -garbageThreshold=0")
	if after := revision(); after <= beforeVacuum {
		t.Fatalf("vacuum did not commit: revision %d -> %d", beforeVacuum, after)
	}
	readLive()
	c.RestartVolumeServer()
	readLive()
}
