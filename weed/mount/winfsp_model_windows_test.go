//go:build windows

package mount

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	cgofuse "github.com/winfsp/cgofuse/fuse"
	"google.golang.org/protobuf/proto"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// FuzzWinfspAdapterModel drives the adapter with sequences of local
// operations, metadata events from another mount, and cache expiry over a
// small namespace, so names collide, get replaced and gain hard links. The
// fake filer is the truth: after every step each name must stat as the filer
// has it, the inode table must be consistent, and once every handle is closed
// and the caches expire, nothing but the root may remain referenced.
func FuzzWinfspAdapterModel(f *testing.F) {
	for _, seed := range [][]byte{
		// Remote hard link, cache both names, remote delete of one.
		{modelRemoteCreate, 0, modelRemoteLink, 6, modelGetattr, 0, modelGetattr, 1, modelRemoteDelete, 0},
		// Rename over a name that has an open handle and another link.
		{modelCreate, 0, modelLink, 6, modelRemoteCreate, 2, modelRename, 2, modelRelease, 0},
		// A remote rename over a cached hard link, then expiry.
		{modelRemoteCreate, 1, modelLink, 1 + 6*2, modelGetattr, 2, modelRemoteCreate, 0, modelRemoteRename, 2, modelExpire, 2},
		// Failed creates and opens around a remote chmod.
		{modelCreate, 0, modelCreateExcl, 0, modelOpen, 3, modelRemoteUpdate, 0, modelGetattr, 0, modelRelease, 0},
		// A directory with children, removed and recreated remotely.
		{modelMkdir, 3, modelCreate, 4, modelRelease, 0, modelUnlink, 4, modelRmdir, 3, modelRemoteMkdir, 3, modelRemoteCreate, 5},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 128 {
			ops = ops[:128]
		}
		m := newWinfspModel(t)
		for i := 0; i+1 < len(ops); i += 2 {
			m.step(ops[i], ops[i+1])
			m.check()
		}
		m.finish()
	})
}

const (
	modelCreate = iota
	modelCreateExcl
	modelOpen
	modelRelease
	modelUnlink
	modelRename
	modelLink
	modelMkdir
	modelRmdir
	modelChmod
	modelGetattr
	modelReaddir
	modelCaseProbe
	modelExpire
	modelRemoteCreate
	modelRemoteUpdate
	modelRemoteDelete
	modelRemoteRename
	modelRemoteLink
	modelRemoteMkdir
	modelCount
)

// The names sit under a directory: the fake filer has no entry for the root
// itself, which a mutation's parent mtime update would wait out.
var winfspModelNames = []string{"/w/a", "/w/b", "/w/c", "/w/d", "/w/d/a", "/w/d/b"}

type winfspModel struct {
	t       *testing.T
	fake    *treeFiler
	wfs     *WFS
	a       *winfspFS
	handles []uint64
	inode   uint64
	link    int
	steps   []string
	// pendingMode is a chmod held on an open handle: the mount defers a
	// handle's metadata to its flush, so its mode leads the filer's until the
	// last handle on the inode closes. Keyed by inode.
	pendingMode map[uint64]uint32
}

func newWinfspModel(t *testing.T) *winfspModel {
	fake := newTreeFiler()
	fake.add("/", treeDir("w"))
	wfs, a := newWinfspTreeWFS(t, fake)
	return &winfspModel{t: t, fake: fake, wfs: wfs, a: a, inode: 1 << 40, pendingMode: map[uint64]uint32{}}
}

func (m *winfspModel) fail(format string, args ...any) {
	m.t.Helper()
	m.t.Fatalf("%s\nsteps:\n  %s", fmt.Sprintf(format, args...), strings.Join(m.steps, "\n  "))
}

// lookup is the filer's entry at path, nil when absent.
func (m *winfspModel) lookup(path string) *filer_pb.Entry {
	dir, name := util.FullPath(path).DirAndName()
	m.fake.mu.Lock()
	defer m.fake.mu.Unlock()
	if e := m.fake.dirs[dir][name]; e != nil {
		return proto.Clone(e).(*filer_pb.Entry)
	}
	return nil
}

func (m *winfspModel) parentIsDir(path string) bool {
	dir, _ := util.FullPath(path).DirAndName()
	e := m.lookup(dir)
	return e != nil && e.IsDirectory
}

func (m *winfspModel) isEmptyDir(path string) bool {
	m.fake.mu.Lock()
	defer m.fake.mu.Unlock()
	return len(m.fake.dirs[path]) == 0
}

// event applies a change another mount made, as the subscription delivers it.
func (m *winfspModel) event(dir string, n *filer_pb.EventNotification) {
	m.fake.mu.Lock()
	m.fake.tsNs++
	tsNs := m.fake.tsNs
	m.fake.mu.Unlock()
	n.Signatures = []int32{2}
	if err := m.wfs.metaCache.ApplyMetadataResponse(context.Background(), &filer_pb.SubscribeMetadataResponse{
		Directory: dir, TsNs: tsNs, EventNotification: n,
	}, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
		m.fail("apply event: %v", err)
	}
	m.wfs.metaCache.WaitForEntryInvalidations()
}

// setLinkCount stamps count on every name of the hard link id, as the filer's
// shared record makes every name report it.
func (m *winfspModel) setLinkCount(id []byte, count int32) {
	m.fake.mu.Lock()
	defer m.fake.mu.Unlock()
	for _, entries := range m.fake.dirs {
		for _, e := range entries {
			if string(e.HardLinkId) == string(id) {
				e.HardLinkCounter = count
			}
		}
	}
}

// created flushes a new file's deferred filer create, which the filer and so
// other mounts see only once the handle is flushed.
func (m *winfspModel) created(p string, fh uint64) {
	m.handles = append(m.handles, fh)
	if code := m.a.Flush(p, fh); code != 0 {
		m.fail("flush new %s = %d", p, code)
	}
}

func (m *winfspModel) step(op, arg byte) {
	start := time.Now()
	defer func() {
		if d := time.Since(start); d > 200*time.Millisecond {
			m.t.Logf("slow step %v: %v", m.steps[len(m.steps)-1:], d)
		}
	}()
	p := winfspModelNames[int(arg)%len(winfspModelNames)]
	q := winfspModelNames[int(arg)/len(winfspModelNames)%len(winfspModelNames)]
	src, dst := m.lookup(p), m.lookup(q)
	var desc string
	switch op % modelCount {
	case modelCreate:
		desc = "create " + p
		if code, fh := m.a.Create(p, cgofuse.O_RDWR|cgofuse.O_CREAT, 0o644); code == 0 {
			m.created(p, fh)
		}
	case modelCreateExcl:
		desc = "create excl " + p
		if code, fh := m.a.Create(p, cgofuse.O_RDWR|cgofuse.O_CREAT|cgofuse.O_EXCL, 0o644); code == 0 {
			m.created(p, fh)
		}
	case modelOpen:
		desc = "open " + p
		if code, fh := m.a.Open(p, cgofuse.O_RDONLY); code == 0 {
			if src == nil {
				m.fail("open %s succeeded, filer has no such name", p)
			}
			m.handles = append(m.handles, fh)
		}
	case modelRelease:
		if len(m.handles) == 0 {
			return
		}
		i := int(arg) % len(m.handles)
		desc = fmt.Sprintf("release handle %d", m.handles[i])
		m.a.Release("", m.handles[i])
		m.handles = append(m.handles[:i], m.handles[i+1:]...)
		m.dropFlushedModes()
	case modelUnlink:
		if src == nil || src.IsDirectory {
			return // the fake does not refuse to unlink a directory
		}
		desc = "unlink " + p
		if code := m.a.Unlink(p); code != 0 {
			m.fail("unlink %s = %d", p, code)
		}
	case modelRename:
		if src == nil || src.IsDirectory || (dst != nil && dst.IsDirectory) || p == q || !m.parentIsDir(q) {
			return // the fake moves no directory children
		}
		desc = "rename " + p + " " + q
		if code := m.a.Rename(p, q); code != 0 {
			m.fail("rename %s %s = %d", p, q, code)
		}
	case modelLink:
		if src == nil || src.IsDirectory || dst != nil || !m.parentIsDir(q) {
			return // the fake's create overwrites
		}
		desc = "link " + p + " " + q
		if code := m.a.Link(p, q); code != 0 {
			m.fail("link %s %s = %d", p, q, code)
		}
	case modelMkdir:
		desc = "mkdir " + p
		m.a.Mkdir(p, 0o755)
	case modelRmdir:
		if src == nil || !src.IsDirectory || !m.isEmptyDir(p) {
			return // the fake removes a non-empty directory
		}
		desc = "rmdir " + p
		if code := m.a.Rmdir(p); code != 0 {
			m.fail("rmdir %s = %d", p, code)
		}
	case modelChmod:
		mode := uint32(0o644)
		if arg&1 != 0 {
			mode = 0o600
		}
		desc = fmt.Sprintf("chmod %s %o", p, mode)
		if m.a.Chmod(p, mode) == 0 {
			if ino, found := m.wfs.inodeToPath.GetInode(util.FullPath(p)); found {
				if _, open := m.wfs.fhMap.FindFileHandle(ino); open {
					m.pendingMode[ino] = mode
				}
			}
		}
	case modelGetattr:
		desc = "getattr " + p
		var st cgofuse.Stat_t
		m.a.Getattr(p, &st, invalidFh)
	case modelReaddir:
		dir, _ := util.FullPath(p).DirAndName()
		desc = "readdir " + dir
		if code, fh := m.a.Opendir(dir); code == 0 {
			m.a.Readdir(dir, func(string, *cgofuse.Stat_t, int64) bool { return true }, 0, fh)
			m.a.Releasedir(dir, fh)
		}
	case modelCaseProbe:
		desc = "case probe " + p
		var lower, upper cgofuse.Stat_t
		lowerCode := m.a.Getattr(p, &lower, invalidFh)
		upperCode := m.a.Getattr(strings.ToUpper(p), &upper, invalidFh)
		if lowerCode != upperCode || lower.Ino != upper.Ino {
			m.fail("case probe %s: %d ino %d, upper %d ino %d", p, lowerCode, lower.Ino, upperCode, upper.Ino)
		}
	case modelExpire:
		ttls := time.Duration(arg % 4)
		desc = fmt.Sprintf("expire +%d ttl", ttls)
		m.a.expireReferences(time.Now().Add(ttls * winfspCacheTTL))
	case modelRemoteCreate:
		if src != nil || !m.parentIsDir(p) {
			return
		}
		m.inode++
		e := treeFile(util.FullPath(p).Name(), 0o644)
		e.Content = []byte(strings.Repeat("y", int(arg)%4))
		e.Attributes.FileSize = uint64(len(e.Content))
		e.Attributes.Inode = m.inode
		dir, _ := util.FullPath(p).DirAndName()
		desc = fmt.Sprintf("remote create %s size %d", p, len(e.Content))
		m.fake.add(dir, proto.Clone(e).(*filer_pb.Entry))
		m.event(dir, &filer_pb.EventNotification{NewEntry: e, NewParentPath: dir})
	case modelRemoteMkdir:
		if src != nil || !m.parentIsDir(p) {
			return
		}
		dir, _ := util.FullPath(p).DirAndName()
		desc = "remote mkdir " + p
		e := treeDir(util.FullPath(p).Name())
		m.fake.add(dir, proto.Clone(e).(*filer_pb.Entry))
		m.event(dir, &filer_pb.EventNotification{NewEntry: e, NewParentPath: dir})
	case modelRemoteUpdate:
		if src == nil || src.IsDirectory {
			return
		}
		dir, _ := util.FullPath(p).DirAndName()
		next := proto.Clone(src).(*filer_pb.Entry)
		next.Attributes.FileMode ^= 0o044
		next.Content = []byte(strings.Repeat("z", 1+int(arg)%5))
		next.Attributes.FileSize = uint64(len(next.Content))
		next.Chunks = nil
		desc = fmt.Sprintf("remote update %s mode %o size %d", p, next.Attributes.FileMode&0o777, len(next.Content))
		// New content from another mount supersedes what an open handle holds.
		// A metadata-only change does not: the mount defers an open file's
		// metadata to close, so its held chmod is flushed after this one.
		if ino, found := m.wfs.inodeToPath.GetInode(util.FullPath(p)); found && string(src.Content) != string(next.Content) {
			delete(m.pendingMode, ino)
		}
		m.fake.add(dir, proto.Clone(next).(*filer_pb.Entry))
		m.event(dir, &filer_pb.EventNotification{OldEntry: src, NewEntry: next, NewParentPath: dir})
	case modelRemoteDelete:
		if src == nil || (src.IsDirectory && !m.isEmptyDir(p)) {
			return
		}
		dir, name := util.FullPath(p).DirAndName()
		desc = "remote delete " + p
		m.fake.remove(dir, name)
		if len(src.HardLinkId) > 0 {
			m.setLinkCount(src.HardLinkId, src.HardLinkCounter-1)
		}
		m.event(dir, &filer_pb.EventNotification{OldEntry: src})
	case modelRemoteRename:
		if src == nil || src.IsDirectory || (dst != nil && dst.IsDirectory) || p == q || !m.parentIsDir(q) {
			return
		}
		if dst != nil && len(src.HardLinkId) > 0 && string(src.HardLinkId) == string(dst.HardLinkId) {
			return // a POSIX client sends no rename between names of one file
		}
		oldDir, oldName := util.FullPath(p).DirAndName()
		newDir, newName := util.FullPath(q).DirAndName()
		desc = "remote rename " + p + " " + q
		m.fake.remove(oldDir, oldName)
		moved := proto.Clone(src).(*filer_pb.Entry)
		moved.Name = newName
		m.fake.add(newDir, proto.Clone(moved).(*filer_pb.Entry))
		if dst != nil {
			if len(dst.HardLinkId) > 0 {
				m.setLinkCount(dst.HardLinkId, dst.HardLinkCounter-1)
			}
			m.event(newDir, &filer_pb.EventNotification{OldEntry: dst})
		}
		m.event(oldDir, &filer_pb.EventNotification{OldEntry: src, NewEntry: moved, NewParentPath: newDir})
	case modelRemoteLink:
		if src == nil || src.IsDirectory || dst != nil || !m.parentIsDir(q) {
			return
		}
		srcDir, _ := util.FullPath(p).DirAndName()
		newDir, newName := util.FullPath(q).DirAndName()
		desc = "remote link " + p + " " + q
		linked := proto.Clone(src).(*filer_pb.Entry)
		if len(linked.HardLinkId) == 0 {
			m.link++
			linked.HardLinkId = append([]byte(fmt.Sprintf("model-%015d", m.link)), filer.HARD_LINK_MARKER)
			linked.HardLinkCounter = 1
		}
		linked.HardLinkCounter++
		m.fake.add(srcDir, proto.Clone(linked).(*filer_pb.Entry))
		m.setLinkCount(linked.HardLinkId, linked.HardLinkCounter)
		m.event(srcDir, &filer_pb.EventNotification{OldEntry: src, NewEntry: linked, NewParentPath: srcDir})
		created := proto.Clone(linked).(*filer_pb.Entry)
		created.Name = newName
		m.fake.add(newDir, proto.Clone(created).(*filer_pb.Entry))
		m.event(newDir, &filer_pb.EventNotification{NewEntry: created, NewParentPath: newDir})
	}
	if desc != "" {
		m.steps = append(m.steps, desc)
	}
}

// dropFlushedModes forgets the held chmods whose inode has no handle left:
// closing the last one flushed them to the filer.
func (m *winfspModel) dropFlushedModes() {
	for ino := range m.pendingMode {
		if _, open := m.wfs.fhMap.FindFileHandle(ino); !open {
			delete(m.pendingMode, ino)
		}
	}
}

// check compares every name with the filer and the inode table with itself.
func (m *winfspModel) check() {
	m.t.Helper()
	for _, p := range winfspModelNames {
		want := m.lookup(p)
		var st cgofuse.Stat_t
		code := m.a.Getattr(p, &st, invalidFh)
		if want == nil {
			if code == 0 {
				m.fail("getattr %s = 0 (ino %d size %d), filer has no such name", p, st.Ino, st.Size)
			}
			continue
		}
		if code != 0 {
			m.fail("getattr %s = %d, filer has it", p, code)
		}
		if isDir := st.Mode&cgofuse.S_IFMT == cgofuse.S_IFDIR; isDir != want.IsDirectory {
			m.fail("getattr %s directory=%v, filer %v", p, isDir, want.IsDirectory)
		}
		if want.IsDirectory {
			continue
		}
		if size := int64(filer.FileSize(want)); st.Size != size {
			m.fail("getattr %s size %d, filer %d", p, st.Size, size)
		}
		perm := os.FileMode(want.Attributes.FileMode).Perm()
		if ino, found := m.wfs.inodeToPath.GetInode(util.FullPath(p)); found {
			if mode, pending := m.pendingMode[ino]; pending {
				perm = os.FileMode(mode).Perm()
			}
		}
		if uint32(st.Mode)&0o777 != uint32(perm) {
			m.fail("getattr %s mode %o, filer %o", p, st.Mode&0o777, perm)
		}
		nlink := uint32(1)
		if want.HardLinkCounter > 1 {
			nlink = uint32(want.HardLinkCounter)
		}
		if st.Nlink != nlink {
			m.fail("getattr %s nlink %d, filer %d", p, st.Nlink, nlink)
		}
	}
	if err := m.wfs.inodeToPath.checkConsistency(); err != nil {
		m.fail("inode table: %v", err)
	}
}

// finish closes every handle and expires every cache: only the root remains.
func (m *winfspModel) finish() {
	m.t.Helper()
	for _, fh := range m.handles {
		m.a.Release("", fh)
	}
	m.handles = nil
	m.dropFlushedModes()
	m.steps = append(m.steps, "release all, expire")
	m.a.expireReferences(time.Now().Add(time.Hour))
	m.a.expireReferences(time.Now().Add(2 * time.Hour))
	if n := inodeTableSize(m.wfs.inodeToPath); n != 1 {
		m.fail("inode table holds %d entries with nothing open: %v", n, trackedPaths(m.wfs))
	}
	if err := m.wfs.inodeToPath.checkConsistency(); err != nil {
		m.fail("inode table: %v", err)
	}
}
