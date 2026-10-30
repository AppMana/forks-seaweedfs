package fuse_test

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startVolumeServer starts a "weed volume" that registers with the mini
// master and returns it with its data node id.
func (f *FuseTestFramework) startVolumeServer(t *testing.T, name string, masterPort int) (*managedProcess, string) {
	t.Helper()
	port := freePort(t)
	dir := filepath.Join(f.tempDir, name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	proc, err := f.startProcess(name, []string{
		"volume",
		"-dir=" + dir,
		"-max=16",
		"-mserver=127.0.0.1:" + strconv.Itoa(masterPort),
		"-ip=127.0.0.1",
		"-ip.bind=127.0.0.1",
		"-port=" + strconv.Itoa(port),
		"-port.grpc=" + strconv.Itoa(port+grpcPortOffset),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		proc.cmd.Process.Signal(syscall.SIGCONT)
		proc.stop()
	})
	return proc, "127.0.0.1:" + strconv.Itoa(port)
}

// growVolumesOn creates count writable volumes on one data node, retrying
// until the node has registered with the master.
func growVolumesOn(t *testing.T, masterPort int, dataNode string, count int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/vol/grow?replication=000&count=%d&dataNode=%s", masterPort, count, dataNode)
	var last string
	require.Eventually(t, func() bool {
		resp, err := http.Get(url)
		if err != nil {
			last = err.Error()
			return false
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var grown struct {
			Count int `json:"count"`
		}
		last = string(body)
		return resp.StatusCode == http.StatusOK && json.Unmarshal(body, &grown) == nil && grown.Count == count
	}, 30*time.Second, 200*time.Millisecond, "grow %d volumes on %s: %s", count, dataNode, last)
}

// A checkpoint written while one of the volume servers it is assigned to
// stops answering must still close() successfully, inside the FUSE request
// deadline: every chunk upload that stalls on that server is abandoned and
// placed on a volume the other servers hold. On 2026-10-11 a trainer's
// torch.save failed in close() with EIO twice because chunk uploads to one
// stalled volume server were retried against the same server and fid until
// the 60 s FLUSH deadline passed.
func TestCloseSucceedsWhenAnAssignedVolumeServerStalls(t *testing.T) {
	const (
		fileSize = 64 << 20
		// The production FUSE request deadline is 60 s; close() has to finish
		// well inside it, with one stalled attempt per chunk. A stalled
		// attempt is abandoned 20 s after its body is sent, sampled each
		// second (up to 22 s); its retry starts after a 1 s backoff and is
		// never abandoned while it moves. A second stalled attempt on one
		// chunk would take at least another 21 s.
		closeBound = 41 * time.Second
	)

	config := DefaultTestConfig()
	config.ChunkSizeMB = 2
	framework := NewFuseTestFramework(t, config)
	defer framework.Cleanup()
	masterPort := freePort(t)
	config.MiniOptions = []string{
		"-master.port=" + strconv.Itoa(masterPort),
		"-master.port.grpc=" + strconv.Itoa(masterPort+grpcPortOffset),
		// The single-master mini fast-start mode admits only its embedded
		// volume server. Use normal membership for this multi-server fixture.
		"-master.peers=127.0.0.1:" + strconv.Itoa(masterPort),
		"-master.volumeSizeLimitMB=64",
	}
	require.NoError(t, framework.Setup(config))

	stalling, stallingNode := framework.startVolumeServer(t, "volume-stalling", masterPort)
	_, healthyNode := framework.startVolumeServer(t, "volume-healthy", masterPort)
	// Most writable volumes, and once it stops filling the emptiest, are on
	// the server that stalls, as on a cluster where one large server has
	// the most free space: a writer that finds a volume avoiding it by
	// drawing assignments at random keeps drawing it.
	growVolumesOn(t, masterPort, stallingNode, 8)
	growVolumesOn(t, masterPort, healthyNode, 3)

	mnt := framework.GetMountPoint()
	// Writes reach every server before the stall.
	warm := make([]byte, 16<<20)
	rand.Read(warm)
	require.NoError(t, os.WriteFile(filepath.Join(mnt, "warm.bin"), warm, 0o644))

	// SIGSTOP keeps the server's sockets open: connections are accepted by the
	// kernel and never answered, and the master keeps assigning to it.
	require.NoError(t, stalling.cmd.Process.Signal(syscall.SIGSTOP))

	payload := make([]byte, fileSize)
	rand.Read(payload)
	path := filepath.Join(mnt, "checkpoint.pt")
	f, err := os.Create(path)
	require.NoError(t, err)
	start := time.Now()
	for off := 0; off < fileSize; off += 1 << 20 {
		_, err := f.Write(payload[off : off+1<<20])
		require.NoError(t, err)
	}
	closeStart := time.Now()
	closeErr := f.Close()
	closeTook := time.Since(closeStart)
	t.Logf("wrote %d MiB in %v, close() took %v: %v", fileSize>>20, closeStart.Sub(start).Round(time.Millisecond), closeTook.Round(time.Millisecond), closeErr)
	require.NoError(t, closeErr, "close() with one stalled volume server")
	require.Less(t, closeTook, closeBound)

	// Each chunk that stalled was retried once, on a volume the stalled
	// server does not hold: as many stalled attempts as successful retries.
	mountLog, err := os.ReadFile(filepath.Join(framework.logDir, "mount.log"))
	require.NoError(t, err)
	stalls := bytes.Count(mountLog, []byte("retry uploadWithRetry: err: "))
	retried := bytes.Count(mountLog, []byte("retry uploadWithRetry successfully"))
	t.Logf("%d chunk uploads stalled, %d retries landed", stalls, retried)
	require.Positive(t, stalls, "no chunk was assigned to the stalled server")
	require.Equal(t, stalls, retried, "a chunk upload stalled more than once")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Equal(payload, got), "content mismatch after close")
}
