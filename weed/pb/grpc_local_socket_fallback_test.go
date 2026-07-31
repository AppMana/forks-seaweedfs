package pb

import (
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
)

// The fork registers local Unix sockets for in-process gRPC services (weed
// mini) so co-located components skip the TCP stack. On Windows -- and on any
// host where the socket path is unusable (missing directory, path too long for
// sockaddr_un, read-only filesystem) -- the bind fails. Upstream logged the
// error and returned, leaving the socket *registered*: every subsequent local
// dial then resolved to a Unix socket nobody was listening on, so the process
// could not talk to its own filer. The fork added UnregisterLocalGrpcSocket
// and calls it on bind failure so dials fall back to TCP.
//
// This is the single change that makes `weed mini` work on Windows at all, and
// it lives in a file (weed/pb/grpc_client_server.go) that upstream edits
// frequently -- a merge dropping the Unregister call would be invisible on
// Linux, where the bind always succeeds.

func TestServeGrpcOnLocalSocketFallsBackToTCPWhenBindFails(t *testing.T) {
	const port = 61987
	// A path under a directory that does not exist: net.Listen("unix", ...)
	// fails with ENOENT. This reproduces the Windows/unusable-path case
	// without needing a Windows host.
	unbindable := filepath.Join(t.TempDir(), "no-such-dir", "seaweedfs-test.sock")

	RegisterLocalGrpcSocket("127.0.0.1", port, unbindable)
	t.Cleanup(func() { UnregisterLocalGrpcSocket(port) })

	if got := GetLocalGrpcSocket(port); got != unbindable {
		t.Fatalf("precondition: GetLocalGrpcSocket(%d) = %q; want %q", port, got, unbindable)
	}
	if got := resolveLocalGrpcSocket("127.0.0.1:61987"); got != unbindable {
		t.Fatalf("precondition: local dial should resolve to the socket before the failed bind, got %q", got)
	}

	server := grpc.NewServer()
	t.Cleanup(server.Stop)
	ServeGrpcOnLocalSocket(server, port) // bind fails here

	if got := GetLocalGrpcSocket(port); got != "" {
		t.Fatalf("GetLocalGrpcSocket(%d) = %q after a failed bind; the socket must be unregistered so local dials fall back to TCP", port, got)
	}
	if got := resolveLocalGrpcSocket("127.0.0.1:61987"); got != "" {
		t.Fatalf("resolveLocalGrpcSocket = %q after a failed bind; want \"\" (TCP). A stale registration points every local dial at a socket nobody serves", got)
	}
}

// A successful bind must NOT unregister -- otherwise the optimization is
// disabled everywhere and the test above would pass vacuously.
func TestServeGrpcOnLocalSocketKeepsRegistrationWhenBindSucceeds(t *testing.T) {
	const port = 61988
	bindable := filepath.Join(t.TempDir(), "seaweedfs-test.sock")

	RegisterLocalGrpcSocket("127.0.0.1", port, bindable)
	t.Cleanup(func() { UnregisterLocalGrpcSocket(port) })

	server := grpc.NewServer()
	t.Cleanup(server.Stop)
	ServeGrpcOnLocalSocket(server, port)

	if got := GetLocalGrpcSocket(port); got != bindable {
		t.Fatalf("GetLocalGrpcSocket(%d) = %q after a successful bind; want %q to stay registered", port, got, bindable)
	}
}

func TestUnregisterLocalGrpcSocketClearsHostMatching(t *testing.T) {
	const port = 61989
	RegisterLocalGrpcSocket("10.0.0.5", port, "/tmp/seaweedfs-unregister-test.sock")

	// Every alias the registration installed must stop resolving, not just
	// the advertised host: localGrpcHosts and localGrpcSockets are separate
	// maps and clearing only one leaves resolveLocalGrpcSocket inconsistent.
	for _, host := range []string{"10.0.0.5", "", "localhost", "127.0.0.1", "::1"} {
		if got := resolveLocalGrpcSocket(hostPort(host, port)); got == "" {
			t.Fatalf("precondition: %q should resolve to the local socket while registered", hostPort(host, port))
		}
	}

	UnregisterLocalGrpcSocket(port)

	for _, host := range []string{"10.0.0.5", "", "localhost", "127.0.0.1", "::1"} {
		if got := resolveLocalGrpcSocket(hostPort(host, port)); got != "" {
			t.Fatalf("resolveLocalGrpcSocket(%q) = %q after Unregister; want \"\"", hostPort(host, port), got)
		}
	}
	if got := GetLocalGrpcSocket(port); got != "" {
		t.Fatalf("GetLocalGrpcSocket(%d) = %q after Unregister; want \"\"", port, got)
	}
}

func hostPort(host string, port int) string {
	if host == "::1" {
		return "[::1]:61989"
	}
	switch port {
	case 61989:
		return host + ":61989"
	}
	panic("unexpected port")
}
