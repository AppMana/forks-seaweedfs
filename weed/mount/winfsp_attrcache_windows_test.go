//go:build windows

package mount

import (
	"context"
	"testing"
	"time"

	cgofuse "github.com/winfsp/cgofuse/fuse"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// changeUnannounced changes an entry on the filer, and in the metadata cache
// if it holds the directory, the way a change looks before its event has been
// applied.
func changeUnannounced(t *testing.T, wfs *WFS, fake *treeFiler, dir string, entry *filer_pb.Entry) {
	t.Helper()
	fake.add(dir, entry)
	if err := wfs.metaCache.UpdateEntry(context.Background(), filer.FromPbEntry(dir, entry), 0); err != nil {
		t.Fatal(err)
	}
}

func getattrMode(t *testing.T, a *winfspFS, path string) uint32 {
	t.Helper()
	var stat cgofuse.Stat_t
	if status := a.Getattr(path, &stat, invalidFh); status != 0 {
		t.Fatalf("getattr %s: %d", path, status)
	}
	return stat.Mode & 0o7777
}

// WinFsp asks for attributes by path again and again. Within the cache TTL
// the adapter answers from what it resolved, the way the kernel's attribute
// cache answers on the unix mounts, until something invalidates it.
func TestWinfspGetattrServesCachedAttributes(t *testing.T) {
	fake := newTreeFiler()
	fake.add("/", treeDir("d"))
	fake.add("/d", treeFile("f", 0o644))
	wfs, a := newWinfspTreeWFS(t, fake)

	if mode := getattrMode(t, a, "/d/f"); mode != 0o644 {
		t.Fatalf("mode = %o, want 644", mode)
	}
	changeUnannounced(t, wfs, fake, "/d", treeFile("f", 0o600))
	if mode := getattrMode(t, a, "/d/f"); mode != 0o644 {
		t.Fatalf("mode = %o within the cache TTL, want the cached 644", mode)
	}
	a.expireReferences(time.Now().Add(time.Hour))
	if mode := getattrMode(t, a, "/d/f"); mode != 0o600 {
		t.Fatalf("mode = %o after the TTL, want 600", mode)
	}

	// An applied metadata event purges the cached attributes at once.
	changeUnannounced(t, wfs, fake, "/d", treeFile("f", 0o640))
	if mode := getattrMode(t, a, "/d/f"); mode != 0o600 {
		t.Fatalf("mode = %o, want the cached 600", mode)
	}
	if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), &filer_pb.SubscribeMetadataResponse{
		Directory: "/d", TsNs: 1000, EventNotification: &filer_pb.EventNotification{
			OldEntry: treeFile("f", 0o600), NewEntry: treeFile("f", 0o640), NewParentPath: "/d",
		},
	}, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	wfs.metaCache.WaitForEntryInvalidations()
	if mode := getattrMode(t, a, "/d/f"); mode != 0o640 {
		t.Fatalf("mode = %o after the event, want 640", mode)
	}

	// Closing a file drops what was cached before it was opened.
	changeUnannounced(t, wfs, fake, "/d", treeFile("f", 0o604))
	status, fh := a.Open("/d/f", cgofuse.O_RDONLY)
	if status != 0 {
		t.Fatalf("open: %d", status)
	}
	a.Release("/d/f", fh)
	if mode := getattrMode(t, a, "/d/f"); mode != 0o604 {
		t.Fatalf("mode = %o after a close, want 604", mode)
	}
}

// A file with an open handle reports the handle's live size, never a cached
// one: writes through the handle grow it before anything reaches the filer.
func TestWinfspGetattrOfAnOpenFileIsNotCached(t *testing.T) {
	fake := newTreeFiler()
	fake.add("/", treeDir("d"))
	fake.add("/d", treeFile("f", 0o644))
	wfs, a := newWinfspTreeWFS(t, fake)
	wfs.option.MountUid, wfs.option.MountGid = 0, 0

	var stat cgofuse.Stat_t
	if status := a.Getattr("/d/f", &stat, invalidFh); status != 0 || stat.Size != 1 {
		t.Fatalf("getattr: %d size %d", status, stat.Size)
	}
	status, fh := a.Open("/d/f", cgofuse.O_RDWR)
	if status != 0 {
		t.Fatalf("open: %d", status)
	}
	for size := int64(2); size <= 4; size++ {
		if n := a.Write("/d/f", []byte("y"), size-1, fh); n != 1 {
			t.Fatalf("write: %d", n)
		}
		if status := a.Getattr("/d/f", &stat, invalidFh); status != 0 || stat.Size != size {
			t.Fatalf("getattr by path with the file open: %d size %d, want %d", status, stat.Size, size)
		}
	}
}

// Every local mutation of a path is visible to the next Getattr, cached
// attributes or not.
func TestWinfspLocalMutationsPurgeCachedAttributes(t *testing.T) {
	fake := newTreeFiler()
	fake.add("/", treeDir("d"))
	fake.add("/d", treeFile("f", 0o644))
	wfs, a := newWinfspTreeWFS(t, fake)
	wfs.option.MountUid, wfs.option.MountGid = 0, 0

	stat := func() cgofuse.Stat_t {
		t.Helper()
		var st cgofuse.Stat_t
		if status := a.Getattr("/d/f", &st, invalidFh); status != 0 {
			t.Fatalf("getattr: %d", status)
		}
		return st
	}
	stat()
	if code := a.Chmod("/d/f", 0o600); code != 0 {
		t.Fatalf("chmod: %d", code)
	}
	if mode := stat().Mode & 0o7777; mode != 0o600 {
		t.Fatalf("mode = %o after chmod, want 600", mode)
	}
	if code := a.Utimens("/d/f", []cgofuse.Timespec{{Sec: 1700000000}, {Sec: 1700000000}}); code != 0 {
		t.Fatalf("utimens: %d", code)
	}
	if mtime := stat().Mtim.Sec; mtime != 1700000000 {
		t.Fatalf("mtime = %d after utimens, want 1700000000", mtime)
	}
	if code := a.Truncate("/d/f", 0, invalidFh); code != 0 {
		t.Fatalf("truncate: %d", code)
	}
	if size := stat().Size; size != 0 {
		t.Fatalf("size = %d after truncate, want 0", size)
	}
	if code := a.Chflags("/d/f", cgofuse.UF_HIDDEN); code != 0 {
		t.Fatalf("chflags: %d", code)
	}
	if flags := stat().Flags; flags != cgofuse.UF_HIDDEN {
		t.Fatalf("flags = %#x after chflags, want hidden", flags)
	}
	if code := a.Setcrtime("/d/f", cgofuse.Timespec{Sec: 1600000000}); code != 0 {
		t.Fatalf("setcrtime: %d", code)
	}
	if crtime := stat().Birthtim.Sec; crtime != 1600000000 {
		t.Fatalf("creation time = %d after setcrtime, want 1600000000", crtime)
	}
}

func TestWinfspHardLinkSiblingAndReadAheadInvalidation(t *testing.T) {
	fake := newTreeFiler()
	fake.add("/", treeDir("d"))
	fake.add("/d", treeFile("src", 0o644))
	for _, name := range []string{"a", "b"} {
		entry := treeFile(name, 0o644)
		entry.HardLinkId = []byte("hl-stat")
		entry.HardLinkCounter = 2
		entry.Attributes.Inode = 5151
		fake.add("/d", entry)
	}
	wfs, a := newWinfspTreeWFS(t, fake)
	wfs.option.MountUid, wfs.option.MountGid = 0, 0

	// Cache /d/b's attributes (Nlink=2), then overwrite its sibling /d/a via Rename.
	var stat cgofuse.Stat_t
	if code := a.Getattr("/d/b", &stat, invalidFh); code != 0 || stat.Nlink != 2 {
		t.Fatalf("getattr /d/b before rename = %d nlink %d, want 0 nlink 2", code, stat.Nlink)
	}
	if code := a.Rename("/d/src", "/d/a"); code != 0 {
		t.Fatalf("rename /d/src -> /d/a: %d", code)
	}
	if code := a.Getattr("/d/b", &stat, invalidFh); code != 0 || stat.Nlink != 1 {
		t.Fatalf("getattr /d/b after rename replaced sibling /d/a = %d nlink %d, want 0 nlink 1", code, stat.Nlink)
	}

	// Populate read-ahead on /d/b, then truncate /d/b via Create(O_TRUNC).
	code, fhRead := a.Open("/d/b", cgofuse.O_RDONLY)
	if code != 0 {
		t.Fatalf("open /d/b: %d", code)
	}
	defer a.Release("/d/b", fhRead)
	buf := make([]byte, 1)
	if n := a.Read("/d/b", buf, 0, fhRead); n != 1 {
		t.Fatalf("initial read /d/b = %d, want 1", n)
	}
	code, fhTrunc := a.Create("/d/b", cgofuse.O_RDWR|cgofuse.O_TRUNC, 0o644)
	if code != 0 {
		t.Fatalf("create O_TRUNC /d/b: %d", code)
	}
	defer a.Release("/d/b", fhTrunc)
	if n := a.Read("/d/b", buf, 0, fhRead); n != 0 {
		t.Fatalf("read /d/b after Create(O_TRUNC) = %d, want 0 (read-ahead invalidated)", n)
	}
}
