package meta_cache

import (
	"context"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// A cached directory's listing is current, per filer process, through a
// position in that filer's log. Every filer appends to its own log in a strict
// order, and the metadata stream relays each filer's events in that order, so
// a cache that has applied a filer's event at a position holds every change
// that filer appended before it. A listing read after the filers named their
// positions holds everything up to them too: a filer appends a change after
// storing it. Positions of different filers say nothing about each other, and
// a mount's own changes, which its stream never delivers, never move them.
type directoryOrigins struct {
	// basis: the positions include those named before the listing was read,
	// so a filer missing from them has appended nothing the listing lacks
	basis     bool
	positions map[int32]int64
	// generation moves on every change applied to the directory, from any
	// source; an unchanged generation means an unchanged listing.
	generation uint64
}

func (mc *MetaCache) originsLocked(dir util.FullPath) *directoryOrigins {
	o := mc.dirChangesApplied[dir]
	if o == nil {
		o = &directoryOrigins{positions: make(map[int32]int64)}
		mc.dirChangesApplied[dir] = o
	}
	return o
}

// SetDirectoryPositionFn installs how a directory build asks the filers for
// their positions before listing.
func (mc *MetaCache) SetDirectoryPositionFn(fn func(ctx context.Context, dir util.FullPath) ([]*filer_pb.FilerDirectoryPosition, error)) {
	mc.positionFn = fn
}

// noteChangesApplied moves the generation of each cached directory whose
// children resp changed and, for an event the stream relayed from its origin
// filer, that filer's position. Runs on the apply loop after the store took
// resp.
func (mc *MetaCache) noteChangesApplied(resp *filer_pb.SubscribeMetadataResponse) {
	if resp == nil {
		return
	}
	dirs := filer.ChangedDirectories(resp)
	if len(dirs) == 0 {
		return
	}
	origin := resp.GetEventNotification().GetOriginFilerId()
	position := resp.GetEventNotification().GetOriginPositionNs()
	advanced := false
	mc.Lock()
	for _, dir := range dirs {
		// An uncached directory reads through to the filer and needs no
		// position; recording one would only grow the map.
		if !mc.isCachedFn(dir) && !mc.isBuildingDir(dir) {
			continue
		}
		o := mc.originsLocked(dir)
		mc.generations++
		o.generation = mc.generations
		if origin != 0 && position > o.positions[origin] {
			o.positions[origin] = position
			advanced = true
		}
	}
	mc.Unlock()
	if advanced {
		mc.signalAppliedAdvanced()
	}
}

// NoteDirectoryBasis records that dir's cached listing was read after the
// filers named positions: it holds every change up to each of them.
func (mc *MetaCache) NoteDirectoryBasis(dir util.FullPath, positions []*filer_pb.FilerDirectoryPosition) {
	mc.Lock()
	if !mc.isCachedFn(dir) {
		mc.Unlock()
		return
	}
	o := mc.originsLocked(dir)
	o.basis = true
	for _, p := range positions {
		if p.PositionNs > o.positions[p.FilerId] {
			o.positions[p.FilerId] = p.PositionNs
		}
	}
	mc.Unlock()
	mc.signalAppliedAdvanced()
}

// DirectoryPending returns the positions in want that dir's cached listing is
// not yet current through, and whether the listing has a basis at all; without
// one nothing can be judged current.
func (mc *MetaCache) DirectoryPending(dir util.FullPath, want []*filer_pb.FilerDirectoryPosition) (basis bool, pending []*filer_pb.FilerDirectoryPosition) {
	mc.RLock()
	defer mc.RUnlock()
	o := mc.dirChangesApplied[dir]
	if o == nil || !o.basis {
		return false, want
	}
	for _, p := range want {
		if o.positions[p.FilerId] < p.PositionNs {
			pending = append(pending, p)
		}
	}
	return true, pending
}

// DirectoryGeneration returns the generation of dir's cached listing, which
// moves on every change applied to it and on every rebuild.
func (mc *MetaCache) DirectoryGeneration(dir util.FullPath) uint64 {
	mc.RLock()
	defer mc.RUnlock()
	if o := mc.dirChangesApplied[dir]; o != nil {
		return o.generation
	}
	return 0
}

// AwaitDirectoryPositions waits until dir's cached listing is current through
// every position in want, reporting false if ctx ends first.
func (mc *MetaCache) AwaitDirectoryPositions(ctx context.Context, dir util.FullPath, want []*filer_pb.FilerDirectoryPosition) bool {
	for {
		advanced := mc.appliedAdvancedChan()
		if basis, pending := mc.DirectoryPending(dir, want); basis && len(pending) == 0 {
			return true
		}
		select {
		case <-advanced:
		case <-ctx.Done():
			return false
		case <-mc.applyDone:
			return false
		}
	}
}

func (mc *MetaCache) appliedAdvancedChan() <-chan struct{} {
	mc.appliedMu.Lock()
	defer mc.appliedMu.Unlock()
	if mc.appliedAdvanced == nil {
		mc.appliedAdvanced = make(chan struct{})
	}
	return mc.appliedAdvanced
}

func (mc *MetaCache) signalAppliedAdvanced() {
	mc.appliedMu.Lock()
	defer mc.appliedMu.Unlock()
	if mc.appliedAdvanced != nil {
		close(mc.appliedAdvanced)
		mc.appliedAdvanced = nil
	}
}
