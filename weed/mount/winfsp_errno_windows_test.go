package mount

import (
	"syscall"
	"testing"

	cgofuse "github.com/winfsp/cgofuse/fuse"

	"github.com/seaweedfs/go-fuse/v2/fuse"
)

func TestToWinErrnoOK(t *testing.T) {
	if got := toWinErrno(fuse.OK); got != 0 {
		t.Fatalf("OK => %d, want 0", got)
	}
}

func TestToWinErrnoMappings(t *testing.T) {
	cases := []struct {
		name string
		st   fuse.Status
		want int
	}{
		{"ENOENT", fuse.Status(syscall.ENOENT), -cgofuse.ENOENT},
		{"EEXIST", fuse.Status(syscall.EEXIST), -cgofuse.EEXIST},
		{"EACCES", fuse.Status(syscall.EACCES), -cgofuse.EACCES},
		{"EPERM", fuse.Status(syscall.EPERM), -cgofuse.EPERM},
		{"EINVAL", fuse.Status(syscall.EINVAL), -cgofuse.EINVAL},
		{"EIO", fuse.Status(syscall.EIO), -cgofuse.EIO},
		{"ENOTDIR", fuse.Status(syscall.ENOTDIR), -cgofuse.ENOTDIR},
		{"EISDIR", fuse.Status(syscall.EISDIR), -cgofuse.EISDIR},
		{"ENOTEMPTY", fuse.Status(syscall.ENOTEMPTY), -cgofuse.ENOTEMPTY},
		{"ENOSPC", fuse.Status(syscall.ENOSPC), -cgofuse.ENOSPC},
		{"EAGAIN", fuse.Status(syscall.EAGAIN), -cgofuse.EAGAIN},
		{"ETIMEDOUT", fuse.Status(syscall.ETIMEDOUT), -cgofuse.ETIMEDOUT},
		{"ENOSYS", fuse.Status(syscall.ENOSYS), -cgofuse.ENOSYS},
		{"ENOTSUP", fuse.Status(syscall.ENOTSUP), -cgofuse.ENOTSUP},
		{"ENAMETOOLONG", fuse.Status(syscall.ENAMETOOLONG), -cgofuse.ENAMETOOLONG},
		{"EINTR", fuse.Status(syscall.EINTR), -cgofuse.EINTR},
		{"EBADF", fuse.Status(syscall.EBADF), -cgofuse.EBADF},
		{"EXDEV", fuse.Status(syscall.EXDEV), -cgofuse.EXDEV},
		{"EDQUOT->ENOSPC", fuse.Status(syscall.EDQUOT), -cgofuse.ENOSPC},
		{"ENODATA", fuse.ENODATA, -cgofuse.ENODATA},
		{"EREMOTEIO->EIO", fuse.EREMOTEIO, -cgofuse.EIO},
	}
	for _, c := range cases {
		if got := toWinErrno(c.st); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestToWinErrnoUnknownDefaultsToEIO(t *testing.T) {
	// A status value that no mapping covers must degrade to EIO, never 0
	// or a positive number.
	unknown := fuse.Status(998877)
	if got := toWinErrno(unknown); got != -cgofuse.EIO {
		t.Fatalf("unknown status => %d, want %d", got, -cgofuse.EIO)
	}
}

func TestWinErrnoValuesAreNegative(t *testing.T) {
	for st, code := range winErrno {
		if st == fuse.OK {
			continue
		}
		if code >= 0 {
			t.Errorf("status %v maps to non-negative %d", st, code)
		}
	}
}
