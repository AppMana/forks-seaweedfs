package mount

import (
	"sync"
	"sync/atomic"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// kernelNotifyGate keeps reverse invalidations that wait on kernel locks from
// running while this mount is serving a request whose caller holds that lock.
//
// Linux takes a directory's i_rwsem uninterruptibly before it expires a name
// in it (fuse_reverse_inval_entry), and locks each page it drops from a
// file's cache (invalidate_inode_pages2_range). A rename, create, mkdir,
// unlink or lookup holds the directory's lock while it waits for our reply; a
// read holds its pages locked. A notification issued meanwhile cannot finish
// until that reply is sent, so the notifying thread sleeps in state D for as
// long as the request takes. A filer that stops answering turns that into a
// hung task (appmana-008 and appmana-031 panicked on it), and a mount that
// exits then cannot close /dev/fuse: the stuck thread keeps it open, so the
// kernel never aborts the request and the caller and the mount wait forever.
//
// Requests mark the inode whose kernel lock their caller holds for as long as
// they are being served. Notifications for a marked inode wait until its last
// such request has been answered, then go out from a goroutine of their own:
// nothing that serves requests ever waits for a notification. A request the
// kernel has queued but this mount has not read yet is not marked; a
// notification racing it waits only until that request is answered.
type kernelNotifyGate struct {
	holders [64]kernelLockShard

	mu      sync.Mutex
	waiting map[uint64][]kernelNotification // inode -> notifications deferred until it has no holders
	ready   []kernelNotification
	pending map[kernelNotification]struct{} // waiting or ready, so repeats are not queued twice
	sending bool
	idle    sync.Cond // broadcast when ready is empty and nothing is sending

	// deferred counts waiting notifications. A request that releases the last
	// hold on an inode only takes mu when it is nonzero; it is raised before
	// the holder count is read, so a release and a deferral cannot miss each
	// other.
	deferred atomic.Int64
}

type kernelLockShard struct {
	mu    sync.Mutex
	count map[uint64]int
}

type kernelNotificationKind uint8

const (
	expireEntryName kernelNotificationKind = iota // EntryNotify(inode, name): takes the directory's i_rwsem
	expireFileData                                // InodeNotify(inode, 0, 0): locks the file's cached pages
)

type kernelNotification struct {
	kind  kernelNotificationKind
	inode uint64
	name  string
}

func (g *kernelNotifyGate) shard(inode uint64) *kernelLockShard {
	return &g.holders[inode%uint64(len(g.holders))]
}

// enter marks inode as locked by the caller of a request being served. Pair
// every enter with an exit once the reply no longer depends on this handler.
func (g *kernelNotifyGate) enter(inode uint64) {
	s := g.shard(inode)
	s.mu.Lock()
	if s.count == nil {
		s.count = make(map[uint64]int)
	}
	s.count[inode]++
	s.mu.Unlock()
}

func (g *kernelNotifyGate) exit(inode uint64, send func(kernelNotification)) {
	s := g.shard(inode)
	s.mu.Lock()
	s.count[inode]--
	idle := s.count[inode] <= 0
	if idle {
		delete(s.count, inode)
	}
	s.mu.Unlock()
	if !idle || g.deferred.Load() == 0 {
		return
	}
	g.mu.Lock()
	if g.held(inode) {
		g.mu.Unlock()
		return
	}
	if waiting := g.waiting[inode]; len(waiting) > 0 {
		delete(g.waiting, inode)
		g.deferred.Add(-int64(len(waiting)))
		g.ready = append(g.ready, waiting...)
		g.startLocked(send)
	}
	g.mu.Unlock()
}

func (g *kernelNotifyGate) held(inode uint64) bool {
	s := g.shard(inode)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count[inode] > 0
}

// submit queues n. It is sent from the gate's own goroutine as soon as no
// request being served holds n's inode.
func (g *kernelNotifyGate) submit(n kernelNotification, send func(kernelNotification)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pending == nil {
		g.pending = make(map[kernelNotification]struct{})
	}
	if _, queued := g.pending[n]; queued {
		return
	}
	g.pending[n] = struct{}{}
	if g.deferLocked(n) {
		return
	}
	g.ready = append(g.ready, n)
	g.startLocked(send)
}

// deferLocked parks n while its inode has holders and reports whether it did.
func (g *kernelNotifyGate) deferLocked(n kernelNotification) bool {
	g.deferred.Add(1)
	if !g.held(n.inode) {
		g.deferred.Add(-1)
		return false
	}
	if g.waiting == nil {
		g.waiting = make(map[uint64][]kernelNotification)
	}
	g.waiting[n.inode] = append(g.waiting[n.inode], n)
	return true
}

func (g *kernelNotifyGate) startLocked(send func(kernelNotification)) {
	if g.sending || len(g.ready) == 0 {
		return
	}
	g.sending = true
	go g.drain(send)
}

func (g *kernelNotifyGate) drain(send func(kernelNotification)) {
	for {
		g.mu.Lock()
		if len(g.ready) == 0 {
			g.sending = false
			if g.idle.L == nil {
				g.idle.L = &g.mu
			}
			g.idle.Broadcast()
			g.mu.Unlock()
			return
		}
		n := g.ready[0]
		g.ready = g.ready[1:]
		// A request may have taken the lock since n was queued.
		if g.deferLocked(n) {
			g.mu.Unlock()
			continue
		}
		delete(g.pending, n)
		g.mu.Unlock()
		send(n)
	}
}

// wait blocks until every notification that is not deferred has been sent.
func (g *kernelNotifyGate) wait() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.idle.L == nil {
		g.idle.L = &g.mu
	}
	for g.sending || len(g.ready) > 0 {
		g.idle.Wait()
	}
}

// holdKernelLock marks a request whose caller holds inode's kernel lock: the
// parent directory of a lookup or namespace change, a directory being listed
// or having its attributes read or changed, or a file being read or written.
// Use as `wfs.holdKernelLock(id); defer wfs.releaseKernelLock(id)`.
func (wfs *WFS) holdKernelLock(inode uint64) {
	wfs.kernelNotify.enter(inode)
}

func (wfs *WFS) releaseKernelLock(inode uint64) {
	wfs.kernelNotify.exit(inode, wfs.sendKernelNotification)
}

func (wfs *WFS) submitKernelNotification(n kernelNotification) {
	wfs.kernelNotify.submit(n, wfs.sendKernelNotification)
}

// waitForKernelNotifications returns once every notification not deferred
// behind a request in flight has reached the kernel.
func (wfs *WFS) waitForKernelNotifications() {
	wfs.kernelNotify.wait()
}

func (wfs *WFS) sendKernelNotification(n kernelNotification) {
	server := wfs.fuseServer
	if server == nil {
		return
	}
	var status fuse.Status
	switch n.kind {
	case expireEntryName:
		status = server.EntryNotify(n.inode, n.name)
	case expireFileData:
		status = server.InodeNotify(n.inode, 0, 0)
	}
	// ENOENT is the kernel not holding the inode or name, ENOSYS a kernel
	// without the notification; neither is worth a line.
	if status != fuse.OK && status != fuse.ENOENT && status != fuse.ENOSYS {
		glog.V(4).Infof("kernel notification %+v: %v", n, status)
	}
}
