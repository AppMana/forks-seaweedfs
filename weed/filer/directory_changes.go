package filer

import (
	"slices"
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

// directoryChangeWriters bounds how many distinct writers a filer remembers
// per directory. Past it the least recent one is forgotten into the
// directory's own floor.
const directoryChangeWriters = 8

// writerChange is the newest change one writer made to a directory's
// children: its position in this filer's log and the event's signatures.
type writerChange struct {
	positionNs int64
	signatures []int32
}

type directoryWriters struct {
	recent    []writerChange // least recent first, one per signature set
	forgotten int64          // newest position forgotten from recent
}

// directoryWriterChanges remembers, per directory and per writer, the log
// position of the newest change this filer appended to the directory's
// children. Positions are the filer's log buffer positions, which rise
// strictly in append order, so a reader that has applied this filer's events
// up to a position holds every change the filer appended before it. Changes
// are told apart by their signatures so that a caller can leave out its own,
// which its metadata stream never delivers.
type directoryWriterChanges struct {
	sync.Mutex
	dirs    map[util.FullPath]*directoryWriters
	floorNs int64
}

func (d *directoryWriterChanges) note(positionNs int64, signatures []int32, dirs ...util.FullPath) {
	d.Lock()
	defer d.Unlock()
	if d.dirs == nil {
		d.dirs = make(map[util.FullPath]*directoryWriters)
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		w, found := d.dirs[dir]
		if !found {
			if len(d.dirs) >= directoryChangesLimit {
				d.forgetAllLocked()
			}
			w = &directoryWriters{}
			d.dirs[dir] = w
		}
		for i, c := range w.recent {
			if slices.Equal(c.signatures, signatures) {
				w.recent = append(w.recent[:i], w.recent[i+1:]...)
				break
			}
		}
		if len(w.recent) >= directoryChangeWriters {
			w.forgotten = max(w.forgotten, w.recent[0].positionNs)
			w.recent = w.recent[1:]
		}
		w.recent = append(w.recent, writerChange{positionNs: positionNs, signatures: signatures})
	}
}

func (d *directoryWriterChanges) forgetAllLocked() {
	for _, w := range d.dirs {
		d.floorNs = max(d.floorNs, w.forgotten)
		for _, c := range w.recent {
			d.floorNs = max(d.floorNs, c.positionNs)
		}
	}
	d.dirs = make(map[util.FullPath]*directoryWriters)
}

// position returns the newest change to dir's children not carrying the
// caller's signature (0 leaves nothing out), and whether it is a change this
// filer remembers rather than a floor at or above every change it forgot.
func (d *directoryWriterChanges) position(dir util.FullPath, caller int32) (positionNs int64, remembered bool) {
	d.Lock()
	defer d.Unlock()
	w := d.dirs[dir]
	if w != nil {
		for i := len(w.recent) - 1; i >= 0; i-- {
			c := w.recent[i]
			if caller == 0 || !slices.Contains(c.signatures, caller) {
				// recent is least recent first and everything forgotten is
				// older, so this is the newest such change
				if c.positionNs > d.floorNs {
					return c.positionNs, true
				}
				break
			}
		}
	}
	floor := d.floorNs
	if w != nil {
		floor = max(floor, w.forgotten)
	}
	return floor, false
}

// DirectoryWriterPosition reports the newest change this filer appended to
// dir's children that does not carry the caller's signature, in this filer's
// log positions (see directoryWriterChanges).
func (f *Filer) DirectoryWriterPosition(dir util.FullPath, caller int32) (positionNs int64, remembered bool) {
	return f.dirWriters.position(dir, caller)
}
