package weed_server

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// The filer server frees chunks outside the filer's own delete path too: the
// garbage of an UpdateEntry and the target a rename overwrites. Neither may
// free a chunk list another entry still shares, and an entry replicated from
// another cluster must not join a group: the replica wrote chunks of its own.

func newSharedChunksServer(t *testing.T) (*FilerServer, *renameTestStore) {
	t.Helper()
	store := newRenameTestStore()
	f := newRenameTestFiler(t, store)
	f.DirBucketsPath = "/buckets"
	return &FilerServer{filer: f, option: &FilerOption{}, entryLockTable: util.NewLockTable[util.FullPath]()}, store
}

func sharedTestChunks(ids ...string) []*filer_pb.FileChunk {
	out := make([]*filer_pb.FileChunk, len(ids))
	for i, id := range ids {
		out[i] = &filer_pb.FileChunk{FileId: id, Offset: int64(i) << 20, Size: 1 << 20, ModifiedTsNs: time.Now().UnixNano()}
	}
	return out
}

func putSharedTestEntry(t *testing.T, f *filer.Filer, path string, chunks []*filer_pb.FileChunk, ext map[string][]byte) {
	t.Helper()
	now := time.Now()
	entry := &filer.Entry{
		FullPath: util.FullPath(path),
		Attr:     filer.Attr{Mtime: now, Crtime: now, Mode: 0644},
		Chunks:   chunks,
		Extended: ext,
	}
	if err := f.CreateEntry(filer.WithSuppressedMetadataEvents(context.Background()), entry, nil, false, false, nil, false, 255); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
}

// linkSharedTestEntries makes dst share src's chunks the way the S3 gateway
// does: mark src, then create dst with a link hint.
func linkSharedTestEntries(t *testing.T, f *filer.Filer, src, dst string) {
	t.Helper()
	ctx := filer.WithSuppressedMetadataEvents(context.Background())
	source, err := f.FindEntry(ctx, util.FullPath(src))
	if err != nil {
		t.Fatal(err)
	}
	ref, member := filer.ParseSharedChunksRef(source.Extended)
	if !member {
		ref = filer.NewSharedChunksRef(filer.SharedChunksGroupOf(source.GetChunks()))
		marked := source.ShallowClone()
		marked.Extended = map[string][]byte{filer.SharedChunksExtKey: ref.Bytes()}
		for k, v := range source.Extended {
			marked.Extended[k] = v
		}
		if err := f.UpdateEntry(ctx, source, marked); err != nil {
			t.Fatal(err)
		}
	}
	putSharedTestEntry(t, f, dst, source.GetChunks(), map[string][]byte{
		filer.SharedChunksExtKey:           filer.NewSharedChunksRef(ref.Group).Bytes(),
		filer.SharedChunksLinkSourceExtKey: filer.SharedChunksLinkSource(util.FullPath(src), ref),
	})
}

func queuedDeletes(f *filer.Filer) []string {
	var out []string
	f.FileIdDeletionQueue.Consume(func(ids []string) { out = append(out, ids...) })
	f.FileIdDeletionQueue.Consume(func(ids []string) { out = append(out, ids...) })
	sort.Strings(out)
	return out
}

func TestSharedChunksUpdateEntryGarbageKeepsChunksAnotherLinkUses(t *testing.T) {
	fs, _ := newSharedChunksServer(t)
	shared := sharedTestChunks("3,0a01", "3,0a02")
	putSharedTestEntry(t, fs.filer, "/buckets/bkt/src", shared, nil)
	linkSharedTestEntries(t, fs.filer, "/buckets/bkt/src", "/buckets/bkt/dst")
	queuedDeletes(fs.filer)

	// dst rewritten with other content through the UpdateEntry RPC: its old
	// chunks become the handler's garbage
	if _, err := fs.UpdateEntry(context.Background(), &filer_pb.UpdateEntryRequest{
		Directory: "/buckets/bkt",
		Entry: &filer_pb.Entry{
			Name:       "dst",
			Attributes: &filer_pb.FuseAttributes{Mtime: time.Now().Unix(), FileMode: 0644},
			Chunks:     sharedTestChunks("4,0b01"),
		},
	}); err != nil {
		t.Fatalf("UpdateEntry: %v", err)
	}
	for _, id := range queuedDeletes(fs.filer) {
		if id == "3,0a01" || id == "3,0a02" {
			t.Fatalf("UpdateEntry freed chunk %s that the source still shares", id)
		}
	}
}

func TestSharedChunksRenameOntoALinkKeepsChunksAnotherLinkUses(t *testing.T) {
	fs, _ := newSharedChunksServer(t)
	shared := sharedTestChunks("3,0c01")
	putSharedTestEntry(t, fs.filer, "/buckets/bkt/src", shared, nil)
	linkSharedTestEntries(t, fs.filer, "/buckets/bkt/src", "/buckets/bkt/dst")
	putSharedTestEntry(t, fs.filer, "/buckets/bkt/other", sharedTestChunks("4,0d01"), nil)
	queuedDeletes(fs.filer)

	// renaming another object onto dst overwrites a member of the group
	if _, err := fs.AtomicRenameEntry(context.Background(), &filer_pb.AtomicRenameEntryRequest{
		OldDirectory: "/buckets/bkt", OldName: "other",
		NewDirectory: "/buckets/bkt", NewName: "dst",
	}); err != nil {
		t.Fatalf("AtomicRenameEntry: %v", err)
	}
	for _, id := range queuedDeletes(fs.filer) {
		if id == "3,0c01" {
			t.Fatalf("rename freed chunk %s that the source still shares", id)
		}
	}
	if _, err := fs.filer.FindEntry(context.Background(), "/buckets/bkt/src"); err != nil {
		t.Fatalf("source gone: %v", err)
	}
}

func TestSharedChunksEntryFromAnotherClusterDoesNotJoinAGroup(t *testing.T) {
	fs, store := newSharedChunksServer(t)
	ref := filer.NewSharedChunksRef(filer.SharedChunksGroupOf(sharedTestChunks("5,0e01")))
	if _, err := fs.CreateEntry(context.Background(), &filer_pb.CreateEntryRequest{
		Directory: "/buckets/bkt",
		Entry: &filer_pb.Entry{
			Name:       "replicated",
			Attributes: &filer_pb.FuseAttributes{Mtime: time.Now().Unix(), FileMode: 0644},
			Chunks:     sharedTestChunks("5,0e01"),
			Extended: map[string][]byte{
				filer.SharedChunksExtKey:           ref.Bytes(),
				filer.SharedChunksLinkSourceExtKey: filer.SharedChunksLinkSource("/buckets/bkt/elsewhere", ref),
			},
		},
		IsFromOtherCluster: true,
	}); err != nil {
		t.Fatalf("CreateEntry from another cluster: %v", err)
	}
	stored := store.entries["/buckets/bkt/replicated"]
	if stored == nil {
		t.Fatal("replicated entry not stored")
	}
	if _, member := stored.Extended[filer.SharedChunksExtKey]; member {
		t.Fatalf("an entry from another cluster joined a shared chunks group")
	}
	if _, hint := stored.Extended[filer.SharedChunksLinkSourceExtKey]; hint {
		t.Fatalf("a link hint was stored")
	}
	if _, refErr := fs.filer.FindEntry(context.Background(), ref.RefPath()); refErr == nil {
		t.Fatalf("a reference was written for an entry from another cluster")
	}
}
