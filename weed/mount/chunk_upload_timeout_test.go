package mount

import (
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/operation"
	"github.com/seaweedfs/seaweedfs/weed/topology"
)

// A mount's chunk upload must wait longer for a response than a volume
// server takes to give up on a stalled replica, so the primary fails first
// and its 500 names the replica to avoid. And an upload that has stopped
// must be abandoned within half the 60 s FUSE request deadline, leaving the
// rest for an attempt elsewhere.
func TestChunkUploadTimeoutOrdering(t *testing.T) {
	const fuseRequestDeadline = 60 * 1e9
	if operation.MountUploadResponseTimeout <= topology.ReplicaWriteStallTimeout+topology.ReplicaWriteResponseTimeout {
		t.Errorf("mount response timeout %v does not outlast a replica forward's %v + %v",
			operation.MountUploadResponseTimeout, topology.ReplicaWriteStallTimeout, topology.ReplicaWriteResponseTimeout)
	}
	if operation.MountUploadResponseTimeout > fuseRequestDeadline/2 || operation.UploadStallTimeout > fuseRequestDeadline/2 {
		t.Errorf("a stopped upload is abandoned after up to %v, leaving no room for a second attempt within %v",
			operation.MountUploadResponseTimeout, fuseRequestDeadline)
	}
}
