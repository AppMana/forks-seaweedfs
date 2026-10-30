//go:build windows

package mount

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	cgofuse "github.com/winfsp/cgofuse/fuse"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// treeFiler serves a mutable namespace for adapter tests that walk it.
type treeFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	mu   sync.Mutex
	dirs map[string]map[string]*filer_pb.Entry
}

func newTreeFiler() *treeFiler {
	return &treeFiler{dirs: map[string]map[string]*filer_pb.Entry{"/": {}}}
}

func (s *treeFiler) add(dir string, entry *filer_pb.Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirs[dir] == nil {
		s.dirs[dir] = map[string]*filer_pb.Entry{}
	}
	s.dirs[dir][entry.Name] = entry
	if entry.IsDirectory {
		child := string(util.FullPath(dir).Child(entry.Name))
		if s.dirs[child] == nil {
			s.dirs[child] = map[string]*filer_pb.Entry{}
		}
	}
}

func (s *treeFiler) remove(dir, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.dirs[dir], name)
}

func (s *treeFiler) LookupDirectoryEntry(_ context.Context, req *filer_pb.LookupDirectoryEntryRequest) (*filer_pb.LookupDirectoryEntryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, found := s.dirs[req.Directory][req.Name]; found {
		return &filer_pb.LookupDirectoryEntryResponse{Entry: entry}, nil
	}
	return nil, filer_pb.ErrNotFound
}

func (s *treeFiler) ListEntries(req *filer_pb.ListEntriesRequest, stream filer_pb.SeaweedFiler_ListEntriesServer) error {
	s.mu.Lock()
	var entries []*filer_pb.Entry
	for _, entry := range s.dirs[req.Directory] {
		entries = append(entries, entry)
	}
	s.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	var sent uint32
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name, req.Prefix) {
			continue
		}
		if entry.Name < req.StartFromFileName || (entry.Name == req.StartFromFileName && !req.InclusiveStartFrom) {
			continue
		}
		if req.Limit > 0 && sent >= req.Limit {
			break
		}
		if err := stream.Send(&filer_pb.ListEntriesResponse{Entry: entry}); err != nil {
			return err
		}
		sent++
	}
	return nil
}

func treeDir(name string) *filer_pb.Entry {
	now := time.Now().Unix()
	return &filer_pb.Entry{Name: name, IsDirectory: true, Attributes: &filer_pb.FuseAttributes{
		FileMode: uint32(os.ModeDir | 0o755), Crtime: now, Mtime: now,
	}}
}

func treeFile(name string, mode uint32) *filer_pb.Entry {
	now := time.Now().Unix()
	return &filer_pb.Entry{Name: name, Content: []byte("x"), Attributes: &filer_pb.FuseAttributes{
		FileMode: mode, Crtime: now, Mtime: now, FileSize: 1,
	}}
}

// newWinfspTreeWFS is a WFS over fake whose applied metadata events reach the
// adapter the way NewWinFspHost wires them.
func newWinfspTreeWFS(t *testing.T, fake *treeFiler) (*WFS, *winfspFS) {
	t.Helper()
	uidGidMapper, err := meta_cache.NewUidGidMapper("", "")
	if err != nil {
		t.Fatal(err)
	}
	root := util.FullPath("/")
	wfs := &WFS{
		option: &Option{
			ChunkSizeLimit: 1024, ConcurrentReaders: 1, VolumeServerAccess: "filerProxy",
			FilerAddresses:     []pb.ServerAddress{pb.NewServerAddressWithGrpcPort("127.0.0.1:1", 1)},
			GrpcDialOption:     grpc.WithTransportCredentials(insecure.NewCredentials()),
			FilerMountRootPath: "/", MountUid: 99, MountGid: 100, MountMode: 0o777,
			MountMtime: time.Now(), MountCtime: time.Now(), UidGidMapper: uidGidMapper,
		},
		signature: 1, inodeToPath: NewInodeToPath(root, 0),
		fhMap: NewFileHandleToInode(), dhMap: NewDirectoryHandleToInode(),
		fhLockTable: util.NewLockTable[FileHandleId](), hardLinkLockTable: util.NewLockTable[string](),
		atimeMap: make(map[uint64]time.Time), pendingAsyncFlush: make(map[uint64]chan struct{}),
	}
	wfs.metaCache = meta_cache.NewMetaCache(
		filepath.Join(t.TempDir(), "meta"), uidGidMapper, root, false,
		func(p util.FullPath) { wfs.inodeToPath.MarkChildrenCached(p) },
		func(p util.FullPath) bool { return wfs.inodeToPath.IsChildrenCached(p) },
		wfs.onEntryInvalidation, nil,
	)
	t.Cleanup(wfs.metaCache.Shutdown)
	startFakeFiler(t, wfs, fake)
	a := newWinfspFS(wfs, false)
	wfs.SetEntryChangeListener(a.entryChanged)
	return wfs, a
}

func trackedPaths(wfs *WFS) []string {
	wfs.inodeToPath.RLock()
	defer wfs.inodeToPath.RUnlock()
	var paths []string
	for p := range wfs.inodeToPath.path2inode {
		paths = append(paths, string(p))
	}
	sort.Strings(paths)
	if len(paths) > 8 {
		paths = append(paths[:8], fmt.Sprintf("... %d more", len(paths)-8))
	}
	return paths
}

// walkLikeExplorer lists every directory under path and stats and opens every
// file in it, closing everything it opens.
func walkLikeExplorer(t *testing.T, a *winfspFS, path string) (files int) {
	t.Helper()
	status, dh := a.Opendir(path)
	if status != 0 {
		t.Fatalf("opendir %s: %d", path, status)
	}
	type child struct {
		name string
		dir  bool
	}
	var children []child
	if status := a.Readdir(path, func(name string, stat *cgofuse.Stat_t, _ int64) bool {
		if name != "." && name != ".." {
			children = append(children, child{name, stat.Mode&cgofuse.S_IFMT == cgofuse.S_IFDIR})
		}
		return true
	}, 0, dh); status != 0 {
		t.Fatalf("readdir %s: %d", path, status)
	}
	a.Releasedir(path, dh)
	for _, c := range children {
		childPath := strings.TrimRight(path, "/") + "/" + c.name
		var stat cgofuse.Stat_t
		if status := a.Getattr(childPath, &stat, invalidFh); status != 0 {
			t.Fatalf("getattr %s: %d", childPath, status)
		}
		if c.dir {
			files += walkLikeExplorer(t, a, childPath)
			continue
		}
		status, fh := a.Open(childPath, cgofuse.O_RDONLY)
		if status != 0 {
			t.Fatalf("open %s: %d", childPath, status)
		}
		buf := make([]byte, 4)
		if n := a.Read(childPath, buf, 0, fh); n != 1 {
			t.Fatalf("read %s: %d", childPath, n)
		}
		a.Release(childPath, fh)
		files++
	}
	return files
}

func newWalkedTree(t *testing.T) (*treeFiler, *WFS, *winfspFS) {
	t.Helper()
	fake := newTreeFiler()
	fake.add("/", treeDir("tree"))
	for d := 0; d < 20; d++ {
		dir := fmt.Sprintf("d%02d", d)
		fake.add("/tree", treeDir(dir))
		for f := 0; f < 50; f++ {
			fake.add("/tree/"+dir, treeFile(fmt.Sprintf("f%03d", f), 0o644))
		}
	}
	wfs, a := newWinfspTreeWFS(t, fake)
	if files := walkLikeExplorer(t, a, "/"); files != 1000 {
		t.Fatalf("walked %d files, want 1000", files)
	}
	return fake, wfs, a
}

// WinFsp never sends FORGET. Every name the adapter resolves or lists has to
// give its lookup reference back, or the inode table grows with every path
// the mount ever touches.
func TestWinfspInodeTableStaysBoundedAfterWalkingATree(t *testing.T) {
	_, wfs, a := newWalkedTree(t)

	status, fh := a.Open("/tree/d03/f007", cgofuse.O_RDONLY)
	if status != 0 {
		t.Fatalf("open: %d", status)
	}
	status, dh := a.Opendir("/tree/d05")
	if status != 0 {
		t.Fatalf("opendir: %d", status)
	}
	fileIno, _ := wfs.inodeToPath.GetInode("/tree/d03/f007")
	dirIno, _ := wfs.inodeToPath.GetInode("/tree/d05")

	a.expireReferences(time.Now().Add(time.Hour))
	if size := inodeTableSize(wfs.inodeToPath); size != 3 {
		t.Fatalf("inode table holds %d entries after the walk, want the root and the two open handles: %v", size, trackedPaths(wfs))
	}
	for path, ino := range map[util.FullPath]uint64{"/tree/d03/f007": fileIno, "/tree/d05": dirIno} {
		if got, status := wfs.inodeToPath.GetPath(ino); status != 0 || got != path {
			t.Fatalf("open handle's inode %d maps to %q, %v; want %s", ino, got, status, path)
		}
	}
	buf := make([]byte, 4)
	if n := a.Read("/tree/d03/f007", buf, 0, fh); n != 1 {
		t.Fatalf("read through the open handle after expiry: %d", n)
	}

	a.Release("/tree/d03/f007", fh)
	a.Releasedir("/tree/d05", dh)
	a.expireReferences(time.Now().Add(2 * time.Hour))
	if size := inodeTableSize(wfs.inodeToPath); size != 1 {
		t.Fatalf("inode table holds %d entries with nothing open, want the root alone: %v", size, trackedPaths(wfs))
	}

	// Everything resolves again afterwards.
	var stat cgofuse.Stat_t
	if status := a.Getattr("/tree/d19/f049", &stat, invalidFh); status != 0 || stat.Size != 1 {
		t.Fatalf("getattr after expiry: %d size %d", status, stat.Size)
	}
}

// A reference a closed file hands back must not be returned while its
// deferred flush is pending: the flush commits metadata only while the saved
// path still maps to the inode.
func TestWinfspReferenceOutlivesPendingAsyncFlush(t *testing.T) {
	_, wfs, a := newWalkedTree(t)
	status, fh := a.Open("/tree/d01/f001", cgofuse.O_RDONLY)
	if status != 0 {
		t.Fatalf("open: %d", status)
	}
	ino, _ := wfs.inodeToPath.GetInode("/tree/d01/f001")
	done := make(chan struct{})
	wfs.pendingAsyncFlushMu.Lock()
	wfs.pendingAsyncFlush[ino] = done
	wfs.pendingAsyncFlushMu.Unlock()
	a.Release("/tree/d01/f001", fh)

	a.expireReferences(time.Now().Add(time.Hour))
	if got, found := wfs.inodeToPath.GetInode("/tree/d01/f001"); !found || got != ino {
		t.Fatalf("pending flush lost its path mapping: %d, %v", got, found)
	}

	wfs.pendingAsyncFlushMu.Lock()
	delete(wfs.pendingAsyncFlush, ino)
	wfs.pendingAsyncFlushMu.Unlock()
	close(done)
	a.expireReferences(time.Now().Add(2 * time.Hour))
	if size := inodeTableSize(wfs.inodeToPath); size != 1 {
		t.Fatalf("inode table holds %d entries after the flush finished: %v", size, trackedPaths(wfs))
	}
}

func TestWinfspHardLinkAliasResolvesAfterReferencesExpire(t *testing.T) {
	fake := newTreeFiler()
	fake.add("/", treeDir("d"))
	for _, name := range []string{"Original", "Alias"} {
		entry := treeFile(name, 0o644)
		entry.HardLinkId = []byte("hard-link-1")
		entry.HardLinkCounter = 2
		entry.Attributes.Inode = 4242
		fake.add("/d", entry)
	}
	wfs, a := newWinfspTreeWFS(t, fake)
	for round := 0; round < 2; round++ {
		for _, path := range []string{"/d/Original", "/d/Alias"} {
			var stat cgofuse.Stat_t
			if status := a.Getattr(path, &stat, invalidFh); status != 0 {
				t.Fatalf("round %d getattr %s: %d", round, path, status)
			}
			if status, got := a.Getpath(path, invalidFh); status != 0 || got != path {
				t.Fatalf("round %d Getpath(%s) = %d, %q", round, path, status, got)
			}
		}
		a.expireReferences(time.Now().Add(time.Duration(round+1) * time.Hour))
		if size := inodeTableSize(wfs.inodeToPath); size != 1 {
			t.Fatalf("round %d: inode table holds %d entries: %v", round, size, trackedPaths(wfs))
		}
	}
}

// Remote renames and deletes reach the adapter through metadata events. The
// references it holds for the old names must go with them.
func TestWinfspRemoteRenameAndDeleteReturnReferences(t *testing.T) {
	fake, wfs, a := newWalkedTree(t)
	var stat cgofuse.Stat_t
	for _, path := range []string{"/tree/d02/f010", "/tree/d02/f011"} {
		if status := a.Getattr(path, &stat, invalidFh); status != 0 {
			t.Fatalf("getattr %s: %d", path, status)
		}
	}

	renamed := treeFile("g010", 0o644)
	fake.remove("/tree/d02", "f010")
	fake.add("/tree/d02", renamed)
	fake.remove("/tree/d02", "f011")
	for _, resp := range []*filer_pb.SubscribeMetadataResponse{
		{Directory: "/tree/d02", TsNs: 1000, EventNotification: &filer_pb.EventNotification{
			OldEntry: treeFile("f010", 0o644), NewEntry: renamed, NewParentPath: "/tree/d02",
		}},
		{Directory: "/tree/d02", TsNs: 1001, EventNotification: &filer_pb.EventNotification{
			OldEntry: treeFile("f011", 0o644),
		}},
	} {
		if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), resp, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
			t.Fatal(err)
		}
	}
	wfs.metaCache.WaitForEntryInvalidations()

	for path, want := range map[string]int{"/tree/d02/f010": -cgofuse.ENOENT, "/tree/d02/f011": -cgofuse.ENOENT, "/tree/d02/g010": 0} {
		if status := a.Getattr(path, &stat, invalidFh); status != want {
			t.Fatalf("getattr %s after the remote change = %d, want %d", path, status, want)
		}
	}
	a.expireReferences(time.Now().Add(time.Hour))
	if size := inodeTableSize(wfs.inodeToPath); size != 1 {
		t.Fatalf("inode table holds %d entries after the remote changes: %v", size, trackedPaths(wfs))
	}
}
