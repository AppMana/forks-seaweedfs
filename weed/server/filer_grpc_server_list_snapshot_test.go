package weed_server

import (
	"context"
	"strconv"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

type captureListStream struct {
	grpc.ServerStream
	responses []*filer_pb.ListEntriesResponse
	trailer   metadata.MD
}

func (s *captureListStream) Send(resp *filer_pb.ListEntriesResponse) error {
	s.responses = append(s.responses, resp)
	return nil
}

func (s *captureListStream) SetTrailer(md metadata.MD) { s.trailer = md }

func (s *captureListStream) Context() context.Context { return context.Background() }

func (s *captureListStream) snapshotTsNs(t *testing.T) int64 {
	t.Helper()
	if len(s.responses) > 0 {
		return s.responses[0].SnapshotTsNs
	}
	values := s.trailer.Get(filer_pb.ListSnapshotTsNsTrailerKey)
	if len(values) == 0 {
		t.Fatal("listing carries no snapshot")
	}
	tsNs, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return tsNs
}

// A listing's snapshot claims every change logged at or below it is in the
// listing. A change stamped before the listing but still on its way to the
// store is not, and its event, logged at its stamp, must not fall under the
// snapshot: a mount fences events at or below it out of the listing it built.
func TestListingSnapshotLeavesOutAChangeStillOnItsWayToTheStore(t *testing.T) {
	store := newRenameTestStore()
	store.entries["/dir/seed"] = newFileEntry("/dir/seed", 1)
	f := newRenameTestFiler(t, store)
	server := &FilerServer{filer: f}

	inFlightTsNs := f.StampMetaLogInflightForTest()
	defer f.DoneMetaLogInflightForTest(inFlightTsNs)

	stream := &captureListStream{}
	if err := server.ListEntries(&filer_pb.ListEntriesRequest{Directory: "/dir", Limit: 100}, stream); err != nil {
		t.Fatal(err)
	}
	if snapshot := stream.snapshotTsNs(t); snapshot >= inFlightTsNs {
		t.Fatalf("snapshot %d covers the change stamped at %d that the listing did not read", snapshot, inFlightTsNs)
	}
}
