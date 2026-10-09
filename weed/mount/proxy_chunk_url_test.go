package mount

import (
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb"
)

// A filer given with an explicit gRPC port (host:port.grpcPort) must still be
// reached on its HTTP port by chunk uploads and reads through the proxy.
func TestFilerProxyChunkUrlUsesTheFilerHttpAddress(t *testing.T) {
	wfs := &WFS{option: &Option{FilerAddresses: []pb.ServerAddress{"127.0.0.1:8888.19999"}}}
	if got, want := wfs.filerProxyChunkUrl("3,01637037d6"), "http://127.0.0.1:8888/?proxyChunkId=3,01637037d6"; got != want {
		t.Fatalf("proxy chunk url = %q, want %q", got, want)
	}
}
