package fuse_test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// throttledProxy forwards TCP connections to target and limits the bytes sent
// towards target, across all connections, to bytesPerSecond: an uplink at a
// fixed speed, the way chunk uploads see a 1 GbE node.
type throttledProxy struct {
	listener       net.Listener
	target         string
	bytesPerSecond int64

	mu   sync.Mutex
	next time.Time
}

func startThrottledProxy(t *testing.T, target string, bytesPerSecond int64) *throttledProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &throttledProxy{listener: listener, target: target, bytesPerSecond: bytesPerSecond}
	t.Cleanup(func() { listener.Close() })
	go p.serve()
	return p
}

func (p *throttledProxy) port() int { return p.listener.Addr().(*net.TCPAddr).Port }

func (p *throttledProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer client.Close()
			server, err := net.Dial("tcp", p.target)
			if err != nil {
				return
			}
			defer server.Close()
			go func() {
				io.Copy(client, server)
				client.Close()
			}()
			buf := make([]byte, 64<<10)
			for {
				n, err := client.Read(buf)
				if n > 0 {
					p.wait(int64(n))
					if _, werr := server.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
}

// wait reserves n bytes of the shared rate and sleeps until they may be sent.
func (p *throttledProxy) wait(n int64) {
	p.mu.Lock()
	now := time.Now()
	if p.next.Before(now) {
		p.next = now
	}
	at := p.next
	p.next = p.next.Add(time.Duration(n * int64(time.Second) / p.bytesPerSecond))
	p.mu.Unlock()
	time.Sleep(time.Until(at))
}

// A close() after a large sequential write must find no more dirty data than
// the write buffer cap, so it drains within the cap's upload time however
// large the file is; writers wait for upload progress instead. Without a cap
// the backlog is the concurrent uploads plus every writable chunk, and a
// checkpoint on a slow link outlasts the FUSE request deadline and returns
// EIO.
func TestLargeSequentialWriteClosesWithinWriteBufferDrainTime(t *testing.T) {
	const (
		rate     = 16 << 20 // bytes per second through the proxy
		capMB    = 32
		fileSize = 256 << 20
		// The cap's upload time, plus the metadata commit and the last
		// chunks' replication to the volume server.
		closeBound = capMB*(1<<20)/rate*time.Second + 4*time.Second
	)

	config := DefaultTestConfig()
	config.ChunkSizeMB = 2
	framework := NewFuseTestFramework(t, config)
	defer framework.Cleanup()
	proxy := startThrottledProxy(t, framework.GetFilerAddr(), rate)
	config.MountOptions = []string{
		// Uploads go through the filer, so through the proxy.
		fmt.Sprintf("-filer=127.0.0.1:%d.%d", proxy.port(), framework.filerPort+10000),
		"-volumeServerAccess=filerProxy",
		fmt.Sprintf("-writeBufferSizeMB=%d", capMB),
	}
	require.NoError(t, framework.Setup(config))

	payload := make([]byte, fileSize)
	rand.Read(payload)
	path := filepath.Join(framework.GetMountPoint(), "checkpoint.pt")
	f, err := os.Create(path)
	require.NoError(t, err)
	writeStart := time.Now()
	// torch.save writes a zip stream sequentially in buffered pieces.
	for off := 0; off < fileSize; off += 1 << 20 {
		_, err := f.Write(payload[off : off+1<<20])
		require.NoError(t, err)
	}
	closeStart := time.Now()
	require.NoError(t, f.Close())
	closeTook := time.Since(closeStart)
	t.Logf("wrote %d MiB in %v, close() took %v (bound %v)", fileSize>>20, closeStart.Sub(writeStart).Round(time.Millisecond), closeTook.Round(time.Millisecond), closeBound)
	require.Less(t, closeTook, closeBound, "close() drained more than the write buffer cap")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, bytes.Equal(payload, got), "content mismatch after close")
}
