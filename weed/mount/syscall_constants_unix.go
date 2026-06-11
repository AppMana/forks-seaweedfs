//go:build !windows

package mount

import "syscall"

const (
	fRdlck   = syscall.F_RDLCK
	fWrlck   = syscall.F_WRLCK
	fUnlck   = syscall.F_UNLCK
	oAccmode = syscall.O_ACCMODE
)
