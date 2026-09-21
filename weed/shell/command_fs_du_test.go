package shell

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/grpc"
)

type duTestClient struct {
	filer_pb.FilerClient
	filer_pb.SeaweedFilerClient
	err error
}

func (c *duTestClient) WithFilerClient(_ bool, fn func(filer_pb.SeaweedFilerClient) error) error {
	return fn(c)
}

func (c *duTestClient) ListEntries(_ context.Context, req *filer_pb.ListEntriesRequest, _ ...grpc.CallOption) (filer_pb.SeaweedFiler_ListEntriesClient, error) {
	if req.Directory == "/root" {
		return &duTestStream{entries: []*filer_pb.Entry{{Name: "child", IsDirectory: true}}}, nil
	}
	return &duTestStream{err: c.err}, nil
}

type duTestStream struct {
	grpc.ClientStream
	entries []*filer_pb.Entry
	err     error
}

func (s *duTestStream) Recv() (*filer_pb.ListEntriesResponse, error) {
	if len(s.entries) > 0 {
		e := s.entries[0]
		s.entries = s.entries[1:]
		return &filer_pb.ListEntriesResponse{Entry: e}, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}

func TestDuTraversePropagatesNestedFailure(t *testing.T) {
	for _, want := range []error{errors.New("incomplete directory stream"), context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			_, _, err := duTraverseDirectory(io.Discard, &duTestClient{err: want}, "/root", "")
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want nested error %v", err, want)
			}
		})
	}
}
