package filer

// Shared chunk lists
//
// An S3 CopyObject of a whole, unencrypted object can give the destination the
// source's chunk fids instead of copying the bytes. The filer frees an entry's
// chunks when the entry is deleted or overwritten (DeleteEntryMetaAndData,
// deleteChunksIfNotNew, the garbage of UpdateEntry and rename) and keeps no
// index of who else points at a chunk, so two entries on one chunk list need a
// reference count.
//
// The existing hard links do not fit: the hard-link KV record holds the whole
// entry (EncodeAttributesAndChunks), and maybeReadHardLink overlays it onto
// every link, so S3 metadata, tags and ETag would be shared between the objects;
// and HardLinkCounter is a read-modify-write of that record (DeleteHardLink,
// and the mount's Link incrementing it client-side) with no compare-and-swap, so
// two filers updating it concurrently lose an update, and a counter below the
// number of live links frees data still in use.
//
// Membership is therefore recorded per entry and counted without any
// read-modify-write:
//
//   - A member entry keeps its own attributes, Extended and a full copy of the
//     chunk list, and carries Extended[SharedChunksExtKey] = "<group>/<nonce>".
//     Reading it needs nothing beyond the entry itself.
//   - Every member owns one reference entry, SharedChunksRefDir/<group>/<nonce>,
//     whose Extended[SharedChunksOwnerExtKey] names the member's path. A group is
//     alive while its directory lists any reference.
//
// Ordering, so that an interruption at any point leaves at worst a leak:
//
//   - A reference is written before the entry that carries it
//     (ensureSharedChunksRef runs before the store write), so no stored member
//     lacks one.
//   - A reference is removed only after its entry stopped carrying it (after the
//     store delete, or after an overwrite without it), and the chunks are freed
//     only by the remover that then lists the group empty. Two removers can both
//     list it empty; the chunks are then queued twice, which the volume delete
//     tolerates.
//   - The S3 gateway links a destination in two requests. It first marks the
//     source a member (a conditional PATCH_EXTENDED: the source's chunks must be
//     unchanged), which writes the source's reference. It then creates the
//     destination with the source's chunk list, its own marker, and a one-shot
//     SharedChunksLinkSourceExtKey hint naming the source and its marker. For
//     that write the store wrapper writes the destination's reference, then
//     checks that the source still carries the marker with the same chunks,
//     and only then stores the destination (linkSharedChunks). A source deleted
//     or overwritten before that check fails the create; one deleted after it
//     lists the destination's reference and keeps the chunks.
//
// A crash between two steps leaves a reference whose owner does not carry it (a
// leak: the group never lists empty, so its chunks are never freed and
// volume.fsck reports them once no entry references them) or an entry that
// stopped carrying a reference not yet removed (the same). Neither frees data in
// use. ScanSharedChunksRefs finds those references.
//
// The count is only as good as the store's view: every filer must use the same
// store with read-your-writes consistency. Filers with separate stores that
// replicate through metadata events do not see each other's references, so the
// S3 gateway only shares chunks when told to. Entries replicated from another
// cluster have the marker removed (StripSharedChunksRef): the replica wrote its
// own chunks.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

const (
	// SharedChunksExtKey marks an entry that shares its chunk list with other
	// entries of the same group. Value: "<group hex>/<nonce hex>".
	SharedChunksExtKey = "Seaweed-Shared-Chunks"
	// SharedChunksOwnerExtKey on a reference entry names the member that owns it.
	SharedChunksOwnerExtKey = "Seaweed-Shared-Chunks-Owner"
	// SharedChunksRefDir holds one directory per group and one entry per member.
	SharedChunksRefDir = "/etc/seaweedfs/shared_chunks"
	// SharedChunksLinkSourceExtKey is a write-time hint on an entry being linked
	// to a source's chunks: "<source full path>\n<source marker>". It is removed
	// before the entry is stored.
	SharedChunksLinkSourceExtKey = "Seaweed-Shared-Chunks-Link-Source"

	sharedChunksGroupBytes = 16
	sharedChunksNonceBytes = 8
)

// SharedChunksRef identifies one member of a group of entries sharing a chunk
// list.
type SharedChunksRef struct {
	Group string
	Nonce string
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("shared chunks: read random: %v", err))
	}
	return hex.EncodeToString(b)
}

// SharedChunksGroupOf names the group of a chunk list by its content: the hash
// of its sorted fids. Everyone who shares a chunk list derives the same group, so
// two copies marking one source concurrently cannot split it into two groups,
// and a delete working from an entry read before the source was marked can still
// find the group its chunks belong to.
func SharedChunksGroupOf(chunks []*filer_pb.FileChunk) string {
	ids := make([]string, 0, len(chunks))
	for _, c := range chunks {
		ids = append(ids, c.GetFileIdString())
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(sum[:sharedChunksGroupBytes])
}

// NewSharedChunksRef returns a new member of group.
func NewSharedChunksRef(group string) SharedChunksRef {
	return SharedChunksRef{Group: group, Nonce: randomHex(sharedChunksNonceBytes)}
}

func (r SharedChunksRef) String() string { return r.Group + "/" + r.Nonce }

// Bytes is the value stored under SharedChunksExtKey.
func (r SharedChunksRef) Bytes() []byte { return []byte(r.String()) }

// GroupDir is the directory holding the group's references.
func (r SharedChunksRef) GroupDir() util.FullPath {
	return util.FullPath(SharedChunksRefDir + "/" + r.Group)
}

// RefPath is the path of this member's reference entry.
func (r SharedChunksRef) RefPath() util.FullPath {
	return r.GroupDir().Child(r.Nonce)
}

func isHexOfLen(s string, bytes int) bool {
	if len(s) != 2*bytes {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ParseSharedChunksRef reads the membership marker from an entry's Extended.
// A malformed value is not a membership: the entry's chunks are its own.
func ParseSharedChunksRef(extended map[string][]byte) (SharedChunksRef, bool) {
	if extended == nil {
		return SharedChunksRef{}, false
	}
	v, found := extended[SharedChunksExtKey]
	if !found {
		return SharedChunksRef{}, false
	}
	group, nonce, ok := strings.Cut(string(v), "/")
	if !ok || !isHexOfLen(group, sharedChunksGroupBytes) || !isHexOfLen(nonce, sharedChunksNonceBytes) {
		return SharedChunksRef{}, false
	}
	return SharedChunksRef{Group: group, Nonce: nonce}, true
}

// entrySharedChunksRef is ParseSharedChunksRef for an entry that may be nil or a
// directory.
func entrySharedChunksRef(entry *Entry) (SharedChunksRef, bool) {
	if entry == nil || entry.IsDirectory() {
		return SharedChunksRef{}, false
	}
	return ParseSharedChunksRef(entry.Extended)
}

// setSharedChunksMarker gives entry the marker ref. The Extended map is copied
// first: entries are often shallow clones sharing one map with the entry they
// came from (a rename's source), which must keep its own marker.
func setSharedChunksMarker(entry *Entry, ref SharedChunksRef) {
	extended := make(map[string][]byte, len(entry.Extended)+1)
	for k, v := range entry.Extended {
		extended[k] = v
	}
	extended[SharedChunksExtKey] = ref.Bytes()
	entry.Extended = extended
}

func sharesAnyChunk(a, b []*filer_pb.FileChunk) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	ids := make(map[string]struct{}, len(a))
	for _, c := range a {
		ids[c.GetFileIdString()] = struct{}{}
	}
	for _, c := range b {
		if _, ok := ids[c.GetFileIdString()]; ok {
			return true
		}
	}
	return false
}

// carrySharedChunksMembership keeps a member a member across a write that keeps
// any of its chunks. Dropping the marker there (a metadata rewrite built without
// it, or a second marking choosing another nonce) would release the entry's
// reference while it still uses the chunks, and the group's last other member
// would then free them under it.
func carrySharedChunksMembership(existing, entry *Entry) {
	existingRef, member := entrySharedChunksRef(existing)
	if !member || entry == nil || entry.IsDirectory() || !sharesAnyChunk(existing.GetChunks(), entry.GetChunks()) {
		return
	}
	if ref, ok := entrySharedChunksRef(entry); ok && ref == existingRef {
		return
	}
	if ref, ok := entrySharedChunksRef(entry); ok && ref.Group == existingRef.Group {
		// a concurrent marking chose another nonce for the same member
		setSharedChunksMarker(entry, existingRef)
		return
	}
	if _, ok := entrySharedChunksRef(entry); !ok {
		setSharedChunksMarker(entry, existingRef)
	}
}

// StripSharedChunksRef removes the membership marker and any link hint. Used for
// entries that do not share the chunks of the group they name, e.g. replicated
// from another cluster, where the replica wrote chunks of its own.
func StripSharedChunksRef(extended map[string][]byte) {
	if extended != nil {
		delete(extended, SharedChunksExtKey)
		delete(extended, SharedChunksLinkSourceExtKey)
	}
}

// NewSharedChunksRefEntry is the reference entry of ref owned by owner.
func NewSharedChunksRefEntry(ref SharedChunksRef, owner util.FullPath) *Entry {
	now := time.Now()
	return &Entry{
		FullPath: ref.RefPath(),
		Attr:     Attr{Mtime: now, Crtime: now, Mode: 0644},
		Extended: map[string][]byte{SharedChunksOwnerExtKey: []byte(owner)},
	}
}

func refOwner(refEntry *Entry) util.FullPath {
	if refEntry == nil || refEntry.Extended == nil {
		return ""
	}
	return util.FullPath(refEntry.Extended[SharedChunksOwnerExtKey])
}

// sharedChunksStore is the part of the store the reference bookkeeping uses.
type sharedChunksStore interface {
	FindEntry(context.Context, util.FullPath) (*Entry, error)
	InsertEntry(context.Context, *Entry) error
	UpdateEntry(context.Context, *Entry) error
	DeleteOneEntry(context.Context, *Entry) error
	ListDirectoryEntries(ctx context.Context, dirPath util.FullPath, startFileName string, includeStartFile bool, limit int64, eachEntryFunc ListEachEntryFunc) (string, error)
}

func isNotFound(err error) bool {
	return err != nil && (errors.Is(err, filer_pb.ErrNotFound) || err == filer_pb.ErrNotFound)
}

// ErrSharedChunksSourceChanged: the source of a link no longer carries the
// marker and chunks the link was made from, so its chunks may already be freed.
var ErrSharedChunksSourceChanged = errors.New("shared chunks: link source changed")

// SharedChunksLinkSource builds the SharedChunksLinkSourceExtKey value.
func SharedChunksLinkSource(source util.FullPath, sourceRef SharedChunksRef) []byte {
	return []byte(string(source) + "\n" + sourceRef.String())
}

func parseSharedChunksLinkSource(v []byte) (util.FullPath, SharedChunksRef, bool) {
	path, marker, ok := strings.Cut(string(v), "\n")
	if !ok || path == "" {
		return "", SharedChunksRef{}, false
	}
	ref, ok := ParseSharedChunksRef(map[string][]byte{SharedChunksExtKey: []byte(marker)})
	return util.FullPath(path), ref, ok
}

func sameChunkFids(a, b []*filer_pb.FileChunk) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, c := range a {
		counts[c.GetFileIdString()]++
	}
	for _, c := range b {
		id := c.GetFileIdString()
		if counts[id] == 0 {
			return false
		}
		counts[id]--
	}
	return true
}

// prepareSharedChunksWrite runs before entry is stored. It takes the link hint
// off entry, writes entry's reference, and for a link checks that the source
// still carries the marker and chunks entry was linked from; returns the
// reference to release if the store write that follows fails.
func prepareSharedChunksWrite(ctx context.Context, store sharedChunksStore, entry *Entry) (written SharedChunksRef, wrote bool, err error) {
	var hint []byte
	if entry.Extended != nil {
		hint = entry.Extended[SharedChunksLinkSourceExtKey]
		delete(entry.Extended, SharedChunksLinkSourceExtKey)
	}
	if _, member := entrySharedChunksRef(entry); !member {
		if hint != nil {
			return SharedChunksRef{}, false, fmt.Errorf("%w: %s has a link source but no marker", ErrSharedChunksSourceChanged, entry.FullPath)
		}
		return SharedChunksRef{}, false, nil
	}
	if err := ensureSharedChunksRef(ctx, store, entry); err != nil {
		return SharedChunksRef{}, false, err
	}
	written, _ = entrySharedChunksRef(entry) // ensureSharedChunksRef may have renewed the nonce
	if hint == nil {
		return written, true, nil
	}
	sourcePath, sourceRef, ok := parseSharedChunksLinkSource(hint)
	if ok && sourceRef.Group == written.Group {
		source, findErr := store.FindEntry(ctx, sourcePath)
		if findErr == nil {
			if current, isMember := entrySharedChunksRef(source); isMember && current == sourceRef && sameChunkFids(source.GetChunks(), entry.GetChunks()) {
				// The source's reference must still be there too: a delete that
				// raced the marking may have released it (and, finding no other
				// reference yet, freed the chunks) while the entry was written
				// back. With it present, that delete's own re-listing sees the
				// reference written above.
				if sourceRefEntry, refErr := store.FindEntry(ctx, sourceRef.RefPath()); refErr == nil && refOwner(sourceRefEntry) == sourcePath {
					return written, true, nil
				} else if refErr != nil && !isNotFound(refErr) {
					abandonSharedChunksRef(ctx, store, written, entry.FullPath)
					return SharedChunksRef{}, false, fmt.Errorf("find link source reference %s: %w", sourceRef.RefPath(), refErr)
				}
			}
		} else if !isNotFound(findErr) {
			abandonSharedChunksRef(ctx, store, written, entry.FullPath)
			return SharedChunksRef{}, false, fmt.Errorf("find link source %s: %w", sourcePath, findErr)
		}
	}
	abandonSharedChunksRef(ctx, store, written, entry.FullPath)
	return SharedChunksRef{}, false, fmt.Errorf("%w: %s", ErrSharedChunksSourceChanged, sourcePath)
}

// abandonSharedChunksRef drops a reference written for an entry that was not
// stored after all. A failure leaves the reference as a leak.
func abandonSharedChunksRef(ctx context.Context, store sharedChunksStore, ref SharedChunksRef, owner util.FullPath) {
	if err := releaseSharedChunksRef(ctx, store, ref, owner); err != nil {
		glog.WarningfCtx(ctx, "drop shared chunks reference %s of %s: %v (leaked)", ref, owner, err)
	}
}

// ensureSharedChunksGroupDir creates the group's directory entry (and the root
// of the reference tree) so ScanSharedChunksRefs can list the groups. The
// references themselves are found by their directory whether or not these exist.
func ensureSharedChunksGroupDir(ctx context.Context, store sharedChunksStore, ref SharedChunksRef) error {
	for _, dir := range []util.FullPath{util.FullPath(SharedChunksRefDir), ref.GroupDir()} {
		if _, err := store.FindEntry(ctx, dir); err == nil {
			continue
		} else if !isNotFound(err) {
			return err
		}
		now := time.Now()
		if err := store.InsertEntry(ctx, &Entry{
			FullPath: dir,
			Attr:     Attr{Mtime: now, Crtime: now, Mode: os.ModeDir | 0755},
		}); err != nil {
			return err
		}
	}
	return nil
}

func insertSharedChunksRef(ctx context.Context, store sharedChunksStore, ref SharedChunksRef, owner util.FullPath) error {
	if err := ensureSharedChunksGroupDir(ctx, store, ref); err != nil {
		return fmt.Errorf("create shared chunks group %s: %w", ref.GroupDir(), err)
	}
	return store.InsertEntry(ctx, NewSharedChunksRefEntry(ref, owner))
}

// ensureSharedChunksRef makes sure the reference named by entry's marker exists
// and belongs to entry, before entry is written. When the reference already
// belongs to another live entry carrying the same marker (the entry was copied
// to a second path: a rename's new path, a mount hard link, a crash between a
// rename's two writes), entry gets a fresh nonce of the same group so each path
// holds its own reference. A reference whose owner no longer carries it is taken
// over.
func ensureSharedChunksRef(ctx context.Context, store sharedChunksStore, entry *Entry) error {
	ref, ok := entrySharedChunksRef(entry)
	if !ok {
		return nil
	}
	current, err := store.FindEntry(ctx, ref.RefPath())
	if isNotFound(err) {
		return insertSharedChunksRef(ctx, store, ref, entry.FullPath)
	}
	if err != nil {
		return fmt.Errorf("find shared chunks reference %s: %w", ref.RefPath(), err)
	}
	owner := refOwner(current)
	if owner == entry.FullPath {
		return nil
	}
	if owner != "" {
		other, otherErr := store.FindEntry(ctx, owner)
		if otherErr != nil && !isNotFound(otherErr) {
			return fmt.Errorf("find shared chunks owner %s: %w", owner, otherErr)
		}
		if otherErr == nil {
			if otherRef, isMember := entrySharedChunksRef(other); isMember && otherRef == ref {
				fresh := NewSharedChunksRef(ref.Group)
				setSharedChunksMarker(entry, fresh)
				return insertSharedChunksRef(ctx, store, fresh, entry.FullPath)
			}
		}
	}
	takeover := NewSharedChunksRefEntry(ref, entry.FullPath)
	takeover.Crtime = current.Crtime
	return store.UpdateEntry(ctx, takeover)
}

// releaseSharedChunksRef removes the reference ref if owner still owns it. Call
// it only after owner stopped carrying ref.
func releaseSharedChunksRef(ctx context.Context, store sharedChunksStore, ref SharedChunksRef, owner util.FullPath) error {
	current, err := store.FindEntry(ctx, ref.RefPath())
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if o := refOwner(current); o != "" && o != owner {
		// another path took this reference over; it is not ours to drop
		return nil
	}
	return store.DeleteOneEntry(ctx, current)
}

// sharedChunksLeft reports whether a write that replaced old with updated left
// old's reference behind. updated nil means old was deleted.
func sharedChunksLeft(old, updated *Entry) (SharedChunksRef, bool) {
	oldRef, ok := entrySharedChunksRef(old)
	if !ok {
		return SharedChunksRef{}, false
	}
	if newRef, stillMember := entrySharedChunksRef(updated); stillMember && newRef == oldRef {
		return SharedChunksRef{}, false
	}
	return oldRef, true
}

// sharedGroupReferenced reports whether any reference of group remains. A
// failed listing counts as referenced: keeping chunks is a leak, freeing them is
// data loss.
func sharedGroupReferenced(ctx context.Context, store sharedChunksStore, group string) bool {
	found := false
	dir := SharedChunksRef{Group: group}.GroupDir()
	if _, err := store.ListDirectoryEntries(ctx, dir, "", false, 1, func(*Entry) (bool, error) {
		found = true
		return false, nil
	}); err != nil {
		glog.WarningfCtx(ctx, "list shared chunks references %s: %v; keeping the chunks", dir, err)
		return true
	}
	return found
}

// inSharedChunksScope: only S3 objects are ever linked, so only entries under the
// buckets directory can hold shared chunks without carrying the marker (a
// stale read of a source marked concurrently).
func (f *Filer) inSharedChunksScope(p util.FullPath) bool {
	return f.DirBucketsPath != "" && strings.HasPrefix(string(p), f.DirBucketsPath+"/")
}

// sharedChunksGroupFor returns the group whose references decide whether the
// chunks of entry may be freed: its marker's group, or, for an unmarked entry
// that may have been read just before a copy marked it, the group of its chunk
// list's content.
func (f *Filer) sharedChunksGroupFor(entry *Entry) (string, bool) {
	if ref, ok := entrySharedChunksRef(entry); ok {
		return ref.Group, true
	}
	if entry == nil || entry.IsDirectory() || len(entry.GetChunks()) == 0 || !f.inSharedChunksScope(entry.FullPath) {
		return "", false
	}
	return SharedChunksGroupOf(entry.GetChunks()), true
}

// SharedChunksHeld is asked after the store stopped holding gone at its path:
// deleted (current nil) or replaced by current. It drops the references of gone's
// group that gone's path still owns (all but the one current carries), then
// reports whether the group still has a reference, in which case gone's chunks
// must not be freed. An entry whose chunks nobody shares reports false.
func (f *Filer) SharedChunksHeld(ctx context.Context, gone, current *Entry) bool {
	group, ok := f.sharedChunksGroupFor(gone)
	if !ok {
		return false
	}
	keep, keepOk := entrySharedChunksRef(current)
	var refs []*Entry
	if err := listAll(ctx, f.Store, SharedChunksRef{Group: group}.GroupDir(), func(e *Entry) error {
		if !e.IsDirectory() {
			refs = append(refs, e)
		}
		return nil
	}); err != nil {
		glog.WarningfCtx(ctx, "list shared chunks group %s for %s: %v; keeping the chunks", group, gone.FullPath, err)
		return true
	}
	remaining := 0
	for _, r := range refs {
		if keepOk && keep.Group == group && keep.Nonce == r.Name() {
			remaining++
			continue
		}
		if refOwner(r) == gone.FullPath {
			if err := f.Store.DeleteOneEntry(ctx, r); err != nil {
				glog.WarningfCtx(ctx, "release shared chunks reference %s/%s of %s: %v; keeping the chunks", group, r.Name(), gone.FullPath, err)
				return true
			}
			continue
		}
		remaining++
	}
	if remaining > 0 {
		return true
	}
	// Other filers may have added a reference since the listing above; ask
	// the store again before letting the caller free the chunks.
	if sharedGroupReferenced(ctx, f.Store, group) {
		return true
	}
	// A plain object's content group never had a directory; skip the lookup.
	if _, member := entrySharedChunksRef(gone); member || len(refs) > 0 {
		f.removeEmptySharedChunksGroup(ctx, group)
	}
	return false
}

// removeEmptySharedChunksGroup drops a group's directory entry once it holds no
// reference. Best-effort: a leftover directory costs one entry.
func (f *Filer) removeEmptySharedChunksGroup(ctx context.Context, group string) {
	dir := SharedChunksRef{Group: group}.GroupDir()
	entry, err := f.Store.FindEntry(ctx, dir)
	if err != nil {
		return
	}
	if err := f.Store.DeleteOneEntry(ctx, entry); err != nil {
		glog.V(1).InfofCtx(ctx, "remove shared chunks group %s: %v", dir, err)
	}
}

// SharedChunksRefState describes one reference entry.
type SharedChunksRefState struct {
	Ref   SharedChunksRef
	Owner util.FullPath
	Age   time.Duration
	// Orphan: the owner does not exist or no longer carries this reference.
	// The group's chunks are kept until the reference is removed.
	Orphan bool
}

// ScanSharedChunksRefs visits every reference entry and reports whether its
// owner still carries it. An orphan older than any copy in flight is a leak:
// removing it (the reference entry only) lets the group's chunks be freed with
// its last member; chunks no entry references at all are volume.fsck's.
func (f *Filer) ScanSharedChunksRefs(ctx context.Context, fn func(SharedChunksRefState) error) error {
	var groups []string
	if err := listAll(ctx, f.Store, util.FullPath(SharedChunksRefDir), func(e *Entry) error {
		if e.IsDirectory() {
			groups = append(groups, e.Name())
		}
		return nil
	}); err != nil {
		return err
	}
	now := time.Now()
	for _, group := range groups {
		var refs []*Entry
		if err := listAll(ctx, f.Store, SharedChunksRef{Group: group}.GroupDir(), func(e *Entry) error {
			if !e.IsDirectory() {
				refs = append(refs, e)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, r := range refs {
			state := SharedChunksRefState{
				Ref:   SharedChunksRef{Group: group, Nonce: r.Name()},
				Owner: refOwner(r),
				Age:   now.Sub(r.Crtime),
			}
			owner, err := f.Store.FindEntry(ctx, state.Owner)
			switch {
			case isNotFound(err) || state.Owner == "":
				state.Orphan = true
			case err != nil:
				return err
			default:
				ownerRef, ok := entrySharedChunksRef(owner)
				state.Orphan = !ok || ownerRef != state.Ref
			}
			if err := fn(state); err != nil {
				return err
			}
		}
	}
	return nil
}

func listAll(ctx context.Context, store sharedChunksStore, dir util.FullPath, fn func(*Entry) error) error {
	start := ""
	for {
		var batch []*Entry
		if _, err := store.ListDirectoryEntries(ctx, dir, start, false, PaginationSize, func(e *Entry) (bool, error) {
			batch = append(batch, e)
			return true, nil
		}); err != nil {
			if isNotFound(err) {
				return nil
			}
			return err
		}
		for _, e := range batch {
			if err := fn(e); err != nil {
				return err
			}
		}
		if len(batch) < PaginationSize {
			return nil
		}
		start = batch[len(batch)-1].Name()
	}
}
