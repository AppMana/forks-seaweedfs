package weed_server

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/seaweedfs/seaweedfs/weed/security"
	"github.com/seaweedfs/seaweedfs/weed/stats"
	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

const (
	transferVolumeId   = needle.VolumeId(71)
	transferCookie     = types.Cookie(0x5eed)
	transferNeedleSize = 4 << 20 // the 4 MiB chunk needles Harbor's S3 gateway reads and writes

	// transferOverheadPerRequest bounds what one request may hold beyond its
	// admitted bytes: net/http's per-connection bufio reader and writer, the
	// multipart reader's buffer, the pooled 128 KiB response writer of
	// ProcessRangeRequest, request and needle structs. 256 KiB is more than
	// all of them together.
	transferOverheadPerRequest = 256 << 10
)

type transferTestServer struct {
	vs   *VolumeServer
	addr string
}

// newTransferTestServer serves volume transferVolumeId over a real TCP
// listener wrapped like production's (util.NewListener, 30 s idle timeout),
// with the deployed read flags (-readBufferSizeMB=1, hasSlowRead).
func newTransferTestServer(t *testing.T, needles int, uploadLimit, downloadLimit int64) *transferTestServer {
	t.Helper()
	store := newVolumeCopyTestStore(t, t.TempDir())
	if err := store.AddVolume(transferVolumeId, "", storage.NeedleMapInMemory, "000", "", 0,
		needle.GetCurrentVersion(), 0, types.HardDriveType, 0); err != nil {
		t.Fatalf("add volume: %v", err)
	}
	for i := 1; i <= needles; i++ {
		n := new(needle.Needle)
		n.Id = types.Uint64ToNeedleId(uint64(i))
		n.Cookie = transferCookie
		n.Data = randomBytes(uint64(i), transferNeedleSize)
		n.Checksum = needle.NewCRC(n.Data)
		if _, err := store.WriteVolumeNeedle(transferVolumeId, n, true, false); err != nil {
			t.Fatalf("write needle %d: %v", i, err)
		}
	}

	vs := &VolumeServer{
		store:                       store,
		guard:                       security.NewGuard(nil, "", 0, "", 0),
		concurrentUploadLimit:       uploadLimit,
		concurrentDownloadLimit:     downloadLimit,
		inflightUploadDataTimeout:   60 * time.Second,
		inflightDownloadDataTimeout: 60 * time.Second,
		hasSlowRead:                 true,
		readBufferSizeMB:            1,
		ReadMode:                    "proxy",
		fileSizeLimitBytes:          256 << 20,
	}
	listener, err := util.NewListener("127.0.0.1:0", 30*time.Second)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(vs.privateStoreHandler)}
	go server.Serve(listener)
	t.Cleanup(func() {
		// The in-flight gauges are process wide: let this server's handlers
		// release every reservation before the next test reads them.
		server.Close()
		waitUntil(t, "the test server's reservations to be released", func() bool {
			return atomic.LoadInt64(&vs.inFlightUploadDataSize) == 0 &&
				atomic.LoadInt64(&vs.inFlightDownloadDataSize) == 0
		})
	})
	return &transferTestServer{vs: vs, addr: listener.Addr().String()}
}

func randomBytes(seed uint64, size int) []byte {
	rng := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	data := make([]byte, size)
	for i := 0; i < size; i += 8 {
		v := rng.Uint64()
		for k := 0; k < 8 && i+k < size; k++ {
			data[i+k] = byte(v >> (8 * k))
		}
	}
	return data
}

func transferFileId(i int) string {
	return needle.NewFileId(transferVolumeId, uint64(i), uint32(transferCookie)).String()
}

// dialSmallWindow opens a connection with a 4 KiB receive buffer, so a client
// that does not read stops the server's writes quickly.
func (s *transferTestServer) dialSmallWindow(t *testing.T) net.Conn {
	t.Helper()
	dialer := net.Dialer{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
		}); err != nil {
			return err
		}
		return serr
	}}
	conn, err := dialer.Dial("tcp", s.addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// liveHeap returns the bytes reachable after a full collection.
func liveHeap() int64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitHeapSettled returns the live heap once it stops changing, i.e. once the
// server has read everything the stalled clients sent.
func waitHeapSettled(t *testing.T) int64 {
	t.Helper()
	previous := liveHeap()
	for i := 0; i < 200; i++ {
		time.Sleep(50 * time.Millisecond)
		current := liveHeap()
		if current-previous < 64<<10 && previous-current < 64<<10 {
			return current
		}
		previous = current
	}
	t.Fatal("live heap did not settle")
	return 0
}

const transferBoundary = "seaweedfs-transfer-memory"

// stalledUpload sends a multipart POST of a 4 MiB part, the shape of the
// filer's chunk writes and of ?type=replicate forwards, but never the closing
// boundary, so the request sits in ParseUpload with the whole part read. It
// returns the request's Content-Length, the bytes upload admission reserves.
func (s *transferTestServer) stalledUpload(t *testing.T, i int, data []byte) int64 {
	t.Helper()
	head := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"chunk\"\r\n"+
		"Content-Type: application/octet-stream\r\n\r\n", transferBoundary)
	tail := fmt.Sprintf("\r\n--%s--\r\n", transferBoundary)
	contentLength := int64(len(head) + len(data) + len(tail))
	conn := s.dialSmallWindow(t)
	fmt.Fprintf(conn, "POST /%s HTTP/1.1\r\nHost: %s\r\nContent-Type: multipart/form-data; boundary=%s\r\n"+
		"Content-Length: %d\r\n\r\n", transferFileId(i), s.addr, transferBoundary, contentLength)
	go func() {
		// A client held back by admission is not read from until admitted.
		conn.Write([]byte(head))
		conn.Write(data)
	}()
	return contentLength
}

// stalledRangeGet asks for the second 2 MiB half of a 4 MiB needle, the
// request Harbor's S3 gateways issued, and never reads the response.
func (s *transferTestServer) stalledRangeGet(t *testing.T, i int) net.Conn {
	t.Helper()
	conn := s.dialSmallWindow(t)
	fmt.Fprintf(conn, "GET /%s HTTP/1.1\r\nHost: %s\r\nRange: bytes=%d-%d\r\n\r\n",
		transferFileId(i), s.addr, transferNeedleSize/2, transferNeedleSize-1)
	return conn
}

func totalAlloc() int64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.TotalAlloc)
}

// Stalled uploads must stay within their admission reservation plus
// a fixed per-request overhead.
func TestStalledUploadsHoldOnlyTheirAdmittedBytes(t *testing.T) {
	const clients = 24
	s := newTransferTestServer(t, 0, 1<<30, 0)
	data := randomBytes(99, transferNeedleSize)

	base := liveHeap()
	allocBase := totalAlloc()
	var contentLength int64
	for i := 1; i <= clients; i++ {
		contentLength = s.stalledUpload(t, i, data)
	}
	waitUntil(t, "all uploads admitted", func() bool {
		return atomic.LoadInt64(&s.vs.inFlightUploadDataSize) == clients*contentLength
	})
	held := waitHeapSettled(t) - base
	allocated := totalAlloc() - allocBase
	admitted := atomic.LoadInt64(&s.vs.inFlightUploadDataSize)
	budget := admitted + clients*transferOverheadPerRequest
	t.Logf("%d stalled 4 MiB uploads: admitted %d MiB, live heap +%d MiB (%.2f bytes per admitted byte), allocated %d MiB (%.2f per admitted byte)",
		clients, admitted>>20, held>>20, float64(held)/float64(admitted), allocated>>20, float64(allocated)/float64(admitted))
	if held > budget {
		t.Fatalf("stalled uploads hold %d bytes of heap, more than the %d admitted plus %d overhead",
			held, admitted, budget-admitted)
	}
}

// Stalled range reads must stay within their download reservation.
// Reservations and gauges must return to zero after clients disconnect.
func TestStalledRangeReadsHoldOnlyTheirAdmittedBytes(t *testing.T) {
	const (
		clients = 64
		needles = 8
	)
	s := newTransferTestServer(t, needles, 0, 1<<30)

	baseGoroutines := runtime.NumGoroutine()
	base := liveHeap()
	conns := make([]net.Conn, 0, clients)
	for i := 0; i < clients; i++ {
		conns = append(conns, s.stalledRangeGet(t, 1+i%needles))
	}
	held := waitHeapSettled(t) - base
	admitted := atomic.LoadInt64(&s.vs.inFlightDownloadDataSize)
	t.Logf("%d stalled 2 MiB range reads: admitted %d MiB, live heap +%d MiB (%.2f bytes per admitted byte)",
		clients, admitted>>20, held>>20, float64(held)/float64(admitted))
	if budget := admitted + clients*transferOverheadPerRequest; held > budget {
		t.Fatalf("stalled range reads hold %d bytes of heap, more than the %d admitted plus %d overhead",
			held, admitted, budget-admitted)
	}

	for _, conn := range conns {
		conn.Close()
	}
	waitUntil(t, "handlers of departed clients to return", func() bool {
		return atomic.LoadInt64(&s.vs.inFlightDownloadDataSize) == 0 &&
			runtime.NumGoroutine() <= baseGoroutines
	})
	waitUntil(t, "in_flight_download_size gauge to return to zero", func() bool {
		return testutil.ToFloat64(stats.VolumeServerInFlightDownloadSize) == 0
	})
}

// Concurrent stalled uploads and reads must stay within their combined
// admission budgets plus a fixed per-connection overhead.
func TestTransfersStayWithinAdmissionBudgets(t *testing.T) {
	const (
		needles       = 8
		uploads       = 24
		reads         = 16
		uploadSlots   = 16
		downloadSlots = 8
	)
	data := randomBytes(99, transferNeedleSize)
	// The multipart framing stalledUpload adds around the part.
	uploadContentLength := int64(len(fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"chunk\"\r\n"+
		"Content-Type: application/octet-stream\r\n\r\n", transferBoundary)) + len(data) + len(fmt.Sprintf("\r\n--%s--\r\n", transferBoundary)))
	uploadLimit := uploadSlots * uploadContentLength
	downloadLimit := int64(downloadSlots * transferNeedleSize)
	s := newTransferTestServer(t, needles, uploadLimit, downloadLimit)

	base := liveHeap()
	for i := 1; i <= uploads; i++ {
		s.stalledUpload(t, 100+i, data)
	}
	for i := 0; i < reads; i++ {
		s.stalledRangeGet(t, 1+i%needles)
	}
	waitUntil(t, "both budgets full", func() bool {
		return atomic.LoadInt64(&s.vs.inFlightUploadDataSize) == uploadLimit &&
			atomic.LoadInt64(&s.vs.inFlightDownloadDataSize) > downloadLimit-transferNeedleSize-(64<<10)
	})
	held := waitHeapSettled(t) - base
	budget := uploadLimit + downloadLimit + (uploads+reads)*transferOverheadPerRequest
	t.Logf("budgets %d MiB upload + %d MiB download, admitted %d + %d MiB, live heap +%d MiB",
		uploadLimit>>20, downloadLimit>>20, atomic.LoadInt64(&s.vs.inFlightUploadDataSize)>>20,
		atomic.LoadInt64(&s.vs.inFlightDownloadDataSize)>>20, held>>20)
	if held > budget {
		t.Fatalf("stalled transfers hold %d bytes of heap, more than the %d upload and %d download budgets plus %d overhead",
			held, uploadLimit, downloadLimit, budget-uploadLimit-downloadLimit)
	}
}

// TestCompletedTransfersClearInFlightGauges: the in-flight size gauges must
// report what is reserved now, not the value seen when the last request was
// admitted. Production showed in_flight_download_size at 24.5 MB with no GET
// in flight.
func TestCompletedTransfersClearInFlightGauges(t *testing.T) {
	s := newTransferTestServer(t, 1, 1<<30, 1<<30)

	resp, err := http.Get(fmt.Sprintf("http://%s/%s", s.addr, transferFileId(1)))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := drainResponse(resp); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get status %d err %v", resp.StatusCode, err)
	}
	// The client has the whole body before the handler's deferred release
	// runs, so wait for the handler to return.
	waitUntil(t, "the download reservation to be released", func() bool {
		return atomic.LoadInt64(&s.vs.inFlightDownloadDataSize) == 0
	})
	if got := testutil.ToFloat64(stats.VolumeServerInFlightDownloadSize); got != 0 {
		t.Fatalf("in_flight_download_size gauge reads %v after the only download finished", got)
	}

	resp, err = http.Post(fmt.Sprintf("http://%s/%s", s.addr, transferFileId(2)),
		"application/octet-stream", bytes.NewReader(randomBytes(7, 1<<20)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if _, err := drainResponse(resp); err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("post status %d err %v", resp.StatusCode, err)
	}
	waitUntil(t, "the upload reservation to be released", func() bool {
		return atomic.LoadInt64(&s.vs.inFlightUploadDataSize) == 0
	})
	if got := testutil.ToFloat64(stats.VolumeServerInFlightUploadSize); got != 0 {
		t.Fatalf("in_flight_upload_size gauge reads %v after the only upload finished", got)
	}
}

func drainResponse(resp *http.Response) (int64, error) {
	defer resp.Body.Close()
	return io.Copy(io.Discard, resp.Body)
}
