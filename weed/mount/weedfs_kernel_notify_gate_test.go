package mount

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// lockModelNotifier stands in for the kernel's side of reverse invalidation:
// it records each notification together with whether the request it would
// have to wait for was still stalled at that moment.
type lockModelNotifier struct {
	mu        sync.Mutex
	entries   []entryNotification
	inodes    []inodeNotification
	stalled   func() bool
	dataInode uint64 // the file whose pages a stalled read holds locked
	violation []string
}

func (n *lockModelNotifier) EntryNotify(parent uint64, name string) fuse.Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.entries = append(n.entries, entryNotification{parent, name})
	if n.stalled() {
		n.violation = append(n.violation, "EntryNotify "+name)
	}
	return fuse.OK
}

func (n *lockModelNotifier) InodeNotify(inode uint64, offset, length int64) fuse.Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.inodes = append(n.inodes, inodeNotification{inode, offset, length})
	// A directory's listing cache is never locked across a request, so only
	// the file being read matters.
	if inode == n.dataInode && offset >= 0 && n.stalled() {
		n.violation = append(n.violation, "InodeNotify data")
	}
	return fuse.OK
}

func (n *lockModelNotifier) snapshot() ([]entryNotification, []inodeNotification, []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]entryNotification(nil), n.entries...), append([]inodeNotification(nil), n.inodes...), append([]string(nil), n.violation...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Linux holds a directory's lock across a lookup, create, rename or unlink in
// it until this mount replies, and takes the same lock, uninterruptibly, to
// expire a name in it. Expiring a name there while that request waits on a
// stalled filer parks the notifying thread in state D for as long as the
// filer stalls (the hung-task panics of appmana-008 and appmana-031), and a
// mount that exits meanwhile cannot release /dev/fuse. The expiry has to wait
// until the request is answered, and must still happen.
func TestNameExpiryWaitsForRequestHoldingTheDirectory(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	fake := &fakeFilerServer{lookupStarted: make(chan struct{}), lookupGate: make(chan struct{})}
	startFakeFiler(t, wfs, fake)
	parent := wfs.inodeToPath.Lookup(util.FullPath("/dir"), time.Now().Unix(), true, false, 0, true)

	var filerStalled atomic.Bool
	filerStalled.Store(true)
	notifier := &lockModelNotifier{stalled: filerStalled.Load}
	wfs.fuseServer = notifier

	lookupDone := make(chan fuse.Status, 1)
	go func() {
		var out fuse.EntryOut
		lookupDone <- wfs.Lookup(nil, &fuse.InHeader{NodeId: parent}, "local", &out)
	}()
	select {
	case <-fake.lookupStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("lookup never reached the filer")
	}

	invalidated := make(chan struct{})
	go func() {
		wfs.onEntryInvalidation(meta_cache.EntryInvalidation{Path: "/dir/remote", Entry: &filer_pb.Entry{Name: "remote"}, Signatures: []int32{wfs.signature + 1}})
		close(invalidated)
	}()
	select {
	case <-invalidated:
	case <-time.After(5 * time.Second):
		t.Fatal("the invalidation worker blocked behind a request in flight")
	}
	time.Sleep(50 * time.Millisecond)

	filerStalled.Store(false)
	close(fake.lookupGate)
	<-lookupDone

	eventually(t, "the deferred name expiry", func() bool {
		entries, _, _ := notifier.snapshot()
		for _, e := range entries {
			if e == (entryNotification{parent, "remote"}) {
				return true
			}
		}
		return false
	})
	if _, _, violations := notifier.snapshot(); len(violations) != 0 {
		t.Fatalf("notified while the directory's request was stalled on the filer: %v", violations)
	}
}

// Dropping a file's cached pages locks each page, and a read in flight holds
// its pages locked until this mount replies: a content invalidation must not
// be sent while a read of that file is still being served.
func TestFileDataExpiryWaitsForReadInFlight(t *testing.T) {
	wfs := newInvalidateTestWFS(t)
	inode := wfs.inodeToPath.Lookup(util.FullPath("/dir/file"), time.Now().Unix(), false, false, 0, false)
	var reading atomic.Bool
	notifier := &lockModelNotifier{stalled: reading.Load, dataInode: inode}
	wfs.fuseServer = notifier

	// A read being served: what Read marks for the length of the request.
	reading.Store(true)
	wfs.holdKernelLock(inode)
	event := updateEventFor("file", 4, 1000)
	event.EventNotification.OldEntry = &filer_pb.Entry{Name: "file", Content: []byte("old!"), Attributes: &filer_pb.FuseAttributes{FileSize: 4}}
	event.EventNotification.NewEntry.Content = []byte("new!")
	event.EventNotification.Signatures = []int32{wfs.signature + 1}
	if err := wfs.metaCache.ApplyMetadataResponse(context.Background(), event, meta_cache.SubscriberMetadataResponseApplyOptions); err != nil {
		t.Fatal(err)
	}
	wfs.metaCache.WaitForEntryInvalidations()
	wfs.waitForKernelNotifications()
	if _, _, violations := notifier.snapshot(); len(violations) != 0 {
		t.Fatalf("file data invalidated during a read of it: %v", violations)
	}
	reading.Store(false)
	wfs.releaseKernelLock(inode)
	wfs.waitForKernelNotifications()
	_, inodes, violations := notifier.snapshot()
	found := false
	for _, call := range inodes {
		found = found || call == (inodeNotification{inode, 0, 0})
	}
	if !found || len(violations) != 0 {
		t.Fatalf("content change not invalidated once the read finished: %+v %v", inodes, violations)
	}
}

// Many requests may hold one directory; the expiry goes out only after the
// last, exactly once however often it was requested meanwhile.
func TestKernelNotifyGateDefersUntilLastHolderAndDeduplicates(t *testing.T) {
	var g kernelNotifyGate
	var mu sync.Mutex
	var sent []kernelNotification
	send := func(n kernelNotification) {
		mu.Lock()
		sent = append(sent, n)
		mu.Unlock()
	}
	n := kernelNotification{kind: expireEntryName, inode: 7, name: "x"}
	g.enter(7)
	g.enter(7)
	g.submit(n, send)
	g.submit(n, send)
	g.exit(7, send)
	g.wait()
	mu.Lock()
	if len(sent) != 0 {
		mu.Unlock()
		t.Fatalf("sent while a holder remained: %v", sent)
	}
	mu.Unlock()
	g.exit(7, send)
	g.wait()
	other := kernelNotification{kind: expireEntryName, inode: 8, name: "y"}
	g.submit(other, send)
	g.wait()
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 || sent[0] != n || sent[1] != other {
		t.Fatalf("sent=%v", sent)
	}
}

// Requests and notifications race on many directories. Whatever the
// interleaving, every notification must eventually be delivered: nothing may
// stay deferred once its directory has no holder. (A holder that arrives after
// the gate decided to send is the kernel's own race, where the request is
// already queued; ordering against known holders is covered above.)
func TestKernelNotifyGateConcurrentHoldersNeverLoseNotifications(t *testing.T) {
	var g kernelNotifyGate
	var mu sync.Mutex
	delivered := map[kernelNotification]int{}
	send := func(n kernelNotification) {
		mu.Lock()
		delivered[n]++
		mu.Unlock()
	}
	submitted := map[kernelNotification]bool{}
	var submittedMu sync.Mutex
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				inode := uint64((w + i) % 16)
				n := kernelNotification{kind: expireEntryName, inode: uint64((w*7 + i) % 16), name: string(rune('a' + i%26))}
				submittedMu.Lock()
				submitted[n] = true
				submittedMu.Unlock()
				g.enter(inode)
				g.submit(n, send)
				g.exit(inode, send)
			}
		}(w)
	}
	wg.Wait()
	g.wait()
	g.mu.Lock()
	stranded, ready, pending := len(g.waiting), len(g.ready), len(g.pending)
	g.mu.Unlock()
	if stranded != 0 || ready != 0 || pending != 0 || g.deferred.Load() != 0 {
		t.Fatalf("gate not quiescent with no holders: waiting=%d ready=%d pending=%d deferred=%d", stranded, ready, pending, g.deferred.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	for n := range submitted {
		if delivered[n] == 0 {
			t.Fatalf("notification %+v never delivered", n)
		}
	}
}
