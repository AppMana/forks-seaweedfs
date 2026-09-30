package leveldb

// Shared chunk lists: an S3 CopyObject of a whole object may give the
// destination the source's chunk fids instead of copying the bytes. Every entry
// that shares a chunk list carries Extended[sharedChunksKey] = "<group>/<nonce>"
// and owns a reference entry at <sharedChunksRefDir>/<group>/<nonce>. The filer
// may delete a shared chunk list only when the entry going away was the last
// reference of its group. These tests drive a real filer over a real store and
// read what the filer queued for deletion.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/seaweedfs/seaweedfs/weed/cluster"
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"github.com/seaweedfs/seaweedfs/weed/util/log_buffer"
	"github.com/seaweedfs/seaweedfs/weed/wdclient"
)

const (
	sharedChunksKey    = "Seaweed-Shared-Chunks"
	sharedChunksRefDir = "/etc/seaweedfs/shared_chunks"
	sharedOwnerKey     = "Seaweed-Shared-Chunks-Owner"
)

var errInjected = errors.New("injected store failure")

// faultStore fails the store operation that matches fail, once armed. Every
// mutation is counted so a test can fail each one in turn.
type faultStore struct {
	filer.FilerStore
	mu        sync.Mutex
	mutations int
	failAt    int // 1-based mutation index to fail; 0 disables
	reads     atomic.Int64
	kvReads   atomic.Int64
}

func (s *faultStore) mutation() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mutations++
	if s.failAt != 0 && s.mutations == s.failAt {
		return errInjected
	}
	return nil
}

func (s *faultStore) arm(n int) {
	s.mu.Lock()
	s.mutations = 0
	s.failAt = n
	s.mu.Unlock()
}

func (s *faultStore) disarm() { s.arm(0) }

func (s *faultStore) InsertEntry(ctx context.Context, e *filer.Entry) error {
	if err := s.mutation(); err != nil {
		return err
	}
	return s.FilerStore.InsertEntry(ctx, e)
}

func (s *faultStore) UpdateEntry(ctx context.Context, e *filer.Entry) error {
	if err := s.mutation(); err != nil {
		return err
	}
	return s.FilerStore.UpdateEntry(ctx, e)
}

func (s *faultStore) DeleteEntry(ctx context.Context, p util.FullPath) error {
	if err := s.mutation(); err != nil {
		return err
	}
	return s.FilerStore.DeleteEntry(ctx, p)
}

func (s *faultStore) FindEntry(ctx context.Context, p util.FullPath) (*filer.Entry, error) {
	s.reads.Add(1)
	return s.FilerStore.FindEntry(ctx, p)
}

func (s *faultStore) KvGet(ctx context.Context, key []byte) ([]byte, error) {
	s.kvReads.Add(1)
	return s.FilerStore.KvGet(ctx, key)
}

func newSharedStore(t testing.TB) (*faultStore, *LevelDB2Store) {
	t.Helper()
	store := &LevelDB2Store{}
	if err := store.initialize(t.TempDir(), 2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Shutdown)
	return &faultStore{FilerStore: store}, store
}

// newSharedFiler builds a filer without its deletion loop, so everything it
// decides to delete stays in FileIdDeletionQueue for the test to read. Two
// filers built over the same store behave like two filer replicas sharing one
// store.
func newSharedFiler(t testing.TB, store filer.FilerStore) *filer.Filer {
	t.Helper()
	dialOption := grpc.WithTransportCredentials(insecure.NewCredentials())
	masterClient := wdclient.NewMasterClient(dialOption, "test", cluster.FilerType,
		pb.ServerAddress("localhost:0"), "", "", *pb.NewServiceDiscoveryFromMap(map[string]pb.ServerAddress{}))
	logBuffer := log_buffer.NewLogBuffer("test", time.Minute,
		func(*log_buffer.LogBuffer, time.Time, time.Time, []byte, int64, int64) {}, nil, func() {})
	t.Cleanup(logBuffer.ShutdownLogBuffer)
	return &filer.Filer{
		Store:               filer.NewFilerStoreWrapper(store),
		MasterClient:        masterClient,
		FilerConf:           filer.NewFilerConf(),
		RemoteStorage:       filer.NewFilerRemoteStorage(),
		MaxFilenameLength:   255,
		DirBucketsPath:      "/buckets",
		LocalMetaLogBuffer:  logBuffer,
		FileIdDeletionQueue: util.NewUnboundedQueue(),
	}
}

func testCtx() context.Context { return filer.WithSuppressedMetadataEvents(context.Background()) }

var fidSeq atomic.Int64

// newChunks returns n distinct plain chunks, 4 MiB each.
func newChunks(n int) []*filer_pb.FileChunk {
	chunks := make([]*filer_pb.FileChunk, n)
	for i := range chunks {
		id := fidSeq.Add(1)
		chunks[i] = &filer_pb.FileChunk{
			FileId:       fmt.Sprintf("%d,%x0000beef", 1+id%7, id),
			Offset:       int64(i) * 4 << 20,
			Size:         4 << 20,
			ModifiedTsNs: time.Now().UnixNano(),
		}
	}
	return chunks
}

func fids(chunks []*filer_pb.FileChunk) []string {
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, c.GetFileIdString())
	}
	sort.Strings(out)
	return out
}

func cloneChunks(chunks []*filer_pb.FileChunk) []*filer_pb.FileChunk {
	out := make([]*filer_pb.FileChunk, len(chunks))
	for i, c := range chunks {
		out[i] = proto.Clone(c).(*filer_pb.FileChunk)
	}
	return out
}

func putObject(t testing.TB, f *filer.Filer, path string, chunks []*filer_pb.FileChunk, ext map[string][]byte) {
	t.Helper()
	if err := createObject(f, path, chunks, ext); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
}

// pathLocks models how writes reach the filers in production: every write to
// one object path (a PUT, a DELETE, the copy's conditional marking of its
// source) is routed to that path's owner filer and runs under its per-path
// lock, whichever filer the client talked to. Writes to different paths, and
// everything the filers do in the store on their behalf, still race freely
// across the two filer instances.
var pathLocks sync.Map

func lockPath(p string) func() {
	m, _ := pathLocks.LoadOrStore(p, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func deleteObjectErr(f *filer.Filer, path string) error {
	defer lockPath(path)()
	return f.DeleteEntryMetaAndData(testCtx(), util.FullPath(path), false, false, true, false, nil, 0)
}

func createObject(f *filer.Filer, path string, chunks []*filer_pb.FileChunk, ext map[string][]byte) error {
	defer lockPath(path)()
	var size uint64
	for _, c := range chunks {
		size += c.Size
	}
	now := time.Now()
	entry := &filer.Entry{
		FullPath: util.FullPath(path),
		Attr:     filer.Attr{Mtime: now, Crtime: now, Mode: 0644, FileSize: size},
		Chunks:   chunks,
		Extended: map[string][]byte{},
	}
	for k, v := range ext {
		entry.Extended[k] = append([]byte(nil), v...)
	}
	return f.CreateEntry(testCtx(), entry, nil, false, false, nil, false, 255)
}

func deleteObject(t testing.TB, f *filer.Filer, path string) {
	t.Helper()
	if err := deleteObjectErr(f, path); err != nil {
		t.Fatalf("delete %s: %v", path, err)
	}
}

// drainDeleted returns every fid the filer queued for deletion since the last
// call, with multiplicity.
func drainDeleted(f *filer.Filer) []string {
	var out []string
	f.FileIdDeletionQueue.Consume(func(ids []string) { out = append(out, ids...) })
	f.FileIdDeletionQueue.Consume(func(ids []string) { out = append(out, ids...) })
	sort.Strings(out)
	return out
}

func sharedRef(entry *filer.Entry) (group, nonce string, ok bool) {
	if entry == nil || entry.Extended == nil {
		return "", "", false
	}
	v, found := entry.Extended[sharedChunksKey]
	if !found {
		return "", "", false
	}
	parts := strings.SplitN(string(v), "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

var nonceSeq atomic.Int64

func newGroup() string { return fmt.Sprintf("%032x", time.Now().UnixNano()+nonceSeq.Add(1)) }
func newNonce() string { return fmt.Sprintf("%016x", nonceSeq.Add(1)) }

func refPath(group, nonce string) util.FullPath {
	return util.FullPath(sharedChunksRefDir + "/" + group + "/" + nonce)
}

// linkStep names the steps of the copy protocol the S3 gateway runs, in order.
// The filer does the rest inside the destination's create: it writes the
// destination's reference, re-checks the source, then stores the destination.
const (
	stepMarkSource  = 1 // mark the source a member of a group (writes its reference)
	stepCreateDest  = 2 // create the destination with the source's chunks and a link hint
	stepsInProtocol = 2
)

const linkSourceKey = "Seaweed-Shared-Chunks-Link-Source"

// link runs the copy protocol from src to dst through filer f, stopping after
// step stopAfter (0 runs it all). It returns whether dst now shares src's
// chunks; a source that changed underneath makes the create fail, which is not
// an error of the test.
func link(f *filer.Filer, src, dst string, stopAfter int) (bool, error) {
	ctx := testCtx()
	srcEntry, err := f.FindEntry(ctx, util.FullPath(src))
	if err != nil {
		return false, err
	}
	if _, _, ok := sharedRef(srcEntry); !ok {
		marked := srcEntry.ShallowClone()
		marked.Extended = map[string][]byte{}
		for k, v := range srcEntry.Extended {
			marked.Extended[k] = v
		}
		marked.Extended[sharedChunksKey] = []byte(filer.SharedChunksGroupOf(srcEntry.GetChunks()) + "/" + newNonce())
		// the gateway's mark is conditional on the chunks it read, checked under
		// the source's path lock
		unlock := lockPath(src)
		current, findErr := f.FindEntry(ctx, util.FullPath(src))
		if findErr != nil || !equalStrings(fids(current.GetChunks()), fids(srcEntry.GetChunks())) {
			unlock()
			return false, nil
		}
		if _, _, already := sharedRef(current); !already {
			marked.Extended = map[string][]byte{}
			for k, v := range current.Extended {
				marked.Extended[k] = v
			}
			marked.Extended[sharedChunksKey] = []byte(filer.SharedChunksGroupOf(current.GetChunks()) + "/" + newNonce())
			if err := f.UpdateEntry(ctx, current, marked); err != nil {
				unlock()
				return false, err
			}
		}
		unlock()
	}
	if stopAfter == stepMarkSource {
		return false, nil
	}
	// a concurrent copy may have marked the source first: link to the marker
	// it actually carries
	srcEntry, err = f.FindEntry(ctx, util.FullPath(src))
	if err != nil {
		return false, nil
	}
	group, srcNonce, ok := sharedRef(srcEntry)
	if !ok {
		return false, nil
	}

	err = createObject(f, dst, cloneChunks(srcEntry.GetChunks()), map[string][]byte{
		sharedChunksKey: []byte(group + "/" + newNonce()),
		linkSourceKey:   []byte(src + "\n" + group + "/" + srcNonce),
	})
	if err != nil {
		if strings.Contains(err.Error(), "link source changed") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func mustLink(t testing.TB, f *filer.Filer, src, dst string) {
	t.Helper()
	linked, err := link(f, src, dst, 0)
	if err != nil || !linked {
		t.Fatalf("link %s -> %s: linked=%v err=%v", src, dst, linked, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustFind(t testing.TB, f *filer.Filer, path string) *filer.Entry {
	t.Helper()
	e, err := f.FindEntry(testCtx(), util.FullPath(path))
	if err != nil {
		t.Fatalf("find %s: %v", path, err)
	}
	return e
}

func assertNoneDeleted(t testing.TB, deleted, live []string, what string) {
	t.Helper()
	set := map[string]bool{}
	for _, d := range deleted {
		set[d] = true
	}
	for _, l := range live {
		if set[l] {
			t.Fatalf("%s: chunk %s was queued for deletion while a live entry still references it (deleted=%v)", what, l, deleted)
		}
	}
}

func countOf(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}

func assertDeletedOnce(t testing.TB, deleted, want []string, what string) {
	t.Helper()
	for _, w := range want {
		if c := countOf(deleted, w); c != 1 {
			t.Fatalf("%s: chunk %s queued %d times, want exactly once (deleted=%v)", what, w, c, deleted)
		}
	}
}

func refsIn(t testing.TB, f *filer.Filer, group string) []string {
	t.Helper()
	var names []string
	_, err := f.Store.ListDirectoryEntries(testCtx(), util.FullPath(sharedChunksRefDir+"/"+group), "", false, 1000, func(e *filer.Entry) (bool, error) {
		names = append(names, e.Name())
		return true, nil
	})
	if err != nil {
		t.Fatalf("list refs of %s: %v", group, err)
	}
	return names
}

// --- behavior -----------------------------------------------------------------

func TestSharedChunksDeletingSourceKeepsDestination(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	chunks := newChunks(3)
	putObject(t, f, "/buckets/bkt/_uploads/u1/data", chunks, nil)
	mustLink(t, f, "/buckets/bkt/_uploads/u1/data", "/buckets/bkt/blobs/sha256/aa")

	dst := mustFind(t, f, "/buckets/bkt/blobs/sha256/aa")
	if !equalStrings(fids(dst.GetChunks()), fids(chunks)) {
		t.Fatalf("destination does not share the source's chunks")
	}

	deleteObject(t, f, "/buckets/bkt/_uploads/u1/data")
	assertNoneDeleted(t, drainDeleted(f), fids(chunks), "delete source")
	mustFind(t, f, "/buckets/bkt/blobs/sha256/aa")

	deleteObject(t, f, "/buckets/bkt/blobs/sha256/aa")
	assertDeletedOnce(t, drainDeleted(f), fids(chunks), "delete last link")
}

func TestSharedChunksDeletingDestinationKeepsSource(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	chunks := newChunks(2)
	putObject(t, f, "/buckets/bkt/src", chunks, nil)
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/dst")

	deleteObject(t, f, "/buckets/bkt/dst")
	assertNoneDeleted(t, drainDeleted(f), fids(chunks), "delete destination")
	src := mustFind(t, f, "/buckets/bkt/src")
	if !equalStrings(fids(src.GetChunks()), fids(chunks)) {
		t.Fatalf("source chunks changed")
	}

	deleteObject(t, f, "/buckets/bkt/src")
	assertDeletedOnce(t, drainDeleted(f), fids(chunks), "delete last link")
}

func TestSharedChunksThreeLinksEveryDeleteOrder(t *testing.T) {
	orders := [][]string{
		{"a", "b", "c"}, {"a", "c", "b"}, {"b", "a", "c"},
		{"b", "c", "a"}, {"c", "a", "b"}, {"c", "b", "a"},
	}
	for _, order := range orders {
		t.Run(strings.Join(order, ""), func(t *testing.T) {
			fs, _ := newSharedStore(t)
			f := newSharedFiler(t, fs)
			chunks := newChunks(2)
			putObject(t, f, "/buckets/bkt/a", chunks, nil)
			mustLink(t, f, "/buckets/bkt/a", "/buckets/bkt/b")
			mustLink(t, f, "/buckets/bkt/b", "/buckets/bkt/c") // copy of a copy
			for i, name := range order {
				deleteObject(t, f, "/buckets/bkt/"+name)
				deleted := drainDeleted(f)
				if i < len(order)-1 {
					assertNoneDeleted(t, deleted, fids(chunks), "delete "+name)
					for _, rest := range order[i+1:] {
						mustFind(t, f, "/buckets/bkt/"+rest)
					}
				} else {
					assertDeletedOnce(t, deleted, fids(chunks), "delete last link "+name)
				}
			}
		})
	}
}

func TestSharedChunksOverwritingSourceKeepsDestination(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	oldChunks := newChunks(2)
	putObject(t, f, "/buckets/bkt/src", oldChunks, nil)
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/dst")

	newSrc := newChunks(1)
	putObject(t, f, "/buckets/bkt/src", newSrc, nil) // a later PUT to the source key
	assertNoneDeleted(t, drainDeleted(f), fids(oldChunks), "overwrite source")
	if dst := mustFind(t, f, "/buckets/bkt/dst"); !equalStrings(fids(dst.GetChunks()), fids(oldChunks)) {
		t.Fatalf("destination changed after the source was overwritten")
	}

	deleteObject(t, f, "/buckets/bkt/dst")
	assertDeletedOnce(t, drainDeleted(f), fids(oldChunks), "delete last holder of the old chunks")

	deleteObject(t, f, "/buckets/bkt/src")
	assertDeletedOnce(t, drainDeleted(f), fids(newSrc), "delete overwritten source")
}

func TestSharedChunksOverwritingDestinationKeepsSource(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	chunks := newChunks(2)
	putObject(t, f, "/buckets/bkt/src", chunks, nil)
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/dst")

	putObject(t, f, "/buckets/bkt/dst", newChunks(1), nil)
	assertNoneDeleted(t, drainDeleted(f), fids(chunks), "overwrite destination")
	mustFind(t, f, "/buckets/bkt/src")

	deleteObject(t, f, "/buckets/bkt/src")
	assertDeletedOnce(t, drainDeleted(f), fids(chunks), "delete last link")
}

func TestSharedChunksPerObjectMetadataStaysSeparate(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	chunks := newChunks(2)
	putObject(t, f, "/buckets/bkt/src", chunks, map[string][]byte{
		"X-Amz-Meta-Color": []byte("red"),
		"Content-Type":     []byte("application/octet-stream"),
	})
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/dst")

	dst := mustFind(t, f, "/buckets/bkt/dst")
	updated := dst.ShallowClone()
	updated.Extended = map[string][]byte{}
	for k, v := range dst.Extended {
		updated.Extended[k] = v
	}
	updated.Extended["X-Amz-Meta-Color"] = []byte("blue")
	updated.Extended["X-Amz-Tagging-Env"] = []byte("prod")
	if err := f.UpdateEntry(testCtx(), dst, updated); err != nil {
		t.Fatal(err)
	}
	assertNoneDeleted(t, drainDeleted(f), fids(chunks), "metadata-only update")

	src := mustFind(t, f, "/buckets/bkt/src")
	if got := string(src.Extended["X-Amz-Meta-Color"]); got != "red" {
		t.Fatalf("source metadata changed to %q by an update of the destination", got)
	}
	if _, leaked := src.Extended["X-Amz-Tagging-Env"]; leaked {
		t.Fatalf("destination tag leaked into the source")
	}
	if got := string(mustFind(t, f, "/buckets/bkt/dst").Extended["X-Amz-Meta-Color"]); got != "blue" {
		t.Fatalf("destination metadata = %q, want blue", got)
	}
}

func TestSharedChunksRenameOfALinkKeepsBothReadable(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	chunks := newChunks(2)
	putObject(t, f, "/buckets/bkt/src", chunks, nil)
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/dst")

	// A rename writes the entry at its new path, then drops the old path
	// without deleting data (what the filer's move does).
	dst := mustFind(t, f, "/buckets/bkt/dst")
	moved := dst.ShallowClone()
	moved.FullPath = "/buckets/bkt/renamed"
	if err := f.CreateEntry(testCtx(), moved, nil, false, false, nil, false, 255); err != nil {
		t.Fatal(err)
	}
	if err := f.Store.DeleteOneEntry(context.WithValue(testCtx(), "OP", "MV"), dst); err != nil {
		t.Fatal(err)
	}
	assertNoneDeleted(t, drainDeleted(f), fids(chunks), "rename")

	deleteObject(t, f, "/buckets/bkt/src")
	assertNoneDeleted(t, drainDeleted(f), fids(chunks), "delete source after rename")
	mustFind(t, f, "/buckets/bkt/renamed")

	deleteObject(t, f, "/buckets/bkt/renamed")
	assertDeletedOnce(t, drainDeleted(f), fids(chunks), "delete last link")
}

// A crash in the middle of a rename leaves the entry at both paths. Each must
// then hold its own reference, or deleting one of them frees the other's data.
func TestSharedChunksEntryDuplicatedAtTwoPathsHoldsTwoReferences(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	chunks := newChunks(1)
	putObject(t, f, "/buckets/bkt/src", chunks, nil)
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/dst")

	dst := mustFind(t, f, "/buckets/bkt/dst")
	dup := dst.ShallowClone()
	dup.FullPath = "/buckets/bkt/half-renamed"
	if err := f.CreateEntry(testCtx(), dup, nil, false, false, nil, false, 255); err != nil {
		t.Fatal(err)
	}

	deleteObject(t, f, "/buckets/bkt/src")
	deleteObject(t, f, "/buckets/bkt/dst")
	assertNoneDeleted(t, drainDeleted(f), fids(chunks), "delete the other two links")
	mustFind(t, f, "/buckets/bkt/half-renamed")

	deleteObject(t, f, "/buckets/bkt/half-renamed")
	assertDeletedOnce(t, drainDeleted(f), fids(chunks), "delete last link")
}

func TestSharedChunksRecursiveDeleteOfAFolderHoldingOneLink(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	chunks := newChunks(2)
	plain := newChunks(1)
	putObject(t, f, "/buckets/bkt/uploads/u1/data", chunks, nil)
	putObject(t, f, "/buckets/bkt/uploads/u1/startedat", plain, nil)
	mustLink(t, f, "/buckets/bkt/uploads/u1/data", "/buckets/bkt/blobs/x")

	if err := f.DeleteEntryMetaAndData(testCtx(), "/buckets/bkt/uploads", true, false, true, false, nil, 0); err != nil {
		t.Fatal(err)
	}
	deleted := drainDeleted(f)
	assertNoneDeleted(t, deleted, fids(chunks), "recursive delete of the folder holding the source")
	assertDeletedOnce(t, deleted, fids(plain), "recursive delete of an unshared file")
	mustFind(t, f, "/buckets/bkt/blobs/x")

	if err := f.DeleteEntryMetaAndData(testCtx(), "/buckets/bkt/blobs", true, false, true, false, nil, 0); err != nil {
		t.Fatal(err)
	}
	assertDeletedOnce(t, drainDeleted(f), fids(chunks), "recursive delete of the last link")
}

func TestSharedChunksReferencesMatchLiveLinks(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	putObject(t, f, "/buckets/bkt/src", newChunks(1), nil)
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/d1")
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/d2")
	group, _, _ := sharedRef(mustFind(t, f, "/buckets/bkt/src"))
	if got := len(refsIn(t, f, group)); got != 3 {
		t.Fatalf("refs = %d, want 3", got)
	}
	deleteObject(t, f, "/buckets/bkt/d1")
	if got := len(refsIn(t, f, group)); got != 2 {
		t.Fatalf("refs after one delete = %d, want 2", got)
	}
	putObject(t, f, "/buckets/bkt/d2", newChunks(1), nil) // overwrite leaves the group
	if got := len(refsIn(t, f, group)); got != 1 {
		t.Fatalf("refs after overwrite = %d, want 1", got)
	}
	deleteObject(t, f, "/buckets/bkt/src")
	if got := len(refsIn(t, f, group)); got != 0 {
		t.Fatalf("refs after deleting everything = %d, want 0", got)
	}
}

// --- interruption ---------------------------------------------------------------

// liveFids returns the fids of every object entry under /buckets that the store
// still holds.
func liveObjects(t testing.TB, f *filer.Filer, paths []string) map[string][]string {
	out := map[string][]string{}
	for _, p := range paths {
		e, err := f.FindEntry(testCtx(), util.FullPath(p))
		if err == nil && e != nil {
			out[p] = fids(e.GetChunks())
		}
	}
	return out
}

// Every step of the copy protocol can be the last one to run (a crash) or fail
// (a store error). Whatever the prefix, no chunk a live entry references may be
// freed, then or after the remaining entries are deleted in either order.
func TestSharedChunksCopyInterruptedAtEveryStep(t *testing.T) {
	for stop := 1; stop <= stepsInProtocol; stop++ {
		for _, deleteSourceFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("stop-after-%d/source-first-%v", stop, deleteSourceFirst), func(t *testing.T) {
				fs, _ := newSharedStore(t)
				f := newSharedFiler(t, fs)
				chunks := newChunks(2)
				putObject(t, f, "/buckets/bkt/src", chunks, nil)
				if _, err := link(f, "/buckets/bkt/src", "/buckets/bkt/dst", stop); err != nil {
					t.Fatalf("link: %v", err)
				}
				verifyDeleteSequence(t, f, chunks, deleteSourceFirst)
			})
		}
	}
}

func TestSharedChunksCopyWithEveryStoreWriteFailing(t *testing.T) {
	for n := 1; n <= 12; n++ {
		for _, deleteSourceFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("fail-write-%d/source-first-%v", n, deleteSourceFirst), func(t *testing.T) {
				fs, _ := newSharedStore(t)
				f := newSharedFiler(t, fs)
				chunks := newChunks(2)
				putObject(t, f, "/buckets/bkt/src", chunks, nil)
				fs.arm(n)
				_, _ = link(f, "/buckets/bkt/src", "/buckets/bkt/dst", 0)
				fs.disarm()
				verifyDeleteSequence(t, f, chunks, deleteSourceFirst)
			})
		}
	}
}

// A remote store can commit a write and lose its acknowledgement. If the
// subsequent read also fails, cleanup must not assume the entry is absent.
type uncertainSharedWriteStore struct {
	filer.FilerStore
	target    util.FullPath
	committed bool
}

func (s *uncertainSharedWriteStore) InsertEntry(ctx context.Context, e *filer.Entry) error {
	if err := s.FilerStore.InsertEntry(ctx, e); err != nil {
		return err
	}
	if e.FullPath == s.target {
		s.committed = true
		return errInjected
	}
	return nil
}

func (s *uncertainSharedWriteStore) FindEntry(ctx context.Context, p util.FullPath) (*filer.Entry, error) {
	if s.committed && p == s.target {
		return nil, errInjected
	}
	return s.FilerStore.FindEntry(ctx, p)
}

func TestSharedChunksUncertainWriteKeepsCommittedDestination(t *testing.T) {
	_, raw := newSharedStore(t)
	store := &uncertainSharedWriteStore{FilerStore: raw, target: "/buckets/bkt/dst"}
	f := newSharedFiler(t, store)
	chunks := newChunks(2)
	putObject(t, f, "/buckets/bkt/src", chunks, nil)
	if _, err := link(f, "/buckets/bkt/src", string(store.target), 0); err == nil {
		t.Fatal("expected lost write acknowledgement")
	}
	store.committed = false // store connectivity recovers
	destination, err := f.FindEntry(testCtx(), store.target)
	if err != nil {
		t.Fatalf("committed destination must still exist: %v", err)
	}
	deleteObject(t, f, "/buckets/bkt/src")
	assertNoneDeleted(t, drainDeleted(f), fids(destination.GetChunks()), "ambiguous destination write committed")
}

func TestSharedChunksDeleteWithEveryStoreWriteFailing(t *testing.T) {
	for n := 1; n <= 6; n++ {
		t.Run(fmt.Sprintf("fail-write-%d", n), func(t *testing.T) {
			fs, _ := newSharedStore(t)
			f := newSharedFiler(t, fs)
			chunks := newChunks(2)
			putObject(t, f, "/buckets/bkt/src", chunks, nil)
			mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/dst")

			fs.arm(n)
			_ = f.DeleteEntryMetaAndData(testCtx(), "/buckets/bkt/src", false, false, true, false, nil, 0)
			fs.disarm()
			live := liveObjects(t, f, []string{"/buckets/bkt/src", "/buckets/bkt/dst"})
			deleted := drainDeleted(f)
			for _, lf := range live {
				assertNoneDeleted(t, deleted, lf, "failed delete")
			}
			verifyDeleteSequence(t, f, chunks, false)
		})
	}
}

// verifyDeleteSequence deletes whatever of src and dst is still there, checking
// after every step that nothing a surviving entry uses was freed. A chunk may
// end up never freed (a leak volume.fsck reclaims) but never freed twice.
func verifyDeleteSequence(t *testing.T, f *filer.Filer, chunks []*filer_pb.FileChunk, sourceFirst bool) {
	t.Helper()
	order := []string{"/buckets/bkt/dst", "/buckets/bkt/src"}
	if sourceFirst {
		order = []string{"/buckets/bkt/src", "/buckets/bkt/dst"}
	}
	var all []string
	all = append(all, drainDeleted(f)...)
	for i, p := range order {
		if _, err := f.FindEntry(testCtx(), util.FullPath(p)); err == nil {
			deleteObject(t, f, p)
		}
		deleted := drainDeleted(f)
		all = append(all, deleted...)
		for _, lf := range liveObjects(t, f, order[i+1:]) {
			assertNoneDeleted(t, all, lf, "after deleting "+p)
		}
	}
	for _, id := range fids(chunks) {
		if c := countOf(all, id); c > 1 {
			t.Fatalf("chunk %s freed %d times", id, c)
		}
	}
}

// --- concurrency ---------------------------------------------------------------

func TestSharedChunksConcurrentCopiesOfOneSourceAcrossTwoFilers(t *testing.T) {
	fs, _ := newSharedStore(t)
	filers := []*filer.Filer{newSharedFiler(t, fs), newSharedFiler(t, fs)}
	chunks := newChunks(2)
	putObject(t, filers[0], "/buckets/bkt/src", chunks, nil)

	const copies = 16
	var wg sync.WaitGroup
	linked := make([]bool, copies)
	for i := 0; i < copies; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := link(filers[i%2], "/buckets/bkt/src", fmt.Sprintf("/buckets/bkt/d%d", i), 0)
			if err != nil {
				t.Errorf("link %d: %v", i, err)
			}
			linked[i] = ok
		}(i)
	}
	wg.Wait()

	group, _, ok := sharedRef(mustFind(t, filers[0], "/buckets/bkt/src"))
	if !ok {
		t.Fatalf("source was never marked")
	}
	live := 1
	for i := 0; i < copies; i++ {
		if linked[i] {
			live++
		}
	}
	if got := len(refsIn(t, filers[0], group)); got != live {
		t.Fatalf("refs = %d, live links = %d", got, live)
	}

	paths := []string{"/buckets/bkt/src"}
	for i := 0; i < copies; i++ {
		if linked[i] {
			paths = append(paths, fmt.Sprintf("/buckets/bkt/d%d", i))
		}
	}
	concurrentDeleteAll(t, filers, paths, chunks)
}

func TestSharedChunksConcurrentCopyAndDeleteOfTheSource(t *testing.T) {
	for round := 0; round < 20; round++ {
		fs, _ := newSharedStore(t)
		filers := []*filer.Filer{newSharedFiler(t, fs), newSharedFiler(t, fs)}
		chunks := newChunks(2)
		putObject(t, filers[0], "/buckets/bkt/src", chunks, nil)

		var wg sync.WaitGroup
		var linked bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			linked, _ = link(filers[0], "/buckets/bkt/src", "/buckets/bkt/dst", 0)
		}()
		go func() {
			defer wg.Done()
			_ = deleteObjectErr(filers[1], "/buckets/bkt/src")
		}()
		wg.Wait()

		deleted := append(drainDeleted(filers[0]), drainDeleted(filers[1])...)
		if linked {
			if e, err := filers[0].FindEntry(testCtx(), "/buckets/bkt/dst"); err == nil {
				assertNoneDeleted(t, deleted, fids(e.GetChunks()), fmt.Sprintf("round %d: copy raced the source delete", round))
			}
		}
		rest := []string{}
		for _, p := range []string{"/buckets/bkt/src", "/buckets/bkt/dst"} {
			if _, err := filers[0].FindEntry(testCtx(), util.FullPath(p)); err == nil {
				rest = append(rest, p)
			}
		}
		for _, p := range rest {
			deleteObject(t, filers[0], p)
		}
		deleted = append(deleted, drainDeleted(filers[0])...)
		for _, id := range fids(chunks) {
			if c := countOf(deleted, id); c > 2 {
				t.Fatalf("round %d: chunk %s freed %d times", round, id, c)
			}
		}
	}
}

func TestSharedChunksConcurrentOverwriteAndCopyOfTheSource(t *testing.T) {
	for round := 0; round < 20; round++ {
		fs, _ := newSharedStore(t)
		filers := []*filer.Filer{newSharedFiler(t, fs), newSharedFiler(t, fs)}
		oldChunks := newChunks(2)
		newChunkList := newChunks(1)
		putObject(t, filers[0], "/buckets/bkt/src", oldChunks, nil)

		var wg sync.WaitGroup
		var linked bool
		wg.Add(2)
		go func() {
			defer wg.Done()
			linked, _ = link(filers[0], "/buckets/bkt/src", "/buckets/bkt/dst", 0)
		}()
		go func() {
			defer wg.Done()
			_ = createObject(filers[1], "/buckets/bkt/src", newChunkList, nil)
		}()
		wg.Wait()

		deleted := append(drainDeleted(filers[0]), drainDeleted(filers[1])...)
		for _, p := range []string{"/buckets/bkt/src", "/buckets/bkt/dst"} {
			if e, err := filers[0].FindEntry(testCtx(), util.FullPath(p)); err == nil {
				assertNoneDeleted(t, deleted, fids(e.GetChunks()), fmt.Sprintf("round %d (linked=%v): %s", round, linked, p))
			}
		}
	}
}

// concurrentDeleteAll deletes every path at once from alternating filers and
// requires that every shared chunk ends up freed, and none while still used.
func concurrentDeleteAll(t *testing.T, filers []*filer.Filer, paths []string, chunks []*filer_pb.FileChunk) {
	t.Helper()
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			if err := deleteObjectErr(filers[i%len(filers)], p); err != nil {
				t.Errorf("delete %s: %v", p, err)
			}
		}(i, p)
	}
	wg.Wait()
	var deleted []string
	for _, f := range filers {
		deleted = append(deleted, drainDeleted(f)...)
	}
	for _, id := range fids(chunks) {
		if c := countOf(deleted, id); c < 1 {
			t.Fatalf("chunk %s never freed after every link was deleted", id)
		}
	}
}

func TestSharedChunksConcurrentDeletesOfTwoLinks(t *testing.T) {
	for round := 0; round < 20; round++ {
		fs, _ := newSharedStore(t)
		filers := []*filer.Filer{newSharedFiler(t, fs), newSharedFiler(t, fs)}
		chunks := newChunks(2)
		putObject(t, filers[0], "/buckets/bkt/src", chunks, nil)
		mustLink(t, filers[0], "/buckets/bkt/src", "/buckets/bkt/dst")
		concurrentDeleteAll(t, filers, []string{"/buckets/bkt/src", "/buckets/bkt/dst"}, chunks)
	}
}

// --- read path cost -------------------------------------------------------------

// Reading an object that shares chunks must cost what reading a plain object
// costs: one store read, no key-value lookup for a shared record.
func TestSharedChunksReadCostsNoMoreThanAPlainObject(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	putObject(t, f, "/buckets/bkt/plain", newChunks(2), nil)
	putObject(t, f, "/buckets/bkt/src", newChunks(2), nil)
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/dst")

	measure := func(path string) (reads, kv int64) {
		fs.reads.Store(0)
		fs.kvReads.Store(0)
		for i := 0; i < 100; i++ {
			mustFind(t, f, path)
		}
		return fs.reads.Load(), fs.kvReads.Load()
	}
	plainReads, plainKv := measure("/buckets/bkt/plain")
	sharedReads, sharedKv := measure("/buckets/bkt/dst")
	if sharedReads != plainReads || sharedKv != plainKv {
		t.Fatalf("100 reads of a shared object: %d store reads, %d kv reads; plain: %d, %d",
			sharedReads, sharedKv, plainReads, plainKv)
	}

	listCost := func(dir string) int64 {
		fs.reads.Store(0)
		fs.kvReads.Store(0)
		_, err := f.Store.ListDirectoryEntries(testCtx(), util.FullPath(dir), "", false, 1000, func(*filer.Entry) (bool, error) { return true, nil })
		if err != nil {
			t.Fatal(err)
		}
		return fs.reads.Load() + fs.kvReads.Load()
	}
	if got := listCost("/buckets/bkt"); got != 0 {
		t.Fatalf("listing a folder of shared objects did %d extra point lookups", got)
	}
}

func benchmarkFind(b *testing.B, shared bool) {
	fs, _ := newSharedStore(b)
	f := newSharedFiler(b, fs)
	putObject(b, f, "/buckets/bkt/src", newChunks(4), nil)
	path := "/buckets/bkt/src"
	if shared {
		if linked, err := link(f, "/buckets/bkt/src", "/buckets/bkt/dst", 0); err != nil || !linked {
			b.Fatalf("link: %v %v", linked, err)
		}
		path = "/buckets/bkt/dst"
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.FindEntry(testCtx(), util.FullPath(path)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSharedChunksFindPlain(b *testing.B)  { benchmarkFind(b, false) }
func BenchmarkSharedChunksFindShared(b *testing.B) { benchmarkFind(b, true) }

// benchmarkDelete times one delete: of a plain object, of a link whose source
// stays (the chunks are kept), or of the last link (the chunks are freed).
func benchmarkDelete(b *testing.B, shared, last bool) {
	fs, _ := newSharedStore(b)
	f := newSharedFiler(b, fs)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		src := fmt.Sprintf("/buckets/bkt/s%d", i)
		dst := fmt.Sprintf("/buckets/bkt/d%d", i)
		putObject(b, f, src, newChunks(2), nil)
		target := src
		if shared {
			if linked, err := link(f, src, dst, 0); err != nil || !linked {
				b.Fatalf("link: %v %v", linked, err)
			}
			target = dst
			if last {
				deleteObject(b, f, src)
				drainDeleted(f)
			}
		}
		b.StartTimer()
		deleteObject(b, f, target)
		b.StopTimer()
		drainDeleted(f)
		b.StartTimer()
	}
}

func BenchmarkSharedChunksDeletePlain(b *testing.B)    { benchmarkDelete(b, false, false) }
func BenchmarkSharedChunksDeleteShared(b *testing.B)   { benchmarkDelete(b, true, false) }
func BenchmarkSharedChunksDeleteLastLink(b *testing.B) { benchmarkDelete(b, true, true) }

// --- membership rules -------------------------------------------------------------

// A rewrite of a member that keeps its chunks but was built without the marker
// (a metadata-only copy onto itself, a client that round-trips the entry) must
// not take the entry out of its group: the group's last other member would then
// free chunks this entry still uses.
func TestSharedChunksRewriteWithoutTheMarkerKeepsMembership(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	chunks := newChunks(2)
	putObject(t, f, "/buckets/bkt/src", chunks, nil)
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/dst")

	dst := mustFind(t, f, "/buckets/bkt/dst")
	rewritten := dst.ShallowClone()
	rewritten.Extended = map[string][]byte{"X-Amz-Meta-Replaced": []byte("yes")}
	if err := f.UpdateEntry(testCtx(), dst, rewritten); err != nil {
		t.Fatal(err)
	}
	if _, _, member := sharedRef(mustFind(t, f, "/buckets/bkt/dst")); !member {
		t.Fatalf("the rewrite dropped the destination out of its group")
	}

	deleteObject(t, f, "/buckets/bkt/src")
	assertNoneDeleted(t, drainDeleted(f), fids(chunks), "delete source after rewriting the destination")
	deleteObject(t, f, "/buckets/bkt/dst")
	assertDeletedOnce(t, drainDeleted(f), fids(chunks), "delete last link")
}

func TestSharedChunksLinkToAChangedSourceIsRefused(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	chunks := newChunks(2)
	putObject(t, f, "/buckets/bkt/src", chunks, nil)
	if _, err := link(f, "/buckets/bkt/src", "/buckets/bkt/unused", stepMarkSource); err != nil {
		t.Fatal(err)
	}
	src := mustFind(t, f, "/buckets/bkt/src")
	group, nonce, _ := sharedRef(src)

	putObject(t, f, "/buckets/bkt/src", newChunks(1), nil) // overwritten before the link
	err := createObject(f, "/buckets/bkt/dst", cloneChunks(chunks), map[string][]byte{
		sharedChunksKey: []byte(group + "/" + newNonce()),
		linkSourceKey:   []byte("/buckets/bkt/src\n" + group + "/" + nonce),
	})
	if err == nil || !strings.Contains(err.Error(), "link source changed") {
		t.Fatalf("link to an overwritten source: err=%v, want link source changed", err)
	}
	if _, err := f.FindEntry(testCtx(), "/buckets/bkt/dst"); err == nil {
		t.Fatalf("the refused link still created the destination")
	}
	if refs := refsIn(t, f, group); len(refs) != 0 {
		t.Fatalf("the refused link left references behind: %v", refs)
	}
}

func TestSharedChunksScanFindsAReferenceLeftByACrash(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	putObject(t, f, "/buckets/bkt/src", newChunks(1), nil)
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/dst")
	group, _, _ := sharedRef(mustFind(t, f, "/buckets/bkt/src"))

	// a copy that wrote its reference and died before its entry
	orphan := filer.NewSharedChunksRef(group)
	if err := f.Store.InsertEntry(testCtx(), filer.NewSharedChunksRefEntry(orphan, "/buckets/bkt/never-written")); err != nil {
		t.Fatal(err)
	}

	var live, orphans []filer.SharedChunksRefState
	if err := f.ScanSharedChunksRefs(testCtx(), func(s filer.SharedChunksRefState) error {
		if s.Orphan {
			orphans = append(orphans, s)
		} else {
			live = append(live, s)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || len(orphans) != 1 || orphans[0].Ref != orphan {
		t.Fatalf("scan: %d live, orphans %+v; want 2 live and the one orphan %v", len(live), orphans, orphan)
	}
}

func TestSharedChunksGroupDirectoryGoesWithItsLastMember(t *testing.T) {
	fs, _ := newSharedStore(t)
	f := newSharedFiler(t, fs)
	putObject(t, f, "/buckets/bkt/src", newChunks(1), nil)
	mustLink(t, f, "/buckets/bkt/src", "/buckets/bkt/dst")
	group, _, _ := sharedRef(mustFind(t, f, "/buckets/bkt/src"))
	if _, err := f.Store.FindEntry(testCtx(), util.FullPath(sharedChunksRefDir+"/"+group)); err != nil {
		t.Fatalf("group directory missing while the group is alive: %v", err)
	}
	deleteObject(t, f, "/buckets/bkt/src")
	deleteObject(t, f, "/buckets/bkt/dst")
	if _, err := f.Store.FindEntry(testCtx(), util.FullPath(sharedChunksRefDir+"/"+group)); err == nil {
		t.Fatalf("group directory left behind after its last member was deleted")
	}
}
