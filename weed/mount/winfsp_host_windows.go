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
func NewWinFspHost(wfs *WFS) *WinFspHost {
	host := cgofuse.NewFileSystemHost(newWinfspFS(wfs))
	// WinFsp-only optimization: Readdir fills full stats, so the FSD can
	// answer directory queries without per-entry Getattr round trips.
	host.SetCapReaddirPlus(true)
	// The filer namespace is case-sensitive; declaring the volume
	// case-insensitive would require fold-on-lookup which the adapter
	// does not implement.
	host.SetCapCaseInsensitive(false)
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
		// Bound attribute/directory staleness like the Linux mount's
		// attrValidSec (milliseconds).
		"-o", "FileInfoTimeout=1000",
		"-o", "DirInfoTimeout=1000",
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
