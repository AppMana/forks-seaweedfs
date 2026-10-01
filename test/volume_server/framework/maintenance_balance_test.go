package framework

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/test/volume_server/matrix"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestMaintenanceCLIBalancePreservesLiveData(t *testing.T) {
	if testing.Short() {
		t.Skip("isolated two-server integration")
	}
	c := StartMultiVolumeCluster(t, matrix.P1(), 2)
	conn, source := DialVolumeServer(t, c.VolumeGRPCAddress(0))
	defer conn.Close()
	data := map[uint32][]byte{}
	for vid := uint32(151); vid <= 154; vid++ {
		AllocateVolume(t, source, vid, "balance-qualification")
		// Balancing uses byte density, not only slot count. Four 20 MiB
		// incompressible volumes exceed two 32 MiB volume equivalents and
		// therefore require a real move to the otherwise empty server.
		data[vid] = make([]byte, 20<<20)
		_, _ = rand.New(rand.NewSource(int64(vid))).Read(data[vid])
		resp := UploadBytes(t, NewHTTPClient(), c.VolumeAdminURL(0), NewFileID(vid, 1, 12345)+"?fsync=true", data[vid])
		ReadAllAndClose(t, resp)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %d: %d", vid, resp.StatusCode)
		}
	}
	masterConn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", c.masterGrpcPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer masterConn.Close()
	master := master_pb.NewSeaweedClient(masterConn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		list, err := master.VolumeList(ctx, &master_pb.VolumeListRequest{})
		if err != nil {
			t.Fatal(err)
		}
		nodes, volumes := 0, 0
		for _, dc := range list.TopologyInfo.DataCenterInfos {
			for _, rack := range dc.RackInfos {
				for _, node := range rack.DataNodeInfos {
					nodes++
					for _, disk := range node.DiskInfos {
						volumes += len(disk.VolumeInfos)
					}
				}
			}
		}
		if nodes == 2 && volumes == 4 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("topology not ready")
		case <-time.After(100 * time.Millisecond):
		}
	}
	shell := func(apply bool) {
		t.Helper()
		command := "lock; volume.balance -collection=balance-qualification -volumesPerExec=1"
		if apply {
			command += " -apply"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, c.weedBinary, "-config_dir="+c.configDir, "shell", "-master="+c.MasterAddress())
		cmd.Dir = c.baseDir
		cmd.Stdin = strings.NewReader(command + "; unlock\n")
		out, err := cmd.CombinedOutput()
		t.Logf("balance apply=%v: %s", apply, out)
		if err != nil {
			t.Fatalf("balance: %v", err)
		}
	}
	verify := func(wantMoved int) {
		t.Helper()
		moved := 0
		for vid, payload := range data {
			copies := 0
			for server := 0; server < 2; server++ {
				// A remote HTTP read can redirect/proxy to the owner. Require
				// local storage before counting it as a moved replica.
				_, err := os.Stat(filepath.Join(c.VolumeDiskDir(server, 0), fmt.Sprintf("balance-qualification_%d.dat", vid)))
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				resp := ReadBytes(t, NewHTTPClient(), c.VolumeAdminURL(server), NewFileID(vid, 1, 12345))
				body := ReadAllAndClose(t, resp)
				if resp.StatusCode == http.StatusNotFound {
					continue
				}
				if resp.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
					t.Fatalf("volume %d on server%d corrupted: HTTP%d", vid, server, resp.StatusCode)
				}
				copies++
				if server == 1 {
					moved++
				}
			}
			if copies != 1 {
				t.Fatalf("volume %d has %d readable copies, want 1", vid, copies)
			}
		}
		if moved != wantMoved {
			t.Fatalf("moved %d live volumes, want %d", moved, wantMoved)
		}
	}
	shell(false)
	verify(0)
	shell(true)
	verify(1)
}
