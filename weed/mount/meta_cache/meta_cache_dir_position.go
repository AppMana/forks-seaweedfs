package meta_cache

import (
	"context"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// A cached directory's listing is current through a log position: its listing
// snapshot, raised by every change to its children applied since. Comparing
// that position with the newest change the filers stamped to the directory
// tells whether the listing already shows a change another mount has made, or
// whether that change is still on its way through the metadata stream.

// noteChangesApplied raises the position of each cached directory whose
// children resp changed. Runs on the apply loop after the store took resp.
func (mc *MetaCache) noteChangesApplied(resp *filer_pb.SubscribeMetadataResponse) {
	if resp == nil || resp.TsNs == 0 {
		return
	}
	dirs := filer.ChangedDirectories(resp)
	if len(dirs) == 0 {
		return
	}
	advanced := false
	mc.Lock()
	for _, dir := range dirs {
		// An uncached directory reads through to the filer and needs no
		// position; recording one would only grow the map.
		if !mc.isCachedFn(dir) && !mc.isBuildingDir(dir) {
			continue
		}
		if resp.TsNs > mc.dirChangesApplied[dir] {
			mc.dirChangesApplied[dir] = resp.TsNs
			advanced = true
		}
	}
	mc.Unlock()
	if advanced {
		mc.signalAppliedAdvanced()
	}
}

// NoteDirectoryCurrentThrough records that dir's cached listing reflects every
// change up to tsNs, as a listing taken after a change was acknowledged does.
func (mc *MetaCache) NoteDirectoryCurrentThrough(dir util.FullPath, tsNs int64) {
	mc.Lock()
	if tsNs > mc.dirChangesApplied[dir] {
		mc.dirChangesApplied[dir] = tsNs
	}
	mc.Unlock()
	mc.signalAppliedAdvanced()
}

// DirectoryCurrentThrough returns the log position dir's cached listing is
// current through: the newer of its listing snapshot and its newest applied
// change.
func (mc *MetaCache) DirectoryCurrentThrough(dir util.FullPath) int64 {
	mc.RLock()
	defer mc.RUnlock()
	tsNs := mc.dirVersionFloors[dir]
	if applied := mc.dirChangesApplied[dir]; applied > tsNs {
		tsNs = applied
	}
	return tsNs
}

// AwaitDirectoryCurrentThrough waits until dir's cached listing is current
// through tsNs, reporting false if ctx ends first.
func (mc *MetaCache) AwaitDirectoryCurrentThrough(ctx context.Context, dir util.FullPath, tsNs int64) bool {
	for {
		advanced := mc.appliedAdvancedChan()
		if mc.DirectoryCurrentThrough(dir) >= tsNs {
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
