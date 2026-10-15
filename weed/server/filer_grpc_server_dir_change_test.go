package weed_server

import (
	"context"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util/log_buffer"
)

func newPositionTestFiler() *filer.Filer {
	return &filer.Filer{
		Signature:     42,
		UniqueFilerId: 1234,
		LocalMetaLogBuffer: log_buffer.NewLogBuffer("test", time.Hour,
			func(*log_buffer.LogBuffer, time.Time, time.Time, []byte, int64, int64) {}, nil, nil),
	}
}

// A filer names its own log position for the directory, leaving out the
// caller's changes, since the caller's stream never delivers them.
func TestDirectoryChangePositionNamesThisFilersLogLeavingOutTheCaller(t *testing.T) {
	f := newPositionTestFiler()
	fs := &FilerServer{filer: f}
	const mountA, mountB = 7, 8
	f.NotifyUpdateEvent(context.Background(), nil, &filer.Entry{FullPath: "/ckpt/peer.pt"}, false, false, []int32{mountB})
	peerPosition, _ := f.DirectoryWriterPosition("/ckpt", 0)
	f.NotifyUpdateEvent(context.Background(), nil, &filer.Entry{FullPath: "/ckpt/own.pt"}, false, false, []int32{mountA})

	resp, err := fs.DirectoryChangePosition(context.Background(), &filer_pb.DirectoryChangePositionRequest{Directory: "/ckpt", Signature: mountA})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Filers) != 1 {
		t.Fatalf("got %d filer positions, want this filer's", len(resp.Filers))
	}
	got := resp.Filers[0]
	if got.FilerId != f.UniqueFilerId || got.PositionNs != peerPosition || !got.Remembered {
		t.Fatalf("got %+v, want filer %d at mount B's change %d", got, f.UniqueFilerId, peerPosition)
	}
}

// Events streamed from the filer's memory carry where it appended them; ones
// already marked by the filer that logged them keep that mark.
func TestStampLocalOriginMarksAppendPosition(t *testing.T) {
	fs := &FilerServer{filer: newPositionTestFiler()}
	event := &filer_pb.SubscribeMetadataResponse{TsNs: 100, EventNotification: &filer_pb.EventNotification{}}
	fs.stampLocalOrigin(event, &filer_pb.LogEntry{TsNs: 105})
	if n := event.EventNotification; n.OriginFilerId != 1234 || n.OriginPositionNs != 105 {
		t.Fatalf("got origin %d at %d, want 1234 at the entry's position 105", n.OriginFilerId, n.OriginPositionNs)
	}
	relayed := &filer_pb.SubscribeMetadataResponse{EventNotification: &filer_pb.EventNotification{OriginFilerId: 99, OriginPositionNs: 7}}
	fs.stampLocalOrigin(relayed, &filer_pb.LogEntry{TsNs: 105})
	if n := relayed.EventNotification; n.OriginFilerId != 99 || n.OriginPositionNs != 7 {
		t.Fatalf("restamped a marked event: %d at %d", n.OriginFilerId, n.OriginPositionNs)
	}
}
