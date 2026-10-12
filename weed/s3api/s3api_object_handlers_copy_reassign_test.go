package s3api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/operation"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// copyAssignFiler hands out destination assignments in order, repeating the
// last, and resolves every source volume to sources.
type copyAssignFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	mu          sync.Mutex
	assignments []*filer_pb.AssignVolumeResponse
	assigned    int
	sources     []*filer_pb.Location
}

func (f *copyAssignFiler) AssignVolume(ctx context.Context, req *filer_pb.AssignVolumeRequest) (*filer_pb.AssignVolumeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := min(f.assigned, len(f.assignments)-1)
	f.assigned++
	return f.assignments[i], nil
}

func (f *copyAssignFiler) LookupVolume(ctx context.Context, req *filer_pb.LookupVolumeRequest) (*filer_pb.LookupVolumeResponse, error) {
	resp := &filer_pb.LookupVolumeResponse{LocationsMap: map[string]*filer_pb.Locations{}}
	for _, vid := range req.VolumeIds {
		resp.LocationsMap[vid] = &filer_pb.Locations{Locations: f.sources}
	}
	return resp, nil
}

func startCopyAssignFiler(t *testing.T, impl *copyAssignFiler) pb.ServerAddress {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	filer_pb.RegisterSeaweedFilerServer(srv, impl)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return pb.ServerAddress(fmt.Sprintf("127.0.0.1:1.%d", lis.Addr().(*net.TCPAddr).Port))
}

// silentServer accepts connections and never answers, the way a volume
// server with a full upload admission or a stuck disk holds a request.
func silentServer(t *testing.T) (addr string, connections func() int) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		lis.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	return lis.Addr().String(), func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(conns)
	}
}

// chunkSource serves payload, honoring a Range header.
func chunkSource(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := int64(0), int64(len(payload)-1)
		if rng := r.Header.Get("Range"); rng != "" {
			fmt.Sscanf(rng, "bytes=%d-%d", &start, &end)
		}
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusOK)
		w.Write(payload[start : end+1])
	}))
	t.Cleanup(srv.Close)
	return srv
}

// chunkDestination accepts uploads and records the paths written.
func chunkDestination(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"size":1}`)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

// An UploadPartCopy chunk assigned to a destination volume server that stalls
// must be copied to another volume instead of failing the copy. On 2026-10-11
// Harbor's push of an 11.6 GB layer failed with 500 InternalError because a
// chunk POST to one stalled volume server timed out awaiting response headers
// and the copy had no second destination.
func TestCopyChunkRangeMovesOffAStalledDestination(t *testing.T) {
	util_http.InitGlobalHttpClient()
	payload := []byte(strings.Repeat("checkpoint", 100<<10))
	src := chunkSource(t, payload)
	stalledAddr, stalledConnections := silentServer(t)
	healthy, healthyPaths := chunkDestination(t)
	healthyAddr := strings.TrimPrefix(healthy.URL, "http://")
	srcAddr := strings.TrimPrefix(src.URL, "http://")

	filer := &copyAssignFiler{
		assignments: []*filer_pb.AssignVolumeResponse{
			{FileId: "3,01637037d6", Location: &filer_pb.Location{Url: stalledAddr}},
			{FileId: "4,02637037d6", Location: &filer_pb.Location{Url: healthyAddr}},
		},
		sources: []*filer_pb.Location{{Url: srcAddr}},
	}
	s3a := &S3ApiServer{option: &S3ApiServerOption{
		Filers:         []pb.ServerAddress{startCopyAssignFiler(t, filer)},
		BucketsPath:    "/buckets",
		GrpcDialOption: grpc.WithTransportCredentials(insecure.NewCredentials()),
	}}

	original := &filer_pb.FileChunk{FileId: "7,03637037d6", Offset: 0, Size: uint64(len(payload))}
	part := &filer_pb.FileChunk{FileId: original.FileId, Offset: 0, Size: original.Size}
	start := time.Now()
	dst, err := s3a.copySingleChunkForRange(context.Background(), original, part, 0, int64(len(payload))-1, "/buckets/b/k")
	took := time.Since(start)
	require.NoError(t, err, "copy with one stalled destination took %v", took)
	require.Equal(t, "4,02637037d6", dst.FileId)
	require.Equal(t, []string{"/4,02637037d6"}, healthyPaths())
	require.GreaterOrEqual(t, stalledConnections(), 1, "the first attempt never reached the stalled server")
	// One attempt on the stalled server, abandoned once it stops making
	// progress, then the copy elsewhere.
	require.Less(t, took, operation.UploadStallTimeout+operation.GatewayUploadResponseTimeout+5*time.Second)
}
