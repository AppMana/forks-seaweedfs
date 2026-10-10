package fuse_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// latencyProxy forwards TCP connections to target and delivers every byte, in
// both directions, delay after it was read: a link with a fixed one-way
// latency and no bandwidth limit.
type latencyProxy struct {
	listener net.Listener
	target   string
	delay    time.Duration
}

func startLatencyProxy(t *testing.T, listenAddr, target string, delay time.Duration) {
	t.Helper()
	listener, err := net.Listen("tcp", listenAddr)
	require.NoError(t, err)
	p := &latencyProxy{listener: listener, target: target, delay: delay}
	t.Cleanup(func() { listener.Close() })
	go p.serve()
}

func (p *latencyProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			server, err := net.Dial("tcp", p.target)
			if err != nil {
				client.Close()
				return
			}
			go p.pump(client, server)
			p.pump(server, client)
		}()
	}
}

type delayedChunk struct {
	data []byte
	at   time.Time
}

func (p *latencyProxy) pump(src, dst net.Conn) {
	chunks := make(chan delayedChunk, 4096)
	go func() {
		defer dst.Close()
		for c := range chunks {
			time.Sleep(time.Until(c.at))
			if _, err := dst.Write(c.data); err != nil {
				return
			}
		}
	}()
	defer close(chunks)
	defer src.Close()
	buf := make([]byte, 64<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunks <- delayedChunk{data: append([]byte(nil), buf[:n]...), at: time.Now().Add(p.delay)}
		}
		if err != nil {
			return
		}
	}
}

// mountSecond mounts the same filer root at a second mount point, as another
// node's mount would, reaching the filer through a link with the given one-way
// latency. It returns the mount's path and a function that unmounts it, which
// must run before the framework's Cleanup stops the filer and removes the
// directory the mount sits in. The filer's HTTP and gRPC ports keep their
// offset behind the proxy, which is how the mount derives one from the other.
func (f *FuseTestFramework) mountSecond(t *testing.T, latency time.Duration) (string, func()) {
	t.Helper()
	proxyPort := freePort(t)
	startLatencyProxy(t, fmt.Sprintf("127.0.0.1:%d", proxyPort), f.filerAddr, latency)
	startLatencyProxy(t, fmt.Sprintf("127.0.0.1:%d", proxyPort+grpcPortOffset), fmt.Sprintf("127.0.0.1:%d", f.filerPort+grpcPortOffset), latency)
	mountPoint := filepath.Join(f.tempDir, "mount-b")
	require.NoError(t, os.MkdirAll(mountPoint, 0755))
	cacheDir := filepath.Join(f.tempDir, "cache-b")
	require.NoError(t, os.MkdirAll(cacheDir, 0755))
	proc, err := f.startProcess("mount-b", []string{
		"mount",
		"-filer=127.0.0.1:" + strconv.Itoa(proxyPort),
		"-dir=" + mountPoint,
		"-filer.path=/",
		"-dirAutoCreate",
		"-allowOthers=false",
		"-cacheDir=" + cacheDir,
	})
	require.NoError(t, err)
	unmount := func() {
		proc.stop()
		exec.Command("fusermount3", "-u", mountPoint).Run()
		exec.Command("fusermount", "-u", mountPoint).Run()
	}
	parentDev, err := deviceID(f.tempDir)
	require.NoError(t, err)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if dev, err := deviceID(mountPoint); err == nil && dev != parentDev {
			if _, err := os.ReadDir(mountPoint); err == nil {
				return mountPoint, unmount
			}
		}
		if exitErr := proc.exited(); exitErr != nil {
			f.dumpLog("mount-b")
			t.Fatalf("second mount exited: %v", exitErr)
		}
		if time.Now().After(deadline) {
			unmount()
			f.dumpLog("mount-b")
			t.Fatal("second mount not ready")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func listNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// A file another mount has closed is committed on the filer, so a listing or
// lookup that starts afterwards on this mount must show it, and must no longer
// show one the other mount has unlinked: close-to-open across mounts. A
// pipeline-parallel trainer depends on it, each stage writing its checkpoint
// file from its own node and rank 0 listing the directory once every stage has
// passed a barrier (2026-10-09, 2 of 4 stage files listed).
func TestCrossMountCloseToOpenVisibility(t *testing.T) {
	config := DefaultTestConfig()
	fw := NewFuseTestFramework(t, config)
	defer fw.Cleanup()
	require.NoError(t, fw.Setup(config))

	// B's metadata events arrive a link latency after the filer logs them,
	// as they do on another node, and later still through a peer filer.
	mountB, unmountB := fw.mountSecond(t, 100*time.Millisecond)
	defer unmountB()
	dirA := filepath.Join(fw.GetMountPoint(), "checkpoints")
	dirB := filepath.Join(mountB, "checkpoints")
	require.NoError(t, os.Mkdir(dirA, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(dirA, "seed"), []byte("seed"), 0644))

	// Mount B lists the directory first, so both its metadata cache and the
	// kernel's listing cache hold it.
	require.Eventually(t, func() bool {
		names, err := os.ReadDir(dirB)
		return err == nil && len(names) == 1
	}, 10*time.Second, 50*time.Millisecond)

	const rounds, filesPerRound = 40, 4
	var stale []string
	var previous []string
	for round := 0; round < rounds; round++ {
		var written []string
		for i := 0; i < filesPerRound; i++ {
			name := fmt.Sprintf("r%02d_stage%d.pt", round, i)
			require.NoError(t, os.WriteFile(filepath.Join(dirA, name), []byte(name), 0644))
			written = append(written, name)
		}
		var removed string
		if len(previous) > 0 {
			removed = previous[0]
			require.NoError(t, os.Remove(filepath.Join(dirA, removed)))
		}

		// Lookup of a name B has never seen, in a directory B has cached.
		lookupName := written[filesPerRound-1]
		if _, err := os.Stat(filepath.Join(dirB, lookupName)); err != nil {
			stale = append(stale, fmt.Sprintf("round %d: stat %s: %v", round, lookupName, err))
		}

		listed := map[string]bool{}
		for _, name := range listNames(t, dirB) {
			listed[name] = true
		}
		for _, name := range written {
			if !listed[name] {
				stale = append(stale, fmt.Sprintf("round %d: %s closed on A but not listed on B", round, name))
			}
		}
		if removed != "" && listed[removed] {
			stale = append(stale, fmt.Sprintf("round %d: %s unlinked on A but still listed on B", round, removed))
		}
		previous = written[1:]
	}
	if len(stale) > 0 {
		fw.dumpLog("mount-b")
		shown := stale
		if len(shown) > 8 {
			shown = shown[:8]
		}
		t.Fatalf("%d stale observations over %d rounds, first %d:\n%s", len(stale), rounds, len(shown), strings.Join(shown, "\n"))
	}
}
