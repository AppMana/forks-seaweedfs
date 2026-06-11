package mount

// Linux fcntl(2) lock types and open(2) access-mode mask. The Windows
// syscall package does not define them; the FUSE wire protocol carries
// the Linux values.
const (
	fRdlck   = 0
	fWrlck   = 1
	fUnlck   = 2
	oAccmode = 0x3
)
