package volume_server_grpc_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/test/volume_server/framework"
	"github.com/seaweedfs/seaweedfs/test/volume_server/matrix"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
)

// Explicit binaries prevent a same-version restart from being reported as an
// upgrade test. Both must use the same production offset format. This is one
// volume-server compatibility gate, not a whole-cluster rolling-upgrade proof.
func TestVolumeBinaryUpgradeVacuumRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("process integration test")
	}
	baseline := os.Getenv("WEED_VOLUME_BINARY")
	candidate := os.Getenv("WEED_CANDIDATE_BINARY")
	if baseline == "" || candidate == "" {
		t.Skip("set WEED_VOLUME_BINARY and WEED_CANDIDATE_BINARY for migration gate")
	}
	if baseline == candidate {
		t.Fatal("migration gate requires distinct baseline and candidate paths")
	}
	// Exercise both sides of Append's 64 KiB single/split-write boundary.
	// Incompressible bytes keep transport/storage compression from turning a
	// large-record migration into another small-record test.
	for _, size := range []int{32 << 10, (64 << 10) - 1, 64 << 10, (64 << 10) + 1, 1 << 20} {
		t.Run(fmt.Sprintf("payload-%d", size), func(t *testing.T) {
			testVolumeBinaryUpgradeVacuumRollback(t, baseline, candidate, size)
		})
	}
}

func testVolumeBinaryUpgradeVacuumRollback(t *testing.T, baseline, candidate string, size int) {
	t.Helper()
	c := framework.StartSingleVolumeCluster(t, matrix.P1())
	conn, client := framework.DialVolumeServer(t, c.VolumeGRPCAddress())
	defer conn.Close()
	const vid = uint32(139)
	framework.AllocateVolume(t, client, vid, "")
	fid := framework.NewFileID(vid, 1234, 0xabcdef12)
	payload := make([]byte, size)
	updated := make([]byte, size)
	random := rand.New(rand.NewSource(139))
	_, _ = random.Read(payload)
	_, _ = random.Read(updated)
	write := func(value []byte) {
		t.Helper()
		resp := framework.UploadBytes(t, framework.NewHTTPClient(), c.VolumeAdminURL(), fid+"?fsync=true", value)
		framework.ReadAllAndClose(t, resp)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("write status %d", resp.StatusCode)
		}
	}
	read := func(value []byte) {
		t.Helper()
		resp := framework.ReadBytes(t, framework.NewHTTPClient(), c.VolumeAdminURL(), fid)
		data := framework.ReadAllAndClose(t, resp)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(data, value) {
			t.Fatalf("read integrity failed: status %d, length %d", resp.StatusCode, len(data))
		}
	}
	write(payload)
	c.RestartVolumeServerWithBinary(candidate)
	read(payload)
	write(updated)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.VacuumVolumeCompact(ctx, &volume_server_pb.VacuumVolumeCompactRequest{VolumeId: vid})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err = stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	// Include a write between the copy and commit phases.
	write(payload)
	if _, err := client.VacuumVolumeCommit(ctx, &volume_server_pb.VacuumVolumeCommitRequest{VolumeId: vid}); err != nil {
		t.Fatal(err)
	}
	read(payload)
	c.RestartVolumeServerWithBinary(baseline)
	read(payload)
	write(updated)
	c.CrashVolumeServer()
	c.RestartVolumeServerWithBinary(candidate)
	read(updated)
}
