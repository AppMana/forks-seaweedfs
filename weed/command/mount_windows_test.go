package command

import (
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

func TestWinfspNotificationsDelete(t *testing.T) {
	resp := &filer_pb.SubscribeMetadataResponse{
		Directory: "/buckets/pvc-1/dir",
		EventNotification: &filer_pb.EventNotification{
			OldEntry: &filer_pb.Entry{Name: "gone.txt"},
		},
	}
	ns := winfspNotifications("/buckets/pvc-1", resp)
	if len(ns) != 1 || ns[0].path != "/dir/gone.txt" || ns[0].action != uint32(mount.NotifyUnlink) {
		t.Fatalf("unexpected: %+v", ns)
	}
}

func TestWinfspNotificationsCreateDirAtRoot(t *testing.T) {
	resp := &filer_pb.SubscribeMetadataResponse{
		Directory: "/buckets/pvc-1",
		EventNotification: &filer_pb.EventNotification{
			NewEntry:      &filer_pb.Entry{Name: "newdir", IsDirectory: true},
			NewParentPath: "/buckets/pvc-1",
		},
	}
	ns := winfspNotifications("/buckets/pvc-1", resp)
	if len(ns) != 1 || ns[0].path != "/newdir" || ns[0].action != uint32(mount.NotifyMkdir) {
		t.Fatalf("unexpected: %+v", ns)
	}
}

func TestWinfspNotificationsRename(t *testing.T) {
	resp := &filer_pb.SubscribeMetadataResponse{
		Directory: "/buckets/pvc-1/a",
		EventNotification: &filer_pb.EventNotification{
			OldEntry:      &filer_pb.Entry{Name: "x.txt"},
			NewEntry:      &filer_pb.Entry{Name: "y.txt"},
			NewParentPath: "/buckets/pvc-1/b",
		},
	}
	ns := winfspNotifications("/buckets/pvc-1", resp)
	if len(ns) != 2 {
		t.Fatalf("want 2 notifications, got %+v", ns)
	}
	if ns[0].path != "/a/x.txt" || ns[0].action != uint32(mount.NotifyUnlink) {
		t.Fatalf("old leg: %+v", ns[0])
	}
	if ns[1].path != "/b/y.txt" || ns[1].action != uint32(mount.NotifyCreate) {
		t.Fatalf("new leg: %+v", ns[1])
	}
}

func TestWinfspNotificationsUpdate(t *testing.T) {
	resp := &filer_pb.SubscribeMetadataResponse{
		Directory: "/buckets/pvc-1",
		EventNotification: &filer_pb.EventNotification{
			OldEntry: &filer_pb.Entry{Name: "f.txt"},
			NewEntry: &filer_pb.Entry{Name: "f.txt"},
		},
	}
	ns := winfspNotifications("/buckets/pvc-1", resp)
	if len(ns) != 1 || ns[0].path != "/f.txt" || ns[0].action != uint32(mount.NotifyTruncate|mount.NotifyUtime) {
		t.Fatalf("unexpected: %+v", ns)
	}
}

func TestWinfspNotificationsOutsideRootIgnored(t *testing.T) {
	resp := &filer_pb.SubscribeMetadataResponse{
		Directory: "/other/place",
		EventNotification: &filer_pb.EventNotification{
			OldEntry: &filer_pb.Entry{Name: "f.txt"},
		},
	}
	if ns := winfspNotifications("/buckets/pvc-1", resp); len(ns) != 0 {
		t.Fatalf("expected no notifications, got %+v", ns)
	}
}

func TestWinfspNotificationsRootMount(t *testing.T) {
	resp := &filer_pb.SubscribeMetadataResponse{
		Directory: "/anywhere",
		EventNotification: &filer_pb.EventNotification{
			NewEntry: &filer_pb.Entry{Name: "f.txt"},
		},
	}
	ns := winfspNotifications("/", resp)
	if len(ns) != 1 || ns[0].path != "/anywhere/f.txt" {
		t.Fatalf("unexpected: %+v", ns)
	}
}
