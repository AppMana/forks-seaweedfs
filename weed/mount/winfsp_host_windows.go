package mount

import (
	"fmt"

	cgofuse "github.com/winfsp/cgofuse/fuse"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// WinFspHost serves a WFS through WinFsp. Mount blocks until the
// filesystem is unmounted.
type WinFspHost struct {
	host *cgofuse.FileSystemHost
}

// NewWinFspHost wraps wfs in the cgofuse adapter.
func NewWinFspHost(wfs *WFS, caseSensitive bool) *WinFspHost {
	host := cgofuse.NewFileSystemHost(newWinfspFS(wfs, caseSensitive))
	// WinFsp-only optimization: Readdir fills full stats, so the FSD can
	// answer directory queries without per-entry Getattr round trips.
	host.SetCapReaddirPlus(true)
	host.SetCapCaseInsensitive(winFspCaseInsensitive(caseSensitive))
	go logWinfspStatsLoop()
	return &WinFspHost{host: host}
}

// Mount mounts at dir (which must not exist; WinFsp creates the mount
// point) and blocks until unmount. volumeLabel is shown in Explorer.
func (h *WinFspHost) Mount(dir string, volumeLabel string, extraOptions []string) error {
	options := []string{
		// Map all files to the mounting user (SYSTEM under HostProcess)
		// instead of translating uid/gid to SIDs.
		"-o", "uid=-1,gid=-1",
		"-o", "umask=000",
		// Present an Everyone-full-access DACL for every file. Without
		// this, files created by a pod user get mode-derived DACLs owned
		// by the mounting user (SYSTEM), and reopening them for write
		// from the pod fails with access denied. Mirrors the Linux CSI
		// mount's -umask=000 (any pod uid can use the volume).
		"-o", "FileSecurity=D:P(A;;FA;;;WD)",
		"-o", fmt.Sprintf("volname=%s", volumeLabel),
		// FileInfoTimeout=-1 would engage the NT cache manager for file
		// DATA (40-90x on warm/small reads, measured), but it also
		// DEFERS the FUSE unlink past DeleteFile() return for files
		// whose data the cache holds: a delete-then-recreate of the
		// same name (pwsh7 Move-Item -Force, compilers, any replace
		// pattern) then races a real "file exists" collision ~50% of
		// the time (verified: at the failure instant the backend still
		// has the file, so this is deferred delete, not stale cache;
		// FspFileSystemNotify does not help). Default to the safe
		// finite timeout; read-mostly volumes can opt into -1 via
		// -winfspOptions=FileInfoTimeout=-1 (SEAWEEDFS_WINFSP_OPTIONS
		// on the CSI DaemonSet).
		"-o", "FileInfoTimeout=1000",
		// Directory listings: bounded staleness, mirrors the Linux
		// mount's entryValidSec (milliseconds).
		"-o", "DirInfoTimeout=2000",
		"-o", "VolumeInfoTimeout=5000",
		"-o", "FileSystemName=seaweedfs",
	}
	options = append(options, extraOptions...)
	glog.V(0).Infof("winfsp mount %s (volume %q) options %v", dir, volumeLabel, options)
	if !h.host.Mount(dir, options) {
		return fmt.Errorf("winfsp mount at %s failed (is WinFsp installed?)", dir)
	}
	return nil
}

// Unmount asks WinFsp to unmount; safe to call from another goroutine.
func (h *WinFspHost) Unmount() bool {
	return h.host.Unmount()
}

// Notify invalidates kernel caches for path ('/'-separated, relative to
// the mount root) after an externally observed change. action is a
// cgofuse NOTIFY_* bitmask. Safe to call before the filesystem is
// mounted (returns false).
func (h *WinFspHost) Notify(path string, action uint32) bool {
	return h.host.Notify(path, action)
}

// cgofuse NOTIFY_* re-exports so callers outside this file do not need
// a cgofuse import.
const (
	NotifyMkdir    = cgofuse.NOTIFY_MKDIR
	NotifyRmdir    = cgofuse.NOTIFY_RMDIR
	NotifyCreate   = cgofuse.NOTIFY_CREATE
	NotifyUnlink   = cgofuse.NOTIFY_UNLINK
	NotifyChmod    = cgofuse.NOTIFY_CHMOD
	NotifyChown    = cgofuse.NOTIFY_CHOWN
	NotifyUtime    = cgofuse.NOTIFY_UTIME
	NotifyTruncate = cgofuse.NOTIFY_TRUNCATE
)
