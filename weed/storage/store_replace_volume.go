package storage

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// StagedVolumeFileName is the base file name a volume copy writes to before
// ReplaceVolume swaps it in. It sits in a directory beside the volume's files,
// which the volume scanner skips, so a partial copy is never loaded.
func StagedVolumeFileName(dir, collection string, id int) string {
	return siblingDirFileName(VolumeFileName(dir, collection, id), ".staging")
}

func replacedVolumeFileName(base string) string {
	return siblingDirFileName(base, ".replaced")
}

func siblingDirFileName(base, suffix string) string {
	return filepath.Join(base+suffix, filepath.Base(base))
}

// moveVolumeFiles renames each volume file of fromBase that exists to toBase.
func moveVolumeFiles(fromBase, toBase string) error {
	for _, ext := range volumeFileExts {
		from := fromBase + "." + ext
		if _, err := os.Lstat(from); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := os.Rename(from, toBase+"."+ext); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceVolume swaps the copy of vid staged at StagedVolumeFileName on
// location in for every local copy of vid, and mounts it with the validators.
// The old files are only moved aside until the new copy is mounted, so on any
// failure they are moved back and the old replica is mounted again.
func (s *Store) ReplaceVolume(vid needle.VolumeId, collection string, location *DiskLocation, validators ...func(*Volume) error) (err error) {
	dataBase := VolumeFileName(location.Directory, collection, int(vid))
	idxBase := VolumeFileName(location.IdxDirectory, collection, int(vid))

	bases := []string{dataBase}
	if idxBase != dataBase {
		bases = append(bases, idxBase)
	}
	addBase := func(base string) {
		for _, b := range bases {
			if b == base {
				return
			}
		}
		bases = append(bases, base)
	}
	keepVif := make(map[string]bool)
	var oldCollection *string
	for _, l := range s.Locations {
		v, found := l.FindVolume(vid)
		if !found {
			continue
		}
		addBase(v.DataFileName())
		addBase(v.IndexFileName())
		if v.sharesVifWithEcVolume() {
			keepVif[v.DataFileName()] = true
		}
		c := v.Collection
		oldCollection = &c
	}

	var moved []string
	stagedIn := false
	restore := func() {
		if stagedIn {
			removeVolumeFiles(dataBase, false)
			removeVolumeFiles(idxBase, false)
		}
		for _, base := range moved {
			backup := replacedVolumeFileName(base)
			if mvErr := moveVolumeFiles(backup, base); mvErr != nil {
				glog.Errorf("volume %d: restore replaced files from %s: %v", vid, filepath.Dir(backup), mvErr)
				continue
			}
			os.Remove(filepath.Dir(backup))
		}
		if oldCollection != nil {
			if mountErr := s.mountVolume(vid, oldCollection); mountErr != nil {
				glog.Errorf("volume %d: remount replaced replica: %v", vid, mountErr)
			}
		}
	}

	if err = s.UnmountVolume(vid); err != nil {
		restore()
		return fmt.Errorf("unmount volume %d: %w", vid, err)
	}
	for _, base := range bases {
		backupDir := filepath.Dir(replacedVolumeFileName(base))
		// A leftover backup may be the only complete copy after an interrupted
		// replacement. Preserve it for recovery instead of replacing it.
		err = os.Mkdir(backupDir, 0755)
		if err != nil {
			restore()
			return fmt.Errorf("volume %d: prepare %s: %w", vid, backupDir, err)
		}
		moved = append(moved, base)
		if err = moveVolumeFiles(base, replacedVolumeFileName(base)); err != nil {
			restore()
			return fmt.Errorf("volume %d: move aside %s: %w", vid, base, err)
		}
	}

	stagedIn = true
	if err = os.WriteFile(dataBase+".note", []byte("replacing with a copied volume"), 0644); err != nil {
		restore()
		return fmt.Errorf("volume %d: write .note: %w", vid, err)
	}
	if err = moveVolumeFiles(StagedVolumeFileName(location.Directory, collection, int(vid)), dataBase); err == nil {
		err = moveVolumeFiles(StagedVolumeFileName(location.IdxDirectory, collection, int(vid)), idxBase)
	}
	if err != nil {
		restore()
		return fmt.Errorf("volume %d: move copied files into place: %w", vid, err)
	}
	if err = os.Remove(dataBase + ".note"); err != nil {
		restore()
		return fmt.Errorf("volume %d: remove .note: %w", vid, err)
	}
	if err = s.mountVolume(vid, &collection, validators...); err != nil {
		restore()
		return err
	}

	for _, base := range moved {
		backup := replacedVolumeFileName(base)
		if keepVif[base] && !util.FileExists(base+".vif") {
			if mvErr := os.Rename(backup+".vif", base+".vif"); mvErr != nil {
				glog.Warningf("volume %d: keep EC volume info %s: %v", vid, base+".vif", mvErr)
			}
		}
		if rmErr := os.RemoveAll(filepath.Dir(backup)); rmErr != nil {
			glog.Warningf("volume %d: remove replaced files %s: %v", vid, filepath.Dir(backup), rmErr)
		}
	}
	return nil
}
