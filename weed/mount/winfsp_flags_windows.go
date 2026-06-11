package mount

import (
	"os"

	cgofuse "github.com/winfsp/cgofuse/fuse"
)

// toGoOpenFlags translates cgofuse open flags (MSVC-style values defined
// in cgofuse's fsop_nocgo_windows.go) to the Go os package flag values
// that the WFS handlers and go-fuse's O_ANYWRITE mask expect. The Go os
// package uses the Linux-style values on every GOOS, including Windows.
func toGoOpenFlags(flags int) uint32 {
	var out int
	switch flags & cgofuse.O_ACCMODE {
	case cgofuse.O_RDONLY:
		out = os.O_RDONLY
	case cgofuse.O_WRONLY:
		out = os.O_WRONLY
	case cgofuse.O_RDWR:
		out = os.O_RDWR
	}
	if flags&cgofuse.O_APPEND != 0 {
		out |= os.O_APPEND
	}
	if flags&cgofuse.O_CREAT != 0 {
		out |= os.O_CREATE
	}
	if flags&cgofuse.O_TRUNC != 0 {
		out |= os.O_TRUNC
	}
	if flags&cgofuse.O_EXCL != 0 {
		out |= os.O_EXCL
	}
	return uint32(out)
}
