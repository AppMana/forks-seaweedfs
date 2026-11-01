//go:build windows

package mount

import (
	"context"
	"testing"

	cgofuse "github.com/winfsp/cgofuse/fuse"
	"google.golang.org/protobuf/proto"

	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

func openEx(t *testing.T, a *winfspFS, path string) (uint64, bool) {
	t.Helper()
	fi := cgofuse.FileInfo_t{Flags: cgofuse.O_RDONLY}
	if status := a.OpenEx(path, &fi); status != 0 {
		t.Fatalf("open %s: %d", path, status)
	}
	return fi.Fh, fi.DirectIo
}

// rewrite applies an in-place content update of name in the root, keeping
// its size and times, as another mount's same-size rewrite arrives.
func rewrite(t *testing.T, wfs *WFS, fake *treeFiler, name string, signatures ...int32) {
	t.Helper()
	old := treeFile(name, 0o644)
	next := proto.Clone(old).(*filer_pb.Entry)
	next.Content = []byte("y")
	fake.add("/", next)
	if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), &filer_pb.SubscribeMetadataResponse{
		Directory: "/", TsNs: 1000, EventNotification: &filer_pb.EventNotification{
			OldEntry: old, NewEntry: next, NewParentPath: "/", Signatures: signatures,
		},
	}, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	wfs.metaCache.WaitForEntryInvalidations()
}

// Windows cannot purge the cache section of a file a process maps, so after
// another mount changes a file Windows holds open, every open of that name
// has to read through the file system until the last handle on it closes.
// Files nobody else changed, and changes this mount made itself, keep the
// Windows cache.
func TestWinfspOpenAfterRemoteChangeWhileHeldBypassesWindowsCache(t *testing.T) {
	fake := newTreeFiler()
	fake.add("/", treeFile("data", 0o644))
	fake.add("/", treeFile("own", 0o644))
	fake.add("/", treeFile("other", 0o644))
	wfs, a := newWinfspTreeWFS(t, fake)

	held, uncached := openEx(t, a, "/data")
	if uncached {
		t.Fatal("first open of an unchanged file bypasses the cache")
	}
	ownHeld, _ := openEx(t, a, "/own")
	otherHeld, _ := openEx(t, a, "/other")

	rewrite(t, wfs, fake, "data")
	rewrite(t, wfs, fake, "own", wfs.signature)

	reopened, uncached := openEx(t, a, "/data")
	if !uncached {
		t.Fatal("reopen of a file another mount changed while it was open keeps the Windows cache")
	}
	ownReopened, uncached := openEx(t, a, "/own")
	if uncached {
		t.Fatal("this mount's own change made its file bypass the cache")
	}
	otherReopened, uncached := openEx(t, a, "/other")
	if uncached {
		t.Fatal("an unchanged open file bypasses the cache")
	}
	for _, release := range []struct {
		path string
		fh   uint64
	}{{"/own", ownHeld}, {"/own", ownReopened}, {"/other", otherHeld}, {"/other", otherReopened}, {"/data", reopened}} {
		a.Release(release.path, release.fh)
	}
	third, uncached := openEx(t, a, "/data")
	if !uncached {
		t.Fatal("closing one of the handles on the changed file restored caching")
	}
	a.Release("/data", third)
	a.Release("/data", held)
	last, uncached := openEx(t, a, "/data")
	if uncached {
		t.Fatal("caching stayed off after every handle on the changed file closed")
	}
	a.Release("/data", last)
}

// WinFsp renames the open file object with the name, so a file renamed here
// while open is held under its new name.
func TestWinfspRemoteChangeAfterLocalRenameOfHeldFileBypassesWindowsCache(t *testing.T) {
	fake := newTreeFiler()
	fake.add("/", treeFile("data", 0o644))
	wfs, a := newWinfspTreeWFS(t, fake)

	held, _ := openEx(t, a, "/data")
	if status := a.Rename("/data", "/moved"); status != 0 {
		t.Fatalf("rename: %d", status)
	}
	rewrite(t, wfs, fake, "moved")
	reopened, uncached := openEx(t, a, "/moved")
	if !uncached {
		t.Fatal("reopen of a renamed held file changed elsewhere keeps the Windows cache")
	}
	a.Release("/moved", reopened)
	a.Release("/data", held)
	last, uncached := openEx(t, a, "/moved")
	if uncached {
		t.Fatal("caching stayed off after every handle closed")
	}
	a.Release("/moved", last)
}
