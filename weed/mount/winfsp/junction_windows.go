package winfsp

import (
	"fmt"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// WinFsp's mount-manager directory mount (\\.\C:\dir) writes a junction that
// targets the volume's NT device name (\Device\Volume{X}\). The mount manager
// later loses that directory's mapping, after which
// GetFinalPathNameByHandle(VOLUME_NAME_DOS) under the mount fails with
// ERROR_PATH_NOT_FOUND; git-lfs canonicalizes its .git directory that way and
// fails. In the Windows lab a junction targeting the same volume's
// mount-manager GUID path (\??\Volume{Y}\) kept every DOS lookup working over
// 64 mount cycles, while a matched rewrite back to the NT name still failed
// (test/storage_lab/README.md). WinFsp itself removes the junction at unmount.

// GetFinalPathNameByHandle flags (fileapi.h); x/sys/windows defines only
// VOLUME_NAME_DOS and VOLUME_NAME_NONE.
const (
	volumeNameGUID = 0x1
	volumeNameNT   = 0x2
)

// RetargetDirectoryMountToVolumeGUID waits up to timeout for dir to become
// the root of a mounted volume, then points its junction at that volume's
// mount-manager GUID path. It returns the old and new junction targets.
func RetargetDirectoryMountToVolumeGUID(dir string, timeout time.Duration) (from, to string, err error) {
	deadline := time.Now().Add(timeout)
	for {
		from, to, err = retargetDirectoryMount(dir)
		if err == nil || time.Now().After(deadline) {
			return from, to, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func retargetDirectoryMount(dir string) (string, string, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return "", "", err
	}
	// Opened through the junction, this is the mounted volume's root.
	root, err := windows.CreateFile(p, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", "", fmt.Errorf("open mount root %s: %w", dir, err)
	}
	defer windows.CloseHandle(root)
	guid, err := finalPathName(root, volumeNameGUID)
	if err != nil {
		return "", "", fmt.Errorf("volume GUID path of %s: %w", dir, err)
	}
	device, err := finalPathName(root, volumeNameNT)
	if err != nil {
		return "", "", fmt.Errorf("volume device path of %s: %w", dir, err)
	}
	if !strings.HasPrefix(guid, `\\?\Volume{`) || !strings.HasSuffix(guid, `}\`) || !strings.HasPrefix(device, `\Device\Volume{`) {
		return "", "", fmt.Errorf("%s is not yet the root of a mounted volume (GUID %q, device %q)", dir, guid, device)
	}

	junction, err := windows.CreateFile(p, windows.FILE_WRITE_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", "", fmt.Errorf("open junction %s: %w", dir, err)
	}
	defer windows.CloseHandle(junction)
	current, err := readMountPointTarget(junction)
	if err != nil {
		return "", "", fmt.Errorf("read junction %s: %w", dir, err)
	}
	target := `\??\` + strings.TrimPrefix(guid, `\\?\`)
	if strings.EqualFold(current, target) {
		return current, target, nil
	}
	// Only rewrite the junction WinFsp created for this very volume.
	if !strings.EqualFold(current, device) {
		return current, "", fmt.Errorf("junction %s targets %q, not the mounted device %q", dir, current, device)
	}
	buf := mountPointReparseBuffer(target)
	var n uint32
	if err := windows.DeviceIoControl(junction, windows.FSCTL_SET_REPARSE_POINT, &buf[0], uint32(len(buf)), nil, 0, &n, nil); err != nil {
		return current, "", fmt.Errorf("retarget junction %s: %w", dir, err)
	}
	readback, err := readMountPointTarget(junction)
	if err != nil {
		return current, "", fmt.Errorf("read back junction %s: %w", dir, err)
	}
	if readback != target {
		return current, "", fmt.Errorf("junction %s reads back %q after writing %q", dir, readback, target)
	}
	return current, target, nil
}

func finalPathName(h windows.Handle, flags uint32) (string, error) {
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
	if err != nil {
		return "", err
	}
	if n == 0 || n >= uint32(len(buf)) {
		return "", fmt.Errorf("final path name needs %d characters", n)
	}
	return windows.UTF16ToString(buf[:n]), nil
}

func readMountPointTarget(junction windows.Handle) (string, error) {
	buf := make([]byte, windows.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var n uint32
	if err := windows.DeviceIoControl(junction, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &buf[0], uint32(len(buf)), &n, nil); err != nil {
		return "", err
	}
	return mountPointReparseTarget(buf[:n])
}
