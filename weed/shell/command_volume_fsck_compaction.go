package shell

import (
	"context"
	"fmt"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/operation"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
)

// fsckIndexRecopyAttempts bounds how often one replica's index is copied
// again, each time because the volume was compacted since the last copy.
const fsckIndexRecopyAttempts = 3

// The master's periodic vacuum is not stopped by the shell lock, so a volume
// can be compacted between its index copy and the needle metadata reads that
// follow the filer walk. Compaction moves needles, leaving the copied
// offsets stale. Each index is therefore copied pinned to the compaction
// revision it was read at, and that revision is kept to tell a compaction
// apart from any other read failure.

// copyVolumeIndexAtRevision copies a volume's index at its current
// compaction revision and records that revision.
func (c *commandVolumeFsck) copyVolumeIndexAtRevision(dataNodeId string, volumeId uint32, vinfo VInfo) (err error) {
	for attempt := 0; attempt <= fsckIndexRecopyAttempts; attempt++ {
		var revision uint32
		if revision, err = c.volumeCompactionRevision(vinfo.server, volumeId); err != nil {
			return fmt.Errorf("read compaction revision of volume %d: %w", volumeId, err)
		}
		if err = c.copyVolumeIndex(dataNodeId, volumeId, vinfo, revision); err == nil {
			c.recordIndexRevision(dataNodeId, volumeId, revision)
			return nil
		}
		// compacted between the revision read and the copy
		if !strings.Contains(err.Error(), fmt.Sprintf("volume %d is compacted", volumeId)) {
			return err
		}
	}
	return err
}

// recopyCompactedIndex copies a replica's index again if, and only if, the
// volume was compacted since its index was copied. Otherwise it returns why
// a fresh copy would not explain the failed reads.
func (c *commandVolumeFsck) recopyCompactedIndex(dataNodeId string, volumeId uint32, vinfo VInfo) error {
	if vinfo.isEcVolume {
		return fmt.Errorf("erasure coded volume")
	}
	copied, found := c.copiedIndexRevision(dataNodeId, volumeId)
	if !found {
		return fmt.Errorf("no compaction revision recorded for the copied index")
	}
	current, err := c.volumeCompactionRevision(vinfo.server, volumeId)
	if err != nil {
		return fmt.Errorf("read compaction revision: %w", err)
	}
	if current == copied {
		return fmt.Errorf("compaction revision %d unchanged since the index was copied", copied)
	}
	fmt.Fprintf(c.writer, "dataNode:%s\tvolume:%d\tcompacted since its index was copied (revision %d, now %d); copying it again\n",
		dataNodeId, volumeId, copied, current)
	return c.copyVolumeIndexAtRevision(dataNodeId, volumeId, vinfo)
}

func (c *commandVolumeFsck) volumeCompactionRevision(server pb.ServerAddress, volumeId uint32) (revision uint32, err error) {
	err = operation.WithVolumeServerClient(false, server, c.env.option.GrpcDialOption, func(client volume_server_pb.VolumeServerClient) error {
		resp, statusErr := client.ReadVolumeFileStatus(context.Background(), &volume_server_pb.ReadVolumeFileStatusRequest{VolumeId: volumeId})
		if statusErr != nil {
			return statusErr
		}
		revision = resp.CompactionRevision
		return nil
	})
	return
}

func (c *commandVolumeFsck) recordIndexRevision(dataNodeId string, volumeId uint32, revision uint32) {
	c.indexRevisionsLock.Lock()
	defer c.indexRevisionsLock.Unlock()
	if c.indexRevisions == nil {
		c.indexRevisions = make(map[string]map[uint32]uint32)
	}
	if c.indexRevisions[dataNodeId] == nil {
		c.indexRevisions[dataNodeId] = make(map[uint32]uint32)
	}
	c.indexRevisions[dataNodeId][volumeId] = revision
}

func (c *commandVolumeFsck) copiedIndexRevision(dataNodeId string, volumeId uint32) (revision uint32, found bool) {
	c.indexRevisionsLock.Lock()
	defer c.indexRevisionsLock.Unlock()
	revision, found = c.indexRevisions[dataNodeId][volumeId]
	return
}
