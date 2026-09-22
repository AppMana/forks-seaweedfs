//go:build linux

package backend

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

func CreateVolumeFile(fileName string, preallocate int64, memoryMapSizeMB uint32) (BackendStorageFile, error) {
	file, e := OpenVolumeFile(fileName, os.O_RDWR|os.O_CREATE|os.O_EXCL)
	if e != nil {
		return nil, e
	}
	if preallocate != 0 {
		if err := syscall.Fallocate(int(file.Fd()), 1, 0, preallocate); err != nil {
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
