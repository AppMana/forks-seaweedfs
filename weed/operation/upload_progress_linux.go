//go:build linux

package operation

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// tcpBytesAcked returns how many bytes the peer has acknowledged on conn, so
// a write whose body already sits in the kernel's send buffer still shows
// progress while the network drains it.
func tcpBytesAcked(conn net.Conn) (uint64, bool) {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return 0, false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var acked uint64
	var got bool
	raw.Control(func(fd uintptr) {
		info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if err == nil {
			acked, got = info.Bytes_acked, true
		}
	})
	return acked, got
}
