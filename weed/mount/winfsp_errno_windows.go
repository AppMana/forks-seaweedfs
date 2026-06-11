package mount

import (
	"syscall"

	cgofuse "github.com/winfsp/cgofuse/fuse"

	"github.com/seaweedfs/go-fuse/v2/fuse"
)

// winErrno maps go-fuse Status values (which wrap the Windows syscall
// package's Errno values when built with GOOS=windows) to the negated
// Linux-valued errno constants that cgofuse/WinFsp expect. The numeric
// values differ between the two worlds, so statuses must never be
// passed through arithmetically.
var winErrno map[fuse.Status]int

func init() {
	winErrno = make(map[fuse.Status]int)
	set := func(errno syscall.Errno, code int) {
		winErrno[fuse.Status(errno)] = -code
	}
	set(syscall.EPERM, cgofuse.EPERM)
	set(syscall.ENOENT, cgofuse.ENOENT)
	set(syscall.EINTR, cgofuse.EINTR)
	set(syscall.EIO, cgofuse.EIO)
	set(syscall.EBADF, cgofuse.EBADF)
	set(syscall.EAGAIN, cgofuse.EAGAIN)
	set(syscall.ENOMEM, cgofuse.ENOMEM)
	set(syscall.EACCES, cgofuse.EACCES)
	set(syscall.EBUSY, cgofuse.EBUSY)
	set(syscall.EEXIST, cgofuse.EEXIST)
	set(syscall.EXDEV, cgofuse.EXDEV)
	set(syscall.ENODEV, cgofuse.ENODEV)
	set(syscall.ENOTDIR, cgofuse.ENOTDIR)
	set(syscall.EISDIR, cgofuse.EISDIR)
	set(syscall.EINVAL, cgofuse.EINVAL)
	set(syscall.EFBIG, cgofuse.EFBIG)
	set(syscall.ENOSPC, cgofuse.ENOSPC)
	set(syscall.EROFS, cgofuse.EROFS)
	set(syscall.EMLINK, cgofuse.EMLINK)
	set(syscall.EPIPE, cgofuse.EPIPE)
	set(syscall.ERANGE, cgofuse.ERANGE)
	set(syscall.ENAMETOOLONG, cgofuse.ENAMETOOLONG)
	set(syscall.ENOSYS, cgofuse.ENOSYS)
	set(syscall.ENOTEMPTY, cgofuse.ENOTEMPTY)
	set(syscall.ENODATA, cgofuse.ENODATA)
	set(syscall.ENOTSUP, cgofuse.ENOTSUP)
	set(syscall.ETIMEDOUT, cgofuse.ETIMEDOUT)
	set(syscall.EDQUOT, cgofuse.ENOSPC) // no EDQUOT in cgofuse; quota -> no space
	// go-fuse statuses that do not wrap a syscall.Errno
	winErrno[fuse.OK] = 0
	winErrno[fuse.ENOATTR] = -cgofuse.ENOATTR
	winErrno[fuse.ENODATA] = -cgofuse.ENODATA
	winErrno[fuse.EREMOTEIO] = -cgofuse.EIO
}

// toWinErrno converts a go-fuse Status to a cgofuse return code.
func toWinErrno(st fuse.Status) int {
	if st == fuse.OK {
		return 0
	}
	if code, ok := winErrno[st]; ok {
		return code
	}
	return -cgofuse.EIO
}
