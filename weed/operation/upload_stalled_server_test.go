package operation

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/grpc"
)

// stalledVolumeServer accepts connections and never answers, the way a volume
// server whose upload admission is full, or whose replica forward is stuck,
// holds an upload.
type stalledVolumeServer struct {
	listener net.Listener
	mu       sync.Mutex
	conns    []net.Conn
	posts    int
}

func startStalledVolumeServer(t *testing.T) *stalledVolumeServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &stalledVolumeServer{listener: listener}
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.posts++
			s.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, c := range s.conns {
			c.Close()
		}
	})
	return s
}

func (s *stalledVolumeServer) addr() string { return s.listener.Addr().String() }

func (s *stalledVolumeServer) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts
}

// healthyVolumeServer answers every upload, or, for the paths in failWith, a
// 500 with the given body.
type healthyVolumeServer struct {
	*httptest.Server
	mu       sync.Mutex
	paths    []string
	failWith map[string]string
}

func startHealthyVolumeServer(t *testing.T, failWith map[string]string) *healthyVolumeServer {
	t.Helper()
	s := &healthyVolumeServer{failWith: failWith}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		body, fail := s.failWith[r.URL.Path]
		s.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"name":"chunk","size":3}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *healthyVolumeServer) addr() string { return strings.TrimPrefix(s.URL, "http://") }

func (s *healthyVolumeServer) uploads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// scriptedAssignFilerClient hands out the given assignments in order, then
// repeats the last one.
type scriptedAssignFilerClient struct {
	filer_pb.SeaweedFilerClient
	mu          sync.Mutex
	assignments []*filer_pb.AssignVolumeResponse
	calls       int
}

func (c *scriptedAssignFilerClient) AssignVolume(ctx context.Context, in *filer_pb.AssignVolumeRequest, opts ...grpc.CallOption) (*filer_pb.AssignVolumeResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := c.calls
	if i >= len(c.assignments) {
		i = len(c.assignments) - 1
	}
	c.calls++
	return c.assignments[i], nil
}

func (c *scriptedAssignFilerClient) WithFilerClient(streamingMode bool, fn func(filer_pb.SeaweedFilerClient) error) error {
	return fn(c)
}
func (c *scriptedAssignFilerClient) AdjustedUrl(loc *filer_pb.Location) string { return loc.GetUrl() }
func (c *scriptedAssignFilerClient) GetDataCenter() string                     { return "" }

func assignment(fid, primary string, replicas ...string) *filer_pb.AssignVolumeResponse {
	resp := &filer_pb.AssignVolumeResponse{FileId: fid, Location: &filer_pb.Location{Url: primary}}
	for _, r := range replicas {
		resp.Replicas = append(resp.Replicas, &filer_pb.Location{Url: r})
	}
	return resp
}

// mountChunkUploadOption is how weed mount uploads a chunk: one attempt per
// assignment, bounded, and avoiding servers that recently stalled.
func mountChunkUploadOption(stallTimeout time.Duration) *UploadOption {
	return &UploadOption{
		Filename:        "chunk",
		MaxAttempts:     1,
		StallTimeout:    stallTimeout,
		ResponseTimeout: stallTimeout,
		StalledServers:  NewStalledServers(time.Minute),
	}
}

// A stalled upload retries with a new fid on a volume whose servers
// have not stalled.
func TestUploadWithRetryMovesOffAStalledVolumeServer(t *testing.T) {
	stalled := startStalledVolumeServer(t)
	healthy := startHealthyVolumeServer(t, nil)
	const attemptTimeout = 300 * time.Millisecond

	client := &scriptedAssignFilerClient{assignments: []*filer_pb.AssignVolumeResponse{
		assignment("1,01", stalled.addr()),
		// another fid on a volume the stalled server also holds
		assignment("2,02", healthy.addr(), stalled.addr()),
		assignment("3,03", stalled.addr()),
		assignment("4,04", healthy.addr()),
	}}
	uploader := newUploader(&http.Client{})

	start := time.Now()
	fid, result, err, _ := uploader.UploadWithRetry(client, &filer_pb.AssignVolumeRequest{Count: 1},
		mountChunkUploadOption(attemptTimeout), bytes.NewReader([]byte("abc")))
	took := time.Since(start)
	if err != nil {
		t.Fatalf("upload with one stalled server and a healthy one failed after %v: %v", took, err)
	}
	if fid != "4,04" || result == nil {
		t.Fatalf("upload landed on %q, want the volume with no stalled server, 4,04", fid)
	}
	if n := stalled.connections(); n != 1 {
		t.Fatalf("stalled server got %d upload connections, want 1: a retry went back to it", n)
	}
	if got := healthy.uploads(); len(got) != 1 || got[0] != "/4,04" {
		t.Fatalf("healthy server uploads = %v, want only /4,04", got)
	}
	// one attempt timeout and one reassignment backoff (1 s)
	if limit := attemptTimeout + 2*time.Second; took > limit {
		t.Fatalf("upload took %v, want under %v", took, limit)
	}
}

// A primary whose replica stalls answers 500 naming the replica. The retry
// must avoid every volume that replica holds, not only the primary.
func TestUploadWithRetryAvoidsTheReplicaAPrimaryNames(t *testing.T) {
	stalledReplica := "replica-stalled:8080"
	primary := startHealthyVolumeServer(t, map[string]string{
		"/1,01": fmt.Sprintf(`{"error":"failed to write to replicas for volume 1: [%s]: Post \"http://%s/1,01?type=replicate\": context deadline exceeded"}`, stalledReplica, stalledReplica),
	})

	client := &scriptedAssignFilerClient{assignments: []*filer_pb.AssignVolumeResponse{
		assignment("1,01", primary.addr(), stalledReplica),
		assignment("2,02", primary.addr(), stalledReplica),
		assignment("3,03", primary.addr(), "replica-healthy:8080"),
	}}
	uploader := newUploader(&http.Client{})

	fid, _, err, _ := uploader.UploadWithRetry(client, &filer_pb.AssignVolumeRequest{Count: 1},
		mountChunkUploadOption(time.Second), bytes.NewReader([]byte("abc")))
	if err != nil {
		t.Fatalf("upload failed: %v", err)
	}
	if fid != "3,03" {
		t.Fatalf("upload landed on %q, want 3,03, the volume without the replica that failed", fid)
	}
	if got := primary.uploads(); len(got) != 2 || got[0] != "/1,01" || got[1] != "/3,03" {
		t.Fatalf("primary uploads = %v, want [/1,01 /3,03]", got)
	}
}

// With no volume free of a stalled server, the upload still goes ahead on
// the last assignment rather than failing: the server may have recovered.
func TestUploadWithRetryUsesAStalledServerWhenNothingElseIsAssigned(t *testing.T) {
	healthy := startHealthyVolumeServer(t, nil)
	option := mountChunkUploadOption(time.Second)
	option.StalledServers.Mark(healthy.addr())

	client := &scriptedAssignFilerClient{assignments: []*filer_pb.AssignVolumeResponse{
		assignment("1,01", healthy.addr()),
	}}
	uploader := newUploader(&http.Client{})
	fid, _, err, _ := uploader.UploadWithRetry(client, &filer_pb.AssignVolumeRequest{Count: 1}, option, bytes.NewReader([]byte("abc")))
	if err != nil || fid != "1,01" {
		t.Fatalf("upload = %q, %v; want 1,01 after %d assignments", fid, err, maxAssignmentsAvoidingStalled)
	}
	if client.calls != maxAssignmentsAvoidingStalled {
		t.Fatalf("asked for %d assignments, want %d", client.calls, maxAssignmentsAvoidingStalled)
	}
}

func TestStalledServersExpire(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewStalledServers(time.Minute)
	s.now = func() time.Time { return now }
	s.Mark("a:8080")
	if server, ok := s.FirstStalled([]string{"b:8080", "a:8080"}); !ok || server != "a:8080" {
		t.Fatalf("firstStalled = %q, %v; want a:8080", server, ok)
	}
	now = now.Add(time.Minute)
	if server, ok := s.FirstStalled([]string{"a:8080"}); ok {
		t.Fatalf("%s still stalled after its ttl", server)
	}
}

// An S3 object write fans each chunk out to every holder of its volume. A
// holder that stalls must cost one bounded attempt, after which the chunk is
// written to a volume that holder is not in.
func TestUploadReaderInChunksMovesOffAStalledHolder(t *testing.T) {
	stalled := startStalledVolumeServer(t)
	healthyA := startHealthyVolumeServer(t, nil)
	healthyB := startHealthyVolumeServer(t, nil)
	var mu sync.Mutex
	assigned := 0
	assignments := []*AssignResult{
		{Fid: "1,01637037d6", Url: healthyA.addr(), Replicas: []Location{{Url: stalled.addr()}}},
		{Fid: "2,02637037d6", Url: healthyB.addr(), Replicas: []Location{{Url: stalled.addr()}}},
		{Fid: "3,03637037d6", Url: healthyA.addr(), Replicas: []Location{{Url: healthyB.addr()}}},
	}
	payload := bytes.Repeat([]byte("layer"), 200<<10) // 1000 KiB, one chunk
	start := time.Now()
	result, err := UploadReaderInChunks(context.Background(), bytes.NewReader(payload), &ChunkedUploadOption{
		ChunkSize:      4 << 20,
		SmallFileLimit: 0,
		StalledServers: NewStalledServers(time.Minute),
		AssignFunc: func(ctx context.Context, count int, expectedDataSize uint64) (*VolumeAssignRequest, *AssignResult, error) {
			mu.Lock()
			defer mu.Unlock()
			a := assignments[min(assigned, len(assignments)-1)]
			assigned++
			return nil, a, nil
		},
	})
	took := time.Since(start)
	if err != nil {
		t.Fatalf("object write with a stalled holder failed after %v: %v", took, err)
	}
	if len(result.FileChunks) != 1 || result.FileChunks[0].FileId != "3,03637037d6" {
		t.Fatalf("chunks = %v, want one chunk on 3,03637037d6", result.FileChunks)
	}
	// One attempt abandoned once it stops making progress, one
	// reassignment; the second assignment, which also holds the stalled
	// server, is skipped.
	if limit := UploadStallTimeout + GatewayUploadResponseTimeout + 2*time.Second; took > limit {
		t.Fatalf("object write took %v, want under %v", took, limit)
	}
	if assigned != 3 {
		t.Fatalf("asked for %d assignments, want 3", assigned)
	}
}

// avoidingAssignFilerClient assigns the way a master does once a stalled
// server's volumes are the emptiest: always on stalled, unless the request
// asks to avoid it.
type avoidingAssignFilerClient struct {
	filer_pb.SeaweedFilerClient
	stalled, healthy string
	mu               sync.Mutex
	requests         []*filer_pb.AssignVolumeRequest
}

func (c *avoidingAssignFilerClient) AssignVolume(ctx context.Context, in *filer_pb.AssignVolumeRequest, opts ...grpc.CallOption) (*filer_pb.AssignVolumeResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, &filer_pb.AssignVolumeRequest{AvoidServers: append([]string(nil), in.AvoidServers...)})
	n := len(c.requests)
	for _, avoided := range in.AvoidServers {
		if avoided == c.stalled {
			return assignment(fmt.Sprintf("2,%02x", n), c.healthy), nil
		}
	}
	return assignment(fmt.Sprintf("1,%02x", n), c.stalled), nil
}

func (c *avoidingAssignFilerClient) WithFilerClient(streamingMode bool, fn func(filer_pb.SeaweedFilerClient) error) error {
	return fn(c)
}
func (c *avoidingAssignFilerClient) AdjustedUrl(loc *filer_pb.Location) string { return loc.GetUrl() }
func (c *avoidingAssignFilerClient) GetDataCenter() string                     { return "" }

// Once an attempt stalls, every later assignment must ask the master to
// avoid that server. Drawing assignments until one misses it is not enough:
// a master weighting by free space hands out the stalled server's volumes,
// which stopped filling, most of the time, and a retry that went back to it
// stalled a second time; a mount close() that met that twice ran past the
// 60 s FUSE request deadline.
func TestUploadWithRetryAsksTheMasterToAvoidAStalledServer(t *testing.T) {
	stalled := startStalledVolumeServer(t)
	healthy := startHealthyVolumeServer(t, nil)
	const attemptTimeout = 300 * time.Millisecond
	client := &avoidingAssignFilerClient{stalled: stalled.addr(), healthy: healthy.addr()}
	uploader := newUploader(&http.Client{})

	start := time.Now()
	fid, _, err, _ := uploader.UploadWithRetry(client, &filer_pb.AssignVolumeRequest{Count: 1},
		mountChunkUploadOption(attemptTimeout), bytes.NewReader([]byte("abc")))
	took := time.Since(start)
	if err != nil {
		t.Fatalf("upload failed after %v: %v", took, err)
	}
	if !strings.HasPrefix(fid, "2,") {
		t.Fatalf("upload landed on %q, want a volume of the healthy server", fid)
	}
	if n := stalled.connections(); n != 1 {
		t.Fatalf("stalled server got %d upload connections, want 1", n)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.requests) != 2 || len(client.requests[0].AvoidServers) != 0 ||
		len(client.requests[1].AvoidServers) != 1 || client.requests[1].AvoidServers[0] != stalled.addr() {
		t.Fatalf("assignment requests avoided %v, want nothing and then [%s]", client.requests, stalled.addr())
	}
	if limit := attemptTimeout + 2*time.Second; took > limit {
		t.Fatalf("upload took %v, want under %v", took, limit)
	}
}
