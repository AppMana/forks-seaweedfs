package meta_cache

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/protobuf/proto"
)

// A no-change UpdateEntry acknowledgment can have the same version as the
// create already cached. It is not a removal: the source and target are the
// same name. Coarse Windows clocks make these acknowledgments common.
func TestInPlaceDirectoryUpdatePreservesEntryAndChildren(t *testing.T) {
	for _, parent := range []string{"/repo", ""} {
		for _, ts := range []int64{99, 100, 101} {
			t.Run(fmt.Sprintf("parent=%q/ts=%d", parent, ts), func(t *testing.T) {
				mc, _, _, _ := newTestMetaCache(t, map[util.FullPath]bool{"/": true, "/repo": true, "/repo/.git": true})
				defer mc.Shutdown()
				ctx := context.Background()
				dir := &filer_pb.Entry{Name: ".git", IsDirectory: true, Attributes: &filer_pb.FuseAttributes{FileMode: uint32(os.ModeDir) | 0755, Mtime: 1}}
				updated := proto.Clone(dir).(*filer_pb.Entry)
				updated.Attributes.Mtime = 2
				for _, event := range []*filer_pb.SubscribeMetadataResponse{
					{Directory: "/repo", TsNs: 100, EventNotification: &filer_pb.EventNotification{NewEntry: dir, NewParentPath: "/repo"}},
					{Directory: "/repo/.git", TsNs: 100, EventNotification: &filer_pb.EventNotification{NewEntry: &filer_pb.Entry{Name: "intact", Content: []byte("preserve me"), Attributes: &filer_pb.FuseAttributes{FileMode: 0644}}, NewParentPath: "/repo/.git"}},
					{Directory: "/repo", TsNs: ts, EventNotification: &filer_pb.EventNotification{OldEntry: dir, NewEntry: updated, NewParentPath: parent}},
				} {
					if err := mc.ApplyMetadataResponse(ctx, event, LocalMetadataResponseApplyOptions); err != nil {
						t.Fatal(err)
					}
				}
				entry, version, err := mc.FindEntry(ctx, "/repo/.git")
				if err != nil || entry == nil {
					t.Fatalf("in-place update removed .git: %v", err)
				}
				wantMtime, wantVersion := int64(1), int64(100)
				if ts > 100 {
					wantMtime, wantVersion = 2, ts
				}
				if entry.Mtime.Unix() != wantMtime || version != wantVersion {
					t.Fatalf("mtime/version = %d/%d, want %d/%d", entry.Mtime.Unix(), version, wantMtime, wantVersion)
				}
				if entry, _, err := mc.FindEntry(ctx, "/repo/.git/intact"); err != nil || entry == nil || string(entry.Content) != "preserve me" {
					t.Fatalf("in-place update removed intact child: %v", err)
				}
			})
		}
	}
}
