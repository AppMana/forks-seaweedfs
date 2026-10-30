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

// A caller's own changes are left out of the position it is answered, and
// every other writer's newest change is still found behind them.
func TestDirectoryWriterChangesLeaveOutTheCaller(t *testing.T) {
	var d directoryWriterChanges
	const mountA, mountB, store = 7, 8, 42
	d.note(10, []int32{mountB, store}, "/d")
	d.note(20, []int32{mountA, store}, "/d")
	if p, remembered := d.position("/d", mountA); p != 10 || !remembered {
		t.Fatalf("mount A: %d %v, want mount B's change at 10", p, remembered)
	}
	if p, remembered := d.position("/d", mountB); p != 20 || !remembered {
		t.Fatalf("mount B: %d %v, want mount A's change at 20", p, remembered)
	}
	if p, remembered := d.position("/d", 0); p != 20 || !remembered {
		t.Fatalf("no caller: %d %v, want the newest change at 20", p, remembered)
	}
	d.note(30, []int32{mountB, store}, "/d")
	if p, _ := d.position("/d", mountA); p != 30 {
		t.Fatalf("mount A: %d, want mount B's newer change at 30", p)
	}
	if p, remembered := d.position("/other", mountA); p != 0 || remembered {
		t.Fatalf("unchanged directory: %d %v, want the floor 0", p, remembered)
	}
	// a directory only the caller changed has nothing for it to wait for
	d.note(40, []int32{mountA, store}, "/mine")
	if p, remembered := d.position("/mine", mountA); p != 0 || remembered {
		t.Fatalf("caller-only directory: %d %v, want the floor 0", p, remembered)
	}
}

// Writers past the per-directory bound are forgotten into the directory's
// floor, which then stands for any of them.
func TestDirectoryWriterChangesForgetLeastRecentWriter(t *testing.T) {
	var d directoryWriterChanges
	for i := 0; i < directoryChangeWriters+1; i++ {
		d.note(int64(100+i), []int32{int32(1000 + i)}, "/d")
	}
	// the first writer's change at 100 is forgotten
	if p, remembered := d.position("/d", 0); p != int64(100+directoryChangeWriters) || !remembered {
		t.Fatalf("newest: %d %v", p, remembered)
	}
	// once another writer's change is forgotten behind changes all carrying
	// the caller's signature, the caller gets the floor that covers it
	var only directoryWriterChanges
	only.note(200, []int32{9}, "/e")
	for i := 0; i < directoryChangeWriters; i++ {
		only.note(int64(300+i), []int32{int32(2000 + i), 5}, "/e")
	}
	if p, remembered := only.position("/e", 5); p != 200 || remembered {
		t.Fatalf("caller in every remembered change: %d %v, want the forgotten floor 200", p, remembered)
	}
}

// Forgetting every directory raises the floor above every forgotten change.
func TestDirectoryWriterChangesForgetIntoTheFloor(t *testing.T) {
	var d directoryWriterChanges
	for i := 0; i < directoryChangesLimit; i++ {
		d.note(int64(100+i), nil, util.FullPath(fmt.Sprintf("/d%d", i)))
	}
	newest := int64(100 + directoryChangesLimit - 1)
	d.note(newest+1, nil, "/new")
	if p, remembered := d.position("/d0", 0); p != newest || remembered {
		t.Fatalf("forgotten directory: %d %v, want floor %d", p, remembered, newest)
	}
	if p, remembered := d.position("/new", 0); p != newest+1 || !remembered {
		t.Fatalf("directory noted after forgetting: %d %v", p, remembered)
	}
}

// A change is recorded at its position in the filer's log, which rises in
// append order even when the event's own timestamp, stamped earlier, does not.
func TestLogMetaEventRecordsLogPosition(t *testing.T) {
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
	newer := &filer_pb.SubscribeMetadataResponse{Directory: "/d", TsNs: 2000, EventNotification: &filer_pb.EventNotification{
		NewEntry: &filer_pb.Entry{Name: "b"}, NewParentPath: "/d", Signatures: []int32{8, 42}}}
	older := &filer_pb.SubscribeMetadataResponse{Directory: "/d", TsNs: 1000, EventNotification: &filer_pb.EventNotification{
		NewEntry: &filer_pb.Entry{Name: "a"}, NewParentPath: "/d", Signatures: []int32{7, 42}}}
	// stamped 1000 then 2000, appended in the other order
	f.logMetaEvent(context.Background(), newer)
	f.logMetaEvent(context.Background(), older)
	p, remembered := f.DirectoryWriterPosition("/d", 0)
	if !remembered || p <= 2000 {
		t.Fatalf("position %d %v, want the later append's buffer position above 2000", p, remembered)
	}
	if p8, _ := f.DirectoryWriterPosition("/d", 7); p8 != 2000 {
		t.Fatalf("leaving out writer 7: %d, want writer 8's position 2000", p8)
	}
}

// When notifyUpdateEvent appends an event whose pre-assigned timestamp is behind
// the log buffer's high-water mark, AddDataToBufferAndGetTs bumps event.TsNs to
// the monotonic log position. DirectoryChangePosition must record that post-bump
// log position so LastDirectoryChangeTsNs never lags behind event.TsNs.
func TestNotifyUpdateEventRecordsBumpedLogPositionInDirectoryChanges(t *testing.T) {
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

	olderTs := f.ReserveEventTsNs()
	newerTs := f.ReserveEventTsNs()

	ctx, sink := WithMetadataEventSink(context.Background())
	f.NotifyUpdateEventAt(ctx, nil, &Entry{
		FullPath: util.FullPath("/d/b"),
	}, false, false, []int32{8}, newerTs)
	firstEvent := sink.Last()
	if firstEvent == nil {
		t.Fatal("expected first metadata event")
	}

	f.NotifyUpdateEventAt(ctx, nil, &Entry{
		FullPath: util.FullPath("/d/a"),
	}, false, false, []int32{7}, olderTs)

	event := sink.Last()
	if event == nil {
		t.Fatal("expected recorded metadata event")
	}
	if event.TsNs <= firstEvent.TsNs {
		t.Fatalf("event.TsNs = %d, want bumped position > %d", event.TsNs, firstEvent.TsNs)
	}
	if pos, remembered := f.DirectoryChangePosition("/d"); !remembered || pos != event.TsNs {
		t.Fatalf("DirectoryChangePosition(/d) = (%d, %v), want (%d, true)", pos, remembered, event.TsNs)
	}
}

