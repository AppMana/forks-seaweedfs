package mount

import (
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/topology"
)

// A chunk upload attempt must outlast the volume server's replica write
// timeout for the same chunk, so a stalled replica fails the primary first
// and the client learns which server to avoid. It must also end within half
// the 60 s FUSE request deadline, leaving the rest for an attempt elsewhere.
func TestChunkUploadAttemptTimeoutOrdering(t *testing.T) {
	const fuseRequestDeadline = 60 * time.Second
	for _, chunkSize := range []int64{2 << 20, 4 << 20, 8 << 20, 64 << 20} {
		attempt := chunkUploadAttemptTimeout(chunkSize)
		replica := topology.ReplicaWriteTimeout(int(chunkSize))
		if attempt <= replica {
			t.Errorf("%d MiB chunk: attempt timeout %v does not outlast the replica write timeout %v", chunkSize>>20, attempt, replica)
		}
		if attempt > fuseRequestDeadline/2 {
			t.Errorf("%d MiB chunk: attempt timeout %v leaves no room for a second attempt within %v", chunkSize>>20, attempt, fuseRequestDeadline)
		}
	}
}
