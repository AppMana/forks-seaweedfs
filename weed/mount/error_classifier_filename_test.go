package mount

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type filenamePolicyServer struct {
	filer_pb.UnimplementedSeaweedFilerServer
}

func (filenamePolicyServer) CreateEntry(context.Context, *filer_pb.CreateEntryRequest) (*filer_pb.CreateEntryResponse, error) {
	// This is the actual filer protocol: application errors are successful RPCs
	// carrying a structured response code, not Go sentinels sent over the wire.
	return &filer_pb.CreateEntryResponse{Error: "policy rejected component", ErrorCode: filer_pb.FilerError_ENTRY_NAME_TOO_LONG}, nil
}

func TestFilenamePolicyErrnoAcrossRPC(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	filer_pb.RegisterSeaweedFilerServer(server, filenamePolicyServer{})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	t.Cleanup(func() { listener.Close() })
	conn, err := grpc.NewClient("passthrough:///filename-policy", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := &filer_pb.CreateEntryRequest{Directory: "/dir", Entry: &filer_pb.Entry{Name: "component"}}
	_, err = filer_pb.CreateEntryWithResponse(ctx, filer_pb.NewSeaweedFilerClient(conn), request)
	if !errors.Is(err, filer_pb.ErrEntryNameTooLong) {
		t.Fatalf("RPC response did not reconstruct sentinel: %v", err)
	}
	if got := grpcErrorToFuseStatus(err); got != fuse.Status(syscall.ENAMETOOLONG) {
		t.Errorf("serialized unary filename policy = %v, want ENAMETOOLONG", got)
	}

	wire, err := proto.Marshal(&filer_pb.StreamMutateEntryResponse{Response: &filer_pb.StreamMutateEntryResponse_CreateResponse{CreateResponse: &filer_pb.CreateEntryResponse{Error: "policy rejected component", ErrorCode: filer_pb.FilerError_ENTRY_NAME_TOO_LONG}}})
	if err != nil {
		t.Fatal(err)
	}
	var decoded filer_pb.StreamMutateEntryResponse
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	_, err = createEntryFromResponse(&decoded, request)
	if !errors.Is(err, filer_pb.ErrEntryNameTooLong) {
		t.Fatalf("stream response did not reconstruct sentinel: %v", err)
	}
	if got := grpcErrorToFuseStatus(err); got != fuse.Status(syscall.ENAMETOOLONG) {
		t.Errorf("serialized stream filename policy = %v, want ENAMETOOLONG", got)
	}
	if got := grpcErrorToFuseStatus(errors.New("entry name too long")); got != fuse.EIO {
		t.Fatalf("untyped text must not impersonate structured policy: %v", got)
	}
	if got := grpcErrorToFuseStatus(nil); got != fuse.OK {
		t.Fatalf("successful operation: %v", got)
	}
}
