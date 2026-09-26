package mount

import (
	"fmt"
	"strings"

	cgofuse "github.com/winfsp/cgofuse/fuse"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
)

// WinFspHost serves a WFS through WinFsp. Mount blocks until the
// filesystem is unmounted.
type WinFspHost struct {
	host             *cgofuse.FileSystemHost
	basicPermissions bool
}

// NewWinFspHost wraps wfs in the cgofuse adapter.
func NewWinFspHost(wfs *WFS, caseSensitive, basicPermissions bool) *WinFspHost {
	// WinFsp checks the caller's Windows token against the mode-derived
	// security descriptor before dispatching Open/Create. Rechecking with
	// the adapter's synthetic Unix identity would reject authorized backup/
	// restore operations. This skips only the redundant Unix access checks;
	// retention, quota and parent-existence checks remain in WFS. No extra
	// permission RPC or per-read/per-write check is introduced.
	if basicPermissions {
		wfs.option.DefaultPermissions = true
	}
	adapter := newWinfspFS(wfs, caseSensitive)
	adapter.basicPermissions = basicPermissions
	host := cgofuse.NewFileSystemHost(adapter)
	// WinFsp-only optimization: Readdir fills full stats, so the FSD can
	// answer directory queries without per-entry Getattr round trips.
	host.SetCapReaddirPlus(true)
	host.SetCapCaseInsensitive(winFspCaseInsensitive(caseSensitive))
	wfs.SetEntryChangeListener(func(event meta_cache.EntryInvalidation) {
		for _, signature := range event.Signatures {
			if signature == wfs.signature {
				return
			}
		}
		if inode, found := wfs.inodeToPath.GetInode(event.Path); found {
			adapter.readAhead.Invalidate(inode)
		}
		if event.Entry == nil || event.Entry.IsDirectory {
			return
		}
		root := strings.TrimRight(wfs.option.FilerMountRootPath, "/")
		path := string(event.Path)
		if !strings.HasPrefix(path, root+"/") {
			return
		}
		// The earlier subscription notification can race handle refresh.
		// Re-notify only after our read-ahead generation and WFS entry advance.
		if !host.Notify(strings.TrimPrefix(path, root), NotifyTruncate|NotifyUtime) {
			glog.V(4).Infof("winfsp post-refresh notify rejected for %s", path)
		}
	})
	go logWinfspStatsLoop()
	return &WinFspHost{host: host, basicPermissions: basicPermissions}
}

// Mount mounts at dir (which must not exist; WinFsp creates the mount
// point) and blocks until unmount. volumeLabel is shown in Explorer.
func (h *WinFspHost) Mount(dir string, volumeLabel string, extraOptions []string) error {
	if h.basicPermissions {
		if err := validateWinFspBasicOptions(extraOptions); err != nil {
			return err
		}
	}
	options := []string{
		// Preserve WinFsp's stored uid/gid/mode translation. Global uid/gid
		// overrides can assign another identity's mode bits to the caller
		// (e.g. an Administrators-owned, SYSTEM-group 0570 directory).
		// The synthetic root alone gets a local owner during adapter Init.
		// A fixed FileSecurity DACL or umask=000 would hide restrictions.
		// This opt-in mode is basic access control, not arbitrary ACL storage.
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
	if !h.basicPermissions {
		// Preserve existing shared-volume behavior on upgrades. Old metadata
		// may have restrictive modes previously hidden by these overrides;
		// silently enforcing them would make intact data inaccessible.
		options = append(options, "-o", "uid=-1,gid=-1", "-o", "umask=000",
			"-o", "FileSecurity=D:P(A;;FA;;;WD)")
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
