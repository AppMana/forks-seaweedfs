package framework

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/test/testutil"
	"github.com/seaweedfs/seaweedfs/test/volume_server/matrix"
)

// TestExternalEtcdSnapshotRestore uses real binaries and only fresh loopback
// stores. It never accepts an existing endpoint, data directory or snapshot.
// This is a metadata disaster-recovery test, not a power-loss simulation.
func TestExternalEtcdSnapshotRestore(t *testing.T) {
	if os.Getenv("SEAWEEDFS_ETCD_RESTORE") != "1" {
		t.Skip("set SEAWEEDFS_ETCD_RESTORE=1 and ETCD_BINARY/ETCDCTL_BINARY")
	}
	etcd, ctl := os.Getenv("ETCD_BINARY"), os.Getenv("ETCDCTL_BINARY")
	for _, binary := range []string{etcd, ctl} {
		if !filepath.IsAbs(binary) || !isExecutableFile(binary) {
			t.Fatalf("expected explicit executable: %q", binary)
		}
	}
	c := StartSingleVolumeCluster(t, matrix.P1())
	baseline := os.Getenv("WEED_FILER_UPGRADE_BASELINE")
	if baseline != "" {
		for _, input := range []struct{ path, pin string }{
			{baseline, os.Getenv("WEED_FILER_UPGRADE_BASELINE_SHA256")},
			{c.weedBinary, os.Getenv("WEED_FILER_UPGRADE_CANDIDATE_SHA256")},
		} {
			data, err := os.ReadFile(input.path)
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != input.pin {
				t.Fatalf("binary hash mismatch: %s: %v", input.path, err)
			}
			version, err := exec.Command(input.path, "version").CombinedOutput()
			if err != nil {
				t.Fatalf("version: %v: %s", err, version)
			}
			t.Logf("pinned filer binary %s SHA256=%s version=%s", input.path, input.pin, version)
		}
		if os.Getenv("WEED_FILER_UPGRADE_BASELINE_SHA256") == os.Getenv("WEED_FILER_UPGRADE_CANDIDATE_SHA256") {
			t.Fatal("identical binaries cannot qualify a filer upgrade")
		}
	}
	run := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, ctl, args...)
		cmd.Env = []string{"ETCDCTL_API=3"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("etcdctl %v: %v\n%s", args, err, out)
		}
		return out
	}
	type store struct {
		client, peer, dir, name string
		stop                    func()
	}
	newStore := func(name string) *store {
		t.Helper()
		ports, err := testutil.AllocatePorts(2)
		if err != nil {
			t.Fatal(err)
		}
		return &store{client: fmt.Sprintf("http://127.0.0.1:%d", ports[0]), peer: fmt.Sprintf("http://127.0.0.1:%d", ports[1]), dir: filepath.Join(c.baseDir, name), name: name}
	}
	start := func(s *store) {
		t.Helper()
		log, err := os.Create(filepath.Join(c.logsDir, s.name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		cmd := exec.Command(etcd, "--name="+s.name, "--data-dir="+s.dir, "--listen-client-urls="+s.client, "--advertise-client-urls="+s.client, "--listen-peer-urls="+s.peer, "--initial-advertise-peer-urls="+s.peer, "--initial-cluster="+s.name+"="+s.peer, "--initial-cluster-token="+s.name)
		cmd.Env = []string{}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		s.stop = func() { once.Do(func() { stopProcess(cmd) }) }
		t.Cleanup(s.stop)
		if err := c.waitForHTTP(s.client + "/health"); err != nil {
			t.Fatal(err)
		}
		run("--endpoints="+s.client, "endpoint", "health")
	}
	identity := func(s *store) uint64 {
		t.Helper()
		var status []struct {
			Status struct {
				Header struct {
					ClusterID uint64 `json:"cluster_id"`
				} `json:"header"`
			} `json:"Status"`
		}
		if err := json.Unmarshal(run("--endpoints="+s.client, "endpoint", "status", "-w=json"), &status); err != nil {
			t.Fatal(err)
		}
		if len(status) != 1 || status[0].Status.Header.ClusterID == 0 {
			t.Fatalf("missing cluster identity: %+v", status)
		}
		return status[0].Status.Header.ClusterID
	}
	filerBinary := c.weedBinary
	filerFor := func(s *store) *ClusterWithFiler {
		return startFilerBinaryForCluster(t, c, fmt.Sprintf("[etcd]\nenabled = true\nservers = %q\nkey_prefix = \"restore-qualification/\"\ntimeout = \"3s\"\n", s.client), filerBinary)
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	get := func(f *ClusterWithFiler, path string, want int) []byte {
		t.Helper()
		resp, err := client.Get("http://" + f.FilerAddress() + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want {
			t.Fatalf("GET %s: %d: %s", path, resp.StatusCode, body)
		}
		return body
	}
	upload := func(f *ClusterWithFiler, name string, payload []byte) {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		resp, err := client.Post("http://"+f.FilerAddress()+"/restore/"+name, writer.FormDataContentType(), &body)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			t.Fatalf("upload: %d %s", resp.StatusCode, out)
		}
	}
	original := newStore("original")
	start(original)
	originalID := identity(original)
	if baseline != "" {
		filerBinary = baseline
	}
	f := filerFor(original)
	payload := bytes.Repeat([]byte("external-etcd-restore-payload\x00"), 65536)
	upload(f, "intact.bin", payload)
	if got := get(f, "/restore/intact.bin", 200); !bytes.Equal(got, payload) {
		t.Fatal("original payload mismatch")
	}
	metadata := get(f, "/restore/intact.bin?metadata=true", 200)
	var entry struct {
		Chunks []json.RawMessage `json:"chunks"`
	}
	if err := json.Unmarshal(metadata, &entry); err != nil || len(entry.Chunks) == 0 {
		t.Fatalf("must reference real volume chunks: %v %s", err, metadata)
	}
	if baseline != "" {
		old := f
		filerBinary = c.weedBinary
		f = filerFor(original)
		if got := get(f, "/restore/intact.bin", 200); !bytes.Equal(got, payload) {
			t.Fatal("candidate cannot read baseline payload")
		}
		if got := get(f, "/restore/intact.bin?metadata=true", 200); !bytes.Equal(got, metadata) {
			t.Fatal("candidate changed baseline chunk metadata")
		}
		// Both versions stay alive against the same fresh etcd store. Read
		// newly created names to avoid claiming cache-invalidation coverage.
		for _, pair := range [][2]*ClusterWithFiler{{old, f}, {f, old}} {
			name := fmt.Sprintf("mixed-%d.bin", pair[0].filerPort)
			upload(pair[0], name, payload)
			if got := get(pair[1], "/restore/"+name, 200); !bytes.Equal(got, payload) {
				t.Fatal("mixed-version filer payload mismatch")
			}
		}
		old.StopFiler()
		t.Log("FILER_MIXED_VERSION_COMPLETE baseline data and bidirectional new writes verified")
	}
	snapshot := filepath.Join(c.baseDir, "snapshot.db")
	run("--endpoints="+original.client, "snapshot", "save", snapshot)
	run("snapshot", "status", snapshot, "-w=json")
	// Prove restore is a point-in-time recovery, not a surviving original store.
	upload(f, "after-snapshot.bin", []byte("must not survive restore"))
	get(f, "/restore/after-snapshot.bin", 200)
	f.StopFiler()
	original.stop()
	for _, addr := range []string{f.FilerAddress(), original.client[len("http://"):]} {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			conn.Close()
			t.Fatalf("original service still reachable: %s", addr)
		}
	}
	restored := newStore("restored")
	run("snapshot", "restore", snapshot, "--name="+restored.name, "--data-dir="+restored.dir, "--initial-cluster="+restored.name+"="+restored.peer, "--initial-advertise-peer-urls="+restored.peer, "--initial-cluster-token="+restored.name)
	start(restored)
	if identity(restored) == originalID {
		t.Fatal("restore must create a new cluster identity")
	}
	r := filerFor(restored)
	if got := get(r, "/restore/intact.bin", 200); !bytes.Equal(got, payload) {
		t.Fatalf("restored payload mismatch: got %x want %x", sha256.Sum256(got), sha256.Sum256(payload))
	}
	if got := get(r, "/restore/intact.bin?metadata=true", 200); !bytes.Equal(got, metadata) {
		t.Fatalf("restored metadata/chunks changed: %s != %s", got, metadata)
	}
	get(r, "/restore/after-snapshot.bin", 404)
	upload(r, "new-cluster.bin", payload)
	if got := get(r, "/restore/new-cluster.bin", 200); !bytes.Equal(got, payload) {
		t.Fatal("restored store cannot persist new data")
	}
	t.Logf("ETCD_RESTORE_COMPLETE original_cluster=%x bytes=%d sha256=%x", originalID, len(payload), sha256.Sum256(payload))
}
