//go:build !linux

package operation

import "net"

// tcpBytesAcked is unavailable here; progress comes from body reads alone.
func tcpBytesAcked(conn net.Conn) (uint64, bool) {
	return 0, false
}
