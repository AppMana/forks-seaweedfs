package framework

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/seaweedfs/seaweedfs/test/testutil"
	"github.com/seaweedfs/seaweedfs/test/volume_server/matrix"
)

type ClusterWithFiler struct {
	*Cluster

	filerCmd      *exec.Cmd
	filerPort     int
	filerGrpcPort int
}

func StartSingleVolumeClusterWithFiler(t testing.TB, profile matrix.Profile) *ClusterWithFiler {
	t.Helper()

	baseCluster := StartSingleVolumeCluster(t, profile)
	return StartFilerForCluster(t, baseCluster, "")
}

// StartFilerForCluster starts a fresh filer process against an existing data
// plane. Explicit configuration supports external-store restore tests without
// changing the master or volume data. Stop the previous filer before replacing
// its configuration.
func StartFilerForCluster(t testing.TB, baseCluster *Cluster, config string) *ClusterWithFiler {
	t.Helper()
	return startFilerBinaryForCluster(t, baseCluster, config, baseCluster.weedBinary)
}

func startFilerBinaryForCluster(t testing.TB, baseCluster *Cluster, config, binary string) *ClusterWithFiler {
	t.Helper()
	if config != "" {
		if err := os.WriteFile(filepath.Join(baseCluster.configDir, "filer.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}

	ports, err := testutil.AllocatePorts(2)
	if err != nil {
		t.Fatalf("allocate filer ports: %v", err)
	}

	filerDataDir := filepath.Join(baseCluster.baseDir, fmt.Sprintf("filer-%d", ports[0]))
	if mkErr := os.MkdirAll(filerDataDir, 0o755); mkErr != nil {
		t.Fatalf("create filer data dir: %v", mkErr)
	}

	logName := fmt.Sprintf("filer-%d.log", ports[0])
	logFile, err := os.Create(filepath.Join(baseCluster.logsDir, logName))
	if err != nil {
		t.Fatalf("create filer log file: %v", err)
	}
	defer logFile.Close()

	filerPort := ports[0]
	filerGrpcPort := ports[1]
	args := []string{
		"-config_dir=" + baseCluster.configDir,
		"filer",
		"-master=127.0.0.1:" + strconv.Itoa(baseCluster.masterPort),
		"-ip=127.0.0.1",
		"-port=" + strconv.Itoa(filerPort),
		"-port.grpc=" + strconv.Itoa(filerGrpcPort),
		"-defaultStoreDir=" + filerDataDir,
	}

	filerCmd := exec.Command(binary, args...)
	filerCmd.Dir = baseCluster.baseDir
	filerCmd.Stdout = logFile
	filerCmd.Stderr = logFile
	if err = filerCmd.Start(); err != nil {
		t.Fatalf("start filer: %v", err)
	}

	if err = baseCluster.waitForTCP(net.JoinHostPort("127.0.0.1", strconv.Itoa(filerGrpcPort))); err != nil {
		filerLogTail := baseCluster.tailLog(logName)
		stopProcess(filerCmd)
		t.Fatalf("wait for filer grpc readiness: %v\nfiler log tail:\n%s", err, filerLogTail)
	}

	c := &ClusterWithFiler{
		Cluster:       baseCluster,
		filerCmd:      filerCmd,
		filerPort:     filerPort,
		filerGrpcPort: filerGrpcPort,
	}
	t.Cleanup(c.StopFiler)
	return c
}

// StopFiler leaves the master, volume bytes and external store untouched.
func (c *ClusterWithFiler) StopFiler() {
	stopProcess(c.filerCmd)
	c.filerCmd = nil
}

func (c *ClusterWithFiler) FilerAddress() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(c.filerPort))
}

func (c *ClusterWithFiler) FilerGRPCAddress() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(c.filerGrpcPort))
}

func (c *ClusterWithFiler) FilerServerAddress() string {
	return fmt.Sprintf("%s.%d", c.FilerAddress(), c.filerGrpcPort)
}
