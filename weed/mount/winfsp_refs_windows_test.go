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
	"google.golang.org/protobuf/proto"

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
	tsNs int64
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
	// The filer keeps a hard link's attributes, content and count in one
	// shared record, so every name reads what the last write through any of
	// them stored.
	if len(entry.HardLinkId) > 0 {
		for d, entries := range s.dirs {
			for name, e := range entries {
				if string(e.HardLinkId) == string(entry.HardLinkId) && (d != dir || name != entry.Name) {
					shared := proto.Clone(entry).(*filer_pb.Entry)
					shared.Name = name
					entries[name] = shared
				}
			}
		}
	}
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

// UpdateEntry stores the entry and answers with its event, as the filer does.
func (s *treeFiler) UpdateEntry(_ context.Context, req *filer_pb.UpdateEntryRequest) (*filer_pb.UpdateEntryResponse, error) {
	s.mu.Lock()
	old := s.dirs[req.Directory][req.Entry.Name]
	s.tsNs++
	tsNs := s.tsNs
	s.mu.Unlock()
	if old == nil {
		return nil, filer_pb.ErrNotFound
	}
	s.add(req.Directory, proto.Clone(req.Entry).(*filer_pb.Entry))
	return &filer_pb.UpdateEntryResponse{MetadataEvent: &filer_pb.SubscribeMetadataResponse{
		Directory: req.Directory, TsNs: tsNs,
		EventNotification: &filer_pb.EventNotification{
			OldEntry: old, NewEntry: req.Entry, NewParentPath: req.Directory, Signatures: req.Signatures,
		},
	}}, nil
}

func (s *treeFiler) CreateEntry(_ context.Context, req *filer_pb.CreateEntryRequest) (*filer_pb.CreateEntryResponse, error) {
	s.mu.Lock()
	if req.OExcl && s.dirs[req.Directory][req.Entry.Name] != nil {
		s.mu.Unlock()
		return &filer_pb.CreateEntryResponse{ErrorCode: filer_pb.FilerError_ENTRY_ALREADY_EXISTS}, nil
	}
	s.tsNs++
	tsNs := s.tsNs
	s.mu.Unlock()
	cloned := proto.Clone(req.Entry).(*filer_pb.Entry)
	s.add(req.Directory, cloned)
	return &filer_pb.CreateEntryResponse{MetadataEvent: &filer_pb.SubscribeMetadataResponse{
		Directory: req.Directory, TsNs: tsNs,
		EventNotification: &filer_pb.EventNotification{
			NewEntry: cloned, NewParentPath: req.Directory, Signatures: req.Signatures,
		},
	}}, nil
}

func (s *treeFiler) DeleteEntry(_ context.Context, req *filer_pb.DeleteEntryRequest) (*filer_pb.DeleteEntryResponse, error) {
	s.mu.Lock()
	old := s.dirs[req.Directory][req.Name]
	if old == nil {
		s.mu.Unlock()
		return nil, filer_pb.ErrNotFound
	}
	delete(s.dirs[req.Directory], req.Name)
	if len(old.HardLinkId) > 0 && old.HardLinkCounter > 1 {
		for _, entries := range s.dirs {
			for _, e := range entries {
				if string(e.HardLinkId) == string(old.HardLinkId) {
					e.HardLinkCounter = old.HardLinkCounter - 1
				}
			}
		}
	}
	s.tsNs++
	tsNs := s.tsNs
	s.mu.Unlock()
	return &filer_pb.DeleteEntryResponse{MetadataEvent: &filer_pb.SubscribeMetadataResponse{
		Directory: req.Directory, TsNs: tsNs,
		EventNotification: &filer_pb.EventNotification{
			OldEntry: old, Signatures: req.Signatures,
		},
	}}, nil
}

func (s *treeFiler) StreamRenameEntry(req *filer_pb.StreamRenameEntryRequest, stream filer_pb.SeaweedFiler_StreamRenameEntryServer) error {
	s.mu.Lock()
	old := s.dirs[req.OldDirectory][req.OldName]
	if old == nil {
		s.mu.Unlock()
		return filer_pb.ErrNotFound
	}
	var replaced *filer_pb.Entry
	if s.dirs[req.NewDirectory] != nil {
		replaced = s.dirs[req.NewDirectory][req.NewName]
	}
	delete(s.dirs[req.OldDirectory], req.OldName)
	moved := proto.Clone(old).(*filer_pb.Entry)
	moved.Name = req.NewName
	if s.dirs[req.NewDirectory] == nil {
		s.dirs[req.NewDirectory] = map[string]*filer_pb.Entry{}
	}
	s.dirs[req.NewDirectory][req.NewName] = moved
	if replaced != nil && len(replaced.HardLinkId) > 0 && replaced.HardLinkCounter > 1 {
		for _, entries := range s.dirs {
			for _, e := range entries {
				if string(e.HardLinkId) == string(replaced.HardLinkId) {
					e.HardLinkCounter = replaced.HardLinkCounter - 1
				}
			}
		}
	}
	s.tsNs++
	tsNs := s.tsNs
	s.mu.Unlock()
	if replaced != nil {
		if err := stream.Send(&filer_pb.StreamRenameEntryResponse{
			Directory: req.NewDirectory, TsNs: tsNs,
			EventNotification: &filer_pb.EventNotification{
				OldEntry: replaced, Signatures: req.Signatures,
			},
		}); err != nil {
			return err
		}
	}
	return stream.Send(&filer_pb.StreamRenameEntryResponse{
		Directory: req.OldDirectory, TsNs: tsNs + 1,
		EventNotification: &filer_pb.EventNotification{
			OldEntry: old, NewEntry: moved, NewParentPath: req.NewDirectory, Signatures: req.Signatures,
		},
	})
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
		// As a mount does by default: idle directory listings go after a while.
		dirIdleEvict: defaultDirIdleEvict,
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

// Concurrent walkers, closes and sweeps must balance every reference.
func TestWinfspConcurrentWalksReturnEveryReference(t *testing.T) {
	_, wfs, a := newWalkedTree(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				a.expireReferences(time.Now().Add(2 * winfspCacheTTL))
				time.Sleep(time.Millisecond)
			}
		}
	}()
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				path := fmt.Sprintf("/tree/d%02d/f%03d", (worker+i)%20, (worker*7+i)%50)
				var stat cgofuse.Stat_t
				if status := a.Getattr(path, &stat, invalidFh); status != 0 {
					t.Errorf("getattr %s: %d", path, status)
					return
				}
				status, fh := a.Open(path, cgofuse.O_RDONLY)
				if status != 0 {
					t.Errorf("open %s: %d", path, status)
					return
				}
				if status, got := a.Getpath(strings.ToUpper(path), invalidFh); status != 0 || got != path {
					t.Errorf("Getpath(%s) = %d, %q", strings.ToUpper(path), status, got)
				}
				a.Release(path, fh)
				if i%50 == 0 {
					dir := fmt.Sprintf("/tree/d%02d", (worker+i)%20)
					status, dh := a.Opendir(dir)
					if status != 0 {
						t.Errorf("opendir %s: %d", dir, status)
						return
					}
					a.Readdir(dir, func(string, *cgofuse.Stat_t, int64) bool { return true }, 0, dh)
					a.Releasedir(dir, dh)
				}
			}
		}(worker)
	}
	wg.Wait()
	close(stop)
	a.expireReferences(time.Now().Add(time.Hour))
	if size := inodeTableSize(wfs.inodeToPath); size != 1 {
		t.Fatalf("inode table holds %d entries after concurrent walks: %v", size, trackedPaths(wfs))
	}
}

func TestWinfspIdleMountExpiresReferences(t *testing.T) {
	fake := newTreeFiler()
	fake.add("/", treeFile("idle", 0o644))
	wfs, a := newWinfspTreeWFS(t, fake)
	a.Init()
	defer a.Destroy()
	var stat cgofuse.Stat_t
	if code := a.Getattr("/idle", &stat, invalidFh); code != 0 {
		t.Fatal(code)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if inodeTableSize(wfs.inodeToPath) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("idle mount retained an expired file reference without further traffic")
}

func TestWinfspRemoteDeleteOfHardLinkKeepsOtherName(t *testing.T) {
	fake := newTreeFiler()
	var original *filer_pb.Entry
	for _, name := range []string{"original", "alias"} {
		entry := treeFile(name, 0o644)
		entry.HardLinkId = []byte("shared")
		entry.HardLinkCounter = 2
		entry.Attributes.Inode = 4242
		fake.add("/", entry)
		if name == "original" {
			original = entry
		}
	}
	wfs, a := newWinfspTreeWFS(t, fake)
	var stat cgofuse.Stat_t
	for _, name := range []string{"/original", "/alias"} {
		if code := a.Getattr(name, &stat, invalidFh); code != 0 {
			t.Fatal(code)
		}
	}
	fake.remove("/", "original")
	if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), &filer_pb.SubscribeMetadataResponse{
		Directory: "/", TsNs: 1000, EventNotification: &filer_pb.EventNotification{OldEntry: original},
	}, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	wfs.metaCache.WaitForEntryInvalidations()
	if code := a.Getattr("/alias", &stat, invalidFh); code != 0 {
		t.Fatalf("surviving hard link getattr = %d", code)
	}
	if code := a.Getattr("/original", &stat, invalidFh); code != -cgofuse.ENOENT {
		t.Fatalf("deleted hard link getattr = %d", code)
	}
}

func TestWinfspOpenReferenceSurvivesRemoteRenameAndDelete(t *testing.T) {
	for _, rename := range []bool{false, true} {
		t.Run(fmt.Sprintf("rename=%v", rename), func(t *testing.T) {
			fake := newTreeFiler()
			old := treeFile("old", 0o644)
			fake.add("/", old)
			wfs, a := newWinfspTreeWFS(t, fake)
			status, fh := a.Open("/old", cgofuse.O_RDONLY)
			if status != 0 {
				t.Fatal(status)
			}
			ino, _ := wfs.inodeToPath.GetInode("/old")
			event := &filer_pb.EventNotification{OldEntry: old}
			fake.remove("/", "old")
			if rename {
				next := proto.Clone(old).(*filer_pb.Entry)
				next.Name = "new"
				fake.add("/", next)
				event.NewEntry, event.NewParentPath = next, "/"
			}
			if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), &filer_pb.SubscribeMetadataResponse{Directory: "/", TsNs: 1000, EventNotification: event}, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
				t.Fatal(err)
			}
			wfs.metaCache.WaitForEntryInvalidations()
			a.expireReferences(time.Now().Add(time.Hour))
			if !wfs.inodeToPath.HasInode(ino) {
				t.Fatal("open handle lost inode")
			}
			if rename {
				if path, status := wfs.inodeToPath.GetPath(ino); status != 0 || path != "/new" {
					t.Fatalf("renamed inode = %q, %v", path, status)
				}
			}
			buf := make([]byte, 1)
			if n := a.Read("/old", buf, 0, fh); n != 1 || string(buf) != "x" {
				t.Fatalf("open read = %d %q", n, buf)
			}
			a.Release("/old", fh)
			a.expireReferences(time.Now().Add(2 * time.Hour))
			if n := inodeTableSize(wfs.inodeToPath); n != 1 {
				t.Fatalf("closed handles left %d inodes", n)
			}
		})
	}
}

func TestWinfspDelayedDeleteKeepsReplacementPath(t *testing.T) {
	fake := newTreeFiler()
	replacement := treeFile("name", 0o644)
	replacement.Attributes.Inode = 84
	fake.add("/", replacement)
	wfs, a := newWinfspTreeWFS(t, fake)
	status, fh := a.Open("/name", cgofuse.O_RDONLY)
	if status != 0 {
		t.Fatal(status)
	}
	defer a.Release("/name", fh)
	a.entryChanged(meta_cache.EntryInvalidation{Path: "/name", Deleted: true, DeletedInode: 42})
	if path, status := wfs.inodeToPath.GetPath(84); status != 0 || path != "/name" {
		t.Fatalf("delayed delete detached replacement: path=%q status=%v", path, status)
	}
}

func TestWinfspFailedCreateReturnsReference(t *testing.T) {
	fake := newTreeFiler()
	fake.add("/", treeDir("d"))
	ro := treeFile("readonly", 0o444)
	ro.Attributes.Uid = 1
	ro.Attributes.Gid = 1
	fake.add("/d", ro)
	wfs, a := newWinfspTreeWFS(t, fake)

	if status, fh := a.Create("/d/readonly", cgofuse.O_RDWR, 0o644); status != -cgofuse.EACCES || fh != invalidFh {
		t.Fatalf("Create on read-only file = (%d, %d), want (%d, %d)", status, fh, -cgofuse.EACCES, invalidFh)
	}
	a.expireReferences(time.Now().Add(time.Hour))
	if size := inodeTableSize(wfs.inodeToPath); size != 1 {
		t.Fatalf("inode table holds %d entries after failed Create, want 1: %v", size, trackedPaths(wfs))
	}
}

func TestWinfspLocalRenameAndUnlinkMaintainReferencesAndPaths(t *testing.T) {
	fake := newTreeFiler()
	fake.add("/", treeDir("d"))
	src := treeFile("src", 0o644)
	src.Content = []byte("new")
	src.Attributes.FileSize = 3
	dst := treeFile("dst", 0o644)
	dst.Content = []byte("x")
	dst.Attributes.FileSize = 1
	fake.add("/d", src)
	fake.add("/d", dst)
	wfs, a := newWinfspTreeWFS(t, fake)
	wfs.option.MountUid, wfs.option.MountGid = 0, 0

	// Open /d/dst so its inode stays in inodeToPath, while /d/src has no
	// cached reference when Rename runs.
	status, fhDst := a.Open("/d/dst", cgofuse.O_RDONLY)
	if status != 0 {
		t.Fatalf("open /d/dst: %d", status)
	}
	a.expireReferences(time.Now().Add(time.Hour))

	if code := a.Rename("/d/src", "/d/dst"); code != 0 {
		t.Fatalf("rename /d/src -> /d/dst: %d", code)
	}
	var stat cgofuse.Stat_t
	if code := a.Getattr("/d/dst", &stat, invalidFh); code != 0 || stat.Size != 3 {
		t.Fatalf("getattr /d/dst after rename = %d size %d, want 0 size 3", code, stat.Size)
	}
	a.Release("/d/dst", fhDst)

	if code := a.Link("/d/dst", "/d/linked"); code != 0 {
		t.Fatalf("link /d/dst -> /d/linked: %d", code)
	}
	if !wfs.inodeToPath.HasPath("/d/linked") {
		t.Fatal("Link did not keep /d/linked in inodeToPath")
	}

	csAdapter := newWinfspFS(wfs, true)
	if code := csAdapter.Unlink("/d/nonexistent"); code != -cgofuse.ENOENT {
		t.Fatalf("case-sensitive Unlink of missing file = %d, want %d", code, -cgofuse.ENOENT)
	}

	a.expireReferences(time.Now().Add(2 * time.Hour))
	csAdapter.expireReferences(time.Now().Add(2 * time.Hour))
	if size := inodeTableSize(wfs.inodeToPath); size != 1 {
		t.Fatalf("inode table holds %d entries after expiry, want 1: %v", size, trackedPaths(wfs))
	}
}

// Renaming one name of a file onto another name of the same file does
// nothing (POSIX rename). The Linux kernel answers it itself; WinFsp passes it
// through, and the filer would drop one name and keep the old link count.
func TestWinfspRenameBetweenNamesOfOneFileKeepsBoth(t *testing.T) {
	fake := newTreeFiler()
	fake.add("/", treeDir("d"))
	fake.add("/d", treeFile("a", 0o644))
	_, a := newWinfspTreeWFS(t, fake)
	if code := a.Link("/d/a", "/d/c"); code != 0 {
		t.Fatalf("link = %d", code)
	}
	if code := a.Rename("/d/a", "/d/c"); code != 0 {
		t.Fatalf("rename = %d", code)
	}
	for _, path := range []string{"/d/a", "/d/c"} {
		var st cgofuse.Stat_t
		if code := a.Getattr(path, &st, invalidFh); code != 0 || st.Nlink != 2 {
			t.Fatalf("getattr %s = %d nlink %d, want 0 nlink 2", path, code, st.Nlink)
		}
	}
}
