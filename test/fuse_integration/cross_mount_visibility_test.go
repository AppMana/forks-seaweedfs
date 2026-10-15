package fuse_test

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
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
	return f.mountSecondVia(t, f.filerPort, latency)
}

// mountSecondVia is mountSecond against the filer at filerPort.
func (f *FuseTestFramework) mountSecondVia(t *testing.T, filerPort int, latency time.Duration) (string, func()) {
	t.Helper()
	proxyPort := freePort(t)
	startLatencyProxy(t, fmt.Sprintf("127.0.0.1:%d", proxyPort), fmt.Sprintf("127.0.0.1:%d", filerPort), latency)
	startLatencyProxy(t, fmt.Sprintf("127.0.0.1:%d", proxyPort+grpcPortOffset), fmt.Sprintf("127.0.0.1:%d", filerPort+grpcPortOffset), latency)
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

// subscriptionDelayProxy forwards gRPC connections to target and delays what
// the server sends on metadata subscription streams, as a relay between filers
// on different nodes trails its events; every other call, on the same
// connection or not, passes at once. Subscriptions are told apart by the
// method their HTTP/2 request names, and the server's frames by stream id.
type subscriptionDelayProxy struct {
	listener net.Listener
	target   string
	delay    time.Duration
}

func startSubscriptionDelayProxy(t *testing.T, listenAddr, target string, delay time.Duration) {
	t.Helper()
	listener, err := net.Listen("tcp", listenAddr)
	require.NoError(t, err)
	p := &subscriptionDelayProxy{listener: listener, target: target, delay: delay}
	t.Cleanup(func() { listener.Close() })
	go p.serve()
}

// subscriptionStreams is the set of HTTP/2 stream ids carrying a subscription.
type subscriptionStreams struct {
	sync.Mutex
	ids map[uint32]bool
}

func (s *subscriptionStreams) add(id uint32) {
	s.Lock()
	defer s.Unlock()
	s.ids[id] = true
}

func (s *subscriptionStreams) has(id uint32) bool {
	s.Lock()
	defer s.Unlock()
	return s.ids[id]
}

func (p *subscriptionDelayProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			client.Close()
			continue
		}
		streams := &subscriptionStreams{ids: map[uint32]bool{}}
		requests, sniffed := io.Pipe()
		go sniffSubscriptions(requests, streams)
		go func() {
			defer server.Close()
			// the sniffer reads each request before the server sees it, so a
			// stream is known as a subscription before anything comes back on it
			io.Copy(io.MultiWriter(sniffed, server), client)
			sniffed.Close()
		}()
		go p.pumpReplies(server, client, streams)
	}
}

// sniffSubscriptions reads the client's HTTP/2 frames and records each stream
// whose request names a metadata subscription.
func sniffSubscriptions(requests *io.PipeReader, streams *subscriptionStreams) {
	defer io.Copy(io.Discard, requests)
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(requests, preface); err != nil {
		return
	}
	framer := http2.NewFramer(nil, requests)
	framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			return
		}
		if headers, ok := frame.(*http2.MetaHeadersFrame); ok {
			path := headers.PseudoValue("path")
			if strings.Contains(path, "/Subscribe") && strings.HasSuffix(path, "Metadata") {
				streams.add(headers.StreamID)
			}
		}
	}
}

// pumpReplies forwards the server's frames, holding those of subscription
// streams for the delay, in order.
func (p *subscriptionDelayProxy) pumpReplies(server, client net.Conn, streams *subscriptionStreams) {
	defer client.Close()
	var writeMu sync.Mutex
	write := func(frame []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err := client.Write(frame)
		return err
	}
	type held struct {
		frame []byte
		at    time.Time
	}
	delayed := make(chan held, 4096)
	defer close(delayed)
	go func() {
		for h := range delayed {
			time.Sleep(time.Until(h.at))
			if write(h.frame) != nil {
				client.Close()
			}
		}
	}()
	for {
		header := make([]byte, 9)
		if _, err := io.ReadFull(server, header); err != nil {
			return
		}
		length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
		frame := make([]byte, 9+length)
		copy(frame, header)
		if _, err := io.ReadFull(server, frame[9:]); err != nil {
			return
		}
		streamID := binary.BigEndian.Uint32(header[5:9]) & 0x7fffffff
		if streamID != 0 && streams.has(streamID) {
			delayed <- held{frame, time.Now().Add(p.delay)}
			continue
		}
		if write(frame) != nil {
			return
		}
	}
}

// startPeerFiler starts a second filer of the cluster with its own store, as
// another node's filer: the two follow each other's metadata logs. Peers reach
// it at advertisedIp, where the caller can put a proxy; it listens on
// 127.0.0.1. It returns the filer's HTTP port.
func (f *FuseTestFramework) startPeerFiler(t *testing.T, masterPort int, advertisedIp string, port int) {
	t.Helper()
	storeDir := filepath.Join(f.tempDir, "filer-2")
	require.NoError(t, os.MkdirAll(storeDir, 0755))
	proc, err := f.startProcess("filer-2", []string{
		"filer",
		"-master=127.0.0.1:" + strconv.Itoa(masterPort),
		"-ip=" + advertisedIp,
		"-ip.bind=127.0.0.1",
		"-port=" + strconv.Itoa(port),
		"-defaultStoreDir=" + storeDir,
	})
	require.NoError(t, err)
	t.Cleanup(proc.stop)
	if err := f.waitForService(proc, fmt.Sprintf("127.0.0.1:%d", port+grpcPortOffset), 30*time.Second); err != nil {
		f.dumpLog("filer-2")
		t.Fatal(err)
	}
}

// A mount's own close says nothing about another mount's earlier close that
// reached the cluster through another filer: the checkpoint barrier of a
// pipeline-parallel trainer, rank 0 creating the staging directory and its own
// stage file through one filer while rank 1's stage file, closed first through
// another filer, is still being relayed (2026-10-15, 1 of 2 stage files
// listed). Each round, B creates its file, A writes and closes its own through
// the peer filer, then B closes its file and lists at once; B must list both.
func TestCrossMountListingAfterOwnCloseSeesPeerFilerClose(t *testing.T) {
	config := DefaultTestConfig()
	masterPort := freePort(t)
	config.MiniOptions = append(config.MiniOptions, "-master.port="+strconv.Itoa(masterPort))
	fw := NewFuseTestFramework(t, config)
	defer fw.Cleanup()
	require.NoError(t, fw.Setup(config))

	// The peer filer's events reach B's filer a relay delay late; position
	// requests between the filers are not delayed.
	const peerIp = "127.0.0.2"
	peerPort := freePort(t)
	startLatencyProxy(t, fmt.Sprintf("%s:%d", peerIp, peerPort), fmt.Sprintf("127.0.0.1:%d", peerPort), 0)
	startSubscriptionDelayProxy(t, fmt.Sprintf("%s:%d", peerIp, peerPort+grpcPortOffset), fmt.Sprintf("127.0.0.1:%d", peerPort+grpcPortOffset), 300*time.Millisecond)
	fw.startPeerFiler(t, masterPort, peerIp, peerPort)
	mountA, unmountA := fw.mountSecondVia(t, peerPort, 0)
	defer unmountA()
	// A filer can only name the positions of peers it knows; wait until B's
	// filer follows the peer, as it has long done in a running cluster.
	require.NoError(t, os.WriteFile(filepath.Join(mountA, "peer-joined"), nil, 0644))
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(fw.GetMountPoint(), "peer-joined"))
		return err == nil
	}, 30*time.Second, 50*time.Millisecond)

	const rounds = 20
	var stale []string
	for round := 0; round < rounds; round++ {
		name := fmt.Sprintf("global_step%02d.incomplete", round)
		dirB := filepath.Join(fw.GetMountPoint(), name)
		dirA := filepath.Join(mountA, name)
		// rank 0 creates the staging directory and lists it
		require.NoError(t, os.Mkdir(dirB, 0755))
		require.Empty(t, listNames(t, dirB))
		require.Eventually(t, func() bool {
			_, err := os.Stat(dirA)
			return err == nil
		}, 10*time.Second, 10*time.Millisecond)

		// rank 0 opens its stage file, rank 1 writes and closes its own, then
		// rank 0 closes and, past the barrier, lists
		own, err := os.Create(filepath.Join(dirB, "mp_rank_00_model_states.pt"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dirA, "mp_rank_01_model_states.pt"), []byte("rank 1"), 0644))
		_, err = own.Write([]byte("rank 0"))
		require.NoError(t, err)
		require.NoError(t, own.Close())
		names := listNames(t, dirB)
		if len(names) != 2 {
			stale = append(stale, fmt.Sprintf("round %d: listed %v", round, names))
		}
	}
	if len(stale) > 0 {
		fw.dumpLog("mount-b")
		t.Fatalf("%d of %d rounds missed the peer's file:\n%s", len(stale), rounds, strings.Join(stale, "\n"))
	}
}
