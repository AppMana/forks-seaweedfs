package filer

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"github.com/seaweedfs/seaweedfs/weed/util/log_buffer"
)

func TestChangedDirectories(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event *filer_pb.SubscribeMetadataResponse
		want  []util.FullPath
	}{
		{
			name: "create",
			event: &filer_pb.SubscribeMetadataResponse{Directory: "/d", EventNotification: &filer_pb.EventNotification{
				NewEntry: &filer_pb.Entry{Name: "f"}, NewParentPath: "/d"}},
			want: []util.FullPath{"/d"},
		},
		{
			name: "update in place",
			event: &filer_pb.SubscribeMetadataResponse{Directory: "/d", EventNotification: &filer_pb.EventNotification{
				OldEntry: &filer_pb.Entry{Name: "sub", IsDirectory: true}, NewEntry: &filer_pb.Entry{Name: "sub", IsDirectory: true}, NewParentPath: "/d"}},
			want: []util.FullPath{"/d", "/d"},
		},
		{
			name: "delete directory",
			event: &filer_pb.SubscribeMetadataResponse{Directory: "/d", EventNotification: &filer_pb.EventNotification{
				OldEntry: &filer_pb.Entry{Name: "sub", IsDirectory: true}}},
			want: []util.FullPath{"/d", "/d/sub"},
		},
		{
			name: "move directory",
			event: &filer_pb.SubscribeMetadataResponse{Directory: "/a", EventNotification: &filer_pb.EventNotification{
				OldEntry: &filer_pb.Entry{Name: "sub", IsDirectory: true}, NewEntry: &filer_pb.Entry{Name: "renamed", IsDirectory: true}, NewParentPath: "/b"}},
			want: []util.FullPath{"/a", "/a/sub", "/b", "/b/renamed"},
		},
		{
			name: "create at root",
			event: &filer_pb.SubscribeMetadataResponse{Directory: "/", EventNotification: &filer_pb.EventNotification{
				NewEntry: &filer_pb.Entry{Name: "f"}, NewParentPath: "/"}},
			want: []util.FullPath{"/"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChangedDirectories(tc.event); !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDirectoryChangesRemembersNewestChangePerDirectory(t *testing.T) {
	var d directoryChanges
	d.reset(10)
	if ts, remembered := d.position("/d"); ts != 10 || remembered {
		t.Fatalf("unchanged directory: %d %v, want the floor 10", ts, remembered)
	}
	d.note(20, "/d")
	d.note(15, "/d")
	if ts, remembered := d.position("/d"); ts != 20 || !remembered {
		t.Fatalf("got %d %v, want 20 remembered", ts, remembered)
	}
	if ts, remembered := d.position("/other"); ts != 10 || remembered {
		t.Fatalf("other directory: %d %v, want the floor 10", ts, remembered)
	}
}

// Forgetting raises the floor above every forgotten change, so no directory
// ever answers below a change the filer stamped.
func TestDirectoryChangesForgetsIntoTheFloor(t *testing.T) {
	var d directoryChanges
	d.reset(1)
	for i := 0; i < directoryChangesLimit; i++ {
		d.note(int64(100+i), util.FullPath(fmt.Sprintf("/d%d", i)))
	}
	newest := int64(100 + directoryChangesLimit - 1)
	d.note(newest+1, "/new")
	if ts, remembered := d.position("/d0"); ts != newest || remembered {
		t.Fatalf("forgotten directory: %d %v, want floor %d", ts, remembered, newest)
	}
	if ts, remembered := d.position("/new"); ts != newest+1 || !remembered {
		t.Fatalf("directory noted after forgetting: %d %v", ts, remembered)
	}
}

// A mutation's event names its directory's newest change before the caller is
// answered, so a reader told about the mutation finds it.
func TestNotifyUpdateEventRecordsDirectoryChange(t *testing.T) {
	f := &Filer{
		Signature: 42,
		LocalMetaLogBuffer: log_buffer.NewLogBuffer(
			"test",
			time.Hour,
			func(*log_buffer.LogBuffer, time.Time, time.Time, []byte, int64, int64) {},
			nil,
			nil,
		),
	}
	f.dirChanges.reset(1)

	ctx, sink := WithMetadataEventSink(context.Background())
	f.NotifyUpdateEvent(ctx, nil, &Entry{FullPath: util.FullPath("/ckpt/global_step1/mp_rank_03_model_states.pt")}, false, false, nil)

	event := sink.Last()
	if event == nil {
		t.Fatal("no event")
	}
	if ts, remembered := f.DirectoryChangePosition("/ckpt/global_step1"); ts != event.TsNs || !remembered {
		t.Fatalf("position %d %v, want the event's %d", ts, remembered, event.TsNs)
	}
}
