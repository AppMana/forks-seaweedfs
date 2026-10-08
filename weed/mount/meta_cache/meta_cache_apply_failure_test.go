package meta_cache

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// oneEventFiler streams a single create event to every subscriber, then holds
// the subscription open.
type oneEventFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
}

func (oneEventFiler) SubscribeMetadata(req *filer_pb.SubscribeMetadataRequest, stream filer_pb.SeaweedFiler_SubscribeMetadataServer) error {
	if err := stream.Send(&filer_pb.SubscribeMetadataResponse{
		Directory: "/dir",
		TsNs:      time.Now().UnixNano(),
		EventNotification: &filer_pb.EventNotification{
			NewEntry: &filer_pb.Entry{Name: "file", Attributes: &filer_pb.FuseAttributes{FileSize: 1}},
		},
	}); err != nil {
		return err
	}
	<-stream.Context().Done()
	return nil
}

type dialFilerClient struct{ address string }

func (c dialFilerClient) WithFilerClient(_ bool, fn func(filer_pb.SeaweedFilerClient) error) error {
	conn, err := grpc.NewClient(c.address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(filer_pb.NewSeaweedFilerClient(conn))
}
func (dialFilerClient) AdjustedUrl(location *filer_pb.Location) string { return location.Url }
func (dialFilerClient) GetDataCenter() string                          { return "" }

const applyFailureChildEnv = "META_CACHE_APPLY_FAILURE_CHILD"

// A mount's subscription used FatalOnError: one event its local metadata
// store failed to write killed the whole mount. On appmana-031 (2026-10-08) a
// full root disk made leveldb refuse a write and weed exited 255 at
// filer_pb_tail.go:97, ending every pod's view of the volume. A store that
// cannot apply an event must be handed to the mount, which can stop trusting
// its cache, and the subscription must go on.
func TestSubscribedEventStoreFailureDoesNotExitTheMount(t *testing.T) {
	if os.Getenv(applyFailureChildEnv) == "1" {
		runApplyFailureChild()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestSubscribedEventStoreFailureDoesNotExitTheMount$", "-test.v")
	cmd.Env = append(os.Environ(), applyFailureChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subscription exited the process when the store failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("APPLY_FAILURE_HANDLED")) {
		t.Fatalf("store failure never reached the mount's handler:\n%s", out)
	}
}

func runApplyFailureChild() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	server := pb.NewGrpcServer()
	filer_pb.RegisterSeaweedFilerServer(server, oneEventFiler{})
	go server.Serve(listener)

	dir, err := os.MkdirTemp("", "apply-failure")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	mapper, _ := NewUidGidMapper("", "")
	mc := NewMetaCache(filepath.Join(dir, "meta"), mapper, util.FullPath("/"), false,
		func(util.FullPath) {}, func(util.FullPath) bool { return true }, func(EntryInvalidation) {}, nil)
	// The store refuses every write from here on, as a full disk does.
	mc.leveldbStore.Shutdown()

	handled := make(chan struct{})
	mc.SetApplyFailureHandler(func(resp *filer_pb.SubscribeMetadataResponse, err error) {
		select {
		case <-handled:
		default:
			close(handled)
		}
	})
	go SubscribeMetaEvents(mc, 7, dialFilerClient{listener.Addr().String()}, nil, "/", 0, false, nil)
	select {
	case <-handled:
		// Still alive after the failure was handled.
		time.Sleep(200 * time.Millisecond)
		os.Stdout.WriteString("APPLY_FAILURE_HANDLED\n")
	case <-time.After(20 * time.Second):
		os.Stdout.WriteString("no store failure observed\n")
	}
}

