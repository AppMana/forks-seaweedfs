package filer

import (
	"sync"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// directoryChangesLimit bounds how many directories a filer remembers the
// newest change of. Past it the filer forgets them all at once and answers
// with a floor instead, which costs each mount one re-listing per directory it
// then opens.
const directoryChangesLimit = 1 << 17

// directoryChanges remembers, per directory, the log timestamp of the newest
// change this filer stamped to the directory's children. A forgotten or never
// changed directory answers with the floor: the newest timestamp forgotten,
// or the time the filer started, below which it stamped nothing it knows of.
type directoryChanges struct {
	sync.Mutex
	positions map[util.FullPath]int64
	floorTsNs int64
}

func (d *directoryChanges) reset(floorTsNs int64) {
	d.Lock()
	defer d.Unlock()
	d.positions = nil
	d.floorTsNs = floorTsNs
}

func (d *directoryChanges) note(tsNs int64, dirs ...util.FullPath) {
	d.Lock()
	defer d.Unlock()
	if d.positions == nil {
		d.positions = make(map[util.FullPath]int64)
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if _, found := d.positions[dir]; !found && len(d.positions) >= directoryChangesLimit {
			for _, ts := range d.positions {
				if ts > d.floorTsNs {
					d.floorTsNs = ts
				}
			}
			d.positions = make(map[util.FullPath]int64)
		}
		if tsNs > d.positions[dir] {
			d.positions[dir] = tsNs
		}
	}
}

// position returns the newest change to dir's children and whether it is one
// this filer remembers rather than its floor.
func (d *directoryChanges) position(dir util.FullPath) (tsNs int64, remembered bool) {
	d.Lock()
	defer d.Unlock()
	if ts, found := d.positions[dir]; found && ts > d.floorTsNs {
		return ts, true
	}
	return d.floorTsNs, false
}

// ChangedDirectories lists the directories whose children an event changes:
// the parents it leaves and enters, and a directory it removes or moves, whose
// children go with it.
func ChangedDirectories(event *filer_pb.SubscribeMetadataResponse) []util.FullPath {
	message := event.GetEventNotification()
	if message == nil {
		return nil
	}
	var oldPath, newPath util.FullPath
	if message.NewEntry != nil {
		newDir := event.Directory
		if message.NewParentPath != "" {
			newDir = message.NewParentPath
		}
		newPath = util.NewFullPath(newDir, message.NewEntry.Name)
	}
	if message.OldEntry != nil {
		oldPath = util.NewFullPath(event.Directory, message.OldEntry.Name)
	}
	moved := message.OldEntry != nil && message.NewEntry != nil && oldPath != newPath
	var dirs []util.FullPath
	if message.OldEntry != nil {
		dirs = append(dirs, util.FullPath(event.Directory))
		if message.OldEntry.IsDirectory && (message.NewEntry == nil || moved) {
			dirs = append(dirs, oldPath)
		}
	}
	if message.NewEntry != nil {
		parent, _ := newPath.DirAndName()
		dirs = append(dirs, util.FullPath(parent))
		if message.NewEntry.IsDirectory && moved {
			dirs = append(dirs, newPath)
		}
	}
	return dirs
}

// DirectoryChangePosition reports the newest change this filer stamped to
// dir's children, and whether it remembers that change or answers its floor.
func (f *Filer) DirectoryChangePosition(dir util.FullPath) (tsNs int64, remembered bool) {
	return f.dirChanges.position(dir)
}
