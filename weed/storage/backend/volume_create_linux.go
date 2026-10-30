//go:build linux

package backend

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// PreallocateSupported reports whether CreateVolumeFile reserves the
// requested size on this platform.
const PreallocateSupported = true

func CreateVolumeFile(fileName string, preallocate int64, memoryMapSizeMB uint32) (BackendStorageFile, error) {
	file, e := OpenVolumeFile(fileName, os.O_RDWR|os.O_CREATE|os.O_EXCL)
	if e != nil {
		return nil, e
	}
	if preallocate != 0 {
		if err := unix.Fallocate(int(file.Fd()), unix.FALLOC_FL_KEEP_SIZE, 0, preallocate); err != nil {
			cleanupErr := errors.Join(file.Close(), os.Remove(fileName))
			return nil, errors.Join(
				fmt.Errorf("preallocate %d bytes for %s: %w", preallocate, fileName, err),
				cleanupErr,
			)
		}
		glog.V(1).Infof("Preallocated %d bytes disk space for %s", preallocate, fileName)
	}
	return NewDiskFile(file), nil
}
