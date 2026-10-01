package framework

import (
	"context"
	"crypto/sha256"
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

func TestMaintenanceReadOnlyAndScheduler(t *testing.T) {
	if testing.Short() {
		t.Skip("isolated two-server binary qualification")
	}
	c := StartMultiVolumeCluster(t, matrix.P1(), 2)
	for server := 0; server < 2; server++ {
		conn, client := DialVolumeServer(t, c.VolumeGRPCAddress(server))
		defer conn.Close()
		AllocateVolume(t, client, 43, "readonly-fixture")
		fid := NewFileID(43, uint64(server+1), 12345)
		resp := UploadBytes(t, NewHTTPClient(), c.VolumeAdminURL(server), fid+"?fsync=true", []byte(fmt.Sprintf("intact unique data on replica %d", server)))
		ReadAllAndClose(t, resp)
		if resp.StatusCode != http.StatusCreated {
			t.Fatal("seed failed")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := client.VolumeMarkReadonly(ctx, &volume_server_pb.VolumeMarkReadonlyRequest{VolumeId: 43})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if server == 0 {
			for vid := uint32(201); vid <= 206; vid++ {
				AllocateVolume(t, client, vid, "scheduler-fixture")
			}
		}
	}
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", c.masterGrpcPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	master := master_pb.NewSeaweedClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		list, err := master.VolumeList(ctx, &master_pb.VolumeListRequest{})
		if err != nil {
			t.Fatal(err)
		}
		count, readonly := 0, 0
		for _, dc := range list.TopologyInfo.DataCenterInfos {
			for _, rack := range dc.RackInfos {
				for _, node := range rack.DataNodeInfos {
					for _, disk := range node.DiskInfos {
						for _, v := range disk.VolumeInfos {
							count++
							if v.Id == 43 && v.ReadOnly {
								readonly++
							}
						}
					}
				}
			}
		}
		if count == 8 && readonly == 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture topology not registered")
		case <-time.After(100 * time.Millisecond):
		}
	}
	hashes := func(t *testing.T) map[string][32]byte {
		t.Helper()
		result := map[string][32]byte{}
		for server := 0; server < 2; server++ {
			for _, ext := range []string{"dat", "idx"} {
				path := filepath.Join(c.VolumeDiskDir(server, 0), "readonly-fixture_43."+ext)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				result[path] = sha256.Sum256(data)
			}
		}
		return result
	}
	shell := func(t *testing.T, command string, timeout time.Duration) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, c.weedBinary, "-config_dir="+c.configDir, "shell", "-master="+c.MasterAddress())
		cmd.Dir = c.baseDir
		cmd.Stdin = strings.NewReader("lock; " + command + "; unlock\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", command, err, out)
		}
		return string(out)
	}
	t.Run("readonly-report-preserves-files", func(t *testing.T) {
		before := hashes(t)
		out := shell(t, "volume.check.disk -volumeId=43 -slow -v -fixReadOnly", 10*time.Second)
		if !strings.Contains(out, "TWO-SIDED") {
			t.Fatalf("unique live data not reported: %s", out)
		}
		for path, digest := range hashes(t) {
			if digest != before[path] {
				t.Fatalf("report mutated %s", path)
			}
		}
	})
	t.Run("six-checks-without-polling-delay", func(t *testing.T) {
		start := time.Now()
		shell(t, "volume.vacuum -collection=scheduler-fixture -garbageThreshold=1", 5*time.Second)
		t.Logf("six-volume no-garbage sweep completed in %v", time.Since(start))
	})
}
