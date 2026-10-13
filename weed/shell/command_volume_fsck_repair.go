package shell

import (
	"context"
	"fmt"
	"sort"

	"github.com/seaweedfs/seaweedfs/weed/operation"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle_map"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// repairMissingFromReplicas restores, on each replica, the filer-referenced
// needles it lacks, copying the exact blob from a replica that holds the
// needle live. A filer reference older than the cutoff is the proof that the
// absence is a missed write, not a vacuumed delete, which volume.check.disk
// cannot establish once a replica has been compacted. Writes go through
// WriteNeedleBlobIfAbsent, which refuses any existing entry, tombstones
// included, so nothing is overwritten or undeleted.
func (c *commandVolumeFsck) repairMissingFromReplicas(dataNodeVolumeIdToVInfo map[string]map[uint32]VInfo) (repaired, lost int, err error) {
	type replica struct {
		dataNodeId string
		info       VInfo
	}
	byVolume := make(map[uint32][]replica)
	for dataNodeId, volumes := range dataNodeVolumeIdToVInfo {
		for vid, info := range volumes {
			byVolume[vid] = append(byVolume[vid], replica{dataNodeId, info})
		}
	}
	vids := make([]uint32, 0, len(byVolume))
	for vid := range byVolume {
		vids = append(vids, vid)
	}
	sort.Slice(vids, func(i, j int) bool { return vids[i] < vids[j] })

	for _, vid := range vids {
		replicas := byVolume[vid]
		sort.Slice(replicas, func(i, j int) bool { return replicas[i].dataNodeId < replicas[j].dataNodeId })
		if replicas[0].info.isEcVolume {
			fmt.Fprintf(c.writer, "volume %d is erasure coded; not repairing\n", vid)
			continue
		}
		dbs := make([]*needle_map.MemDb, len(replicas))
		for i, r := range replicas {
			dbs[i] = needle_map.NewMemDb()
			defer dbs[i].Close()
			if err = dbs[i].LoadFromIdx(getVolumeFileIdFile(c.tempFolder, r.dataNodeId, vid)); err != nil {
				return repaired, lost, fmt.Errorf("load volume %d index of %s: %w", vid, r.dataNodeId, err)
			}
		}
		err = c.readFilerFileIdFile(vid, func(needleId types.NeedleId, itemPath util.FullPath) error {
			var source *needle_map.NeedleValue
			var sourceServer pb.ServerAddress
			var lacking []int
			for i, db := range dbs {
				if value, found := db.Get(needleId); found {
					if source == nil {
						source, sourceServer = value, replicas[i].info.server
					}
				} else {
					lacking = append(lacking, i)
				}
			}
			if len(lacking) == 0 {
				return nil
			}
			if source == nil {
				lost++
				fmt.Fprintf(c.writer, "%d,%x %s: on no replica, cannot repair\n", vid, uint64(needleId), itemPath)
				return nil
			}
			blob, readErr := c.readRepairBlob(sourceServer, vid, *source)
			if readErr != nil {
				return fmt.Errorf("read %d,%x from %s: %w", vid, uint64(needleId), sourceServer, readErr)
			}
			for _, i := range lacking {
				target := replicas[i]
				if target.info.isReadOnly {
					fmt.Fprintf(c.writer, "%d,%x %s: %s is read-only, not repaired\n", vid, uint64(needleId), itemPath, target.info.server)
					continue
				}
				writeErr := c.writeRepairBlob(target.info.server, vid, *source, blob)
				if status.Code(writeErr) == codes.AlreadyExists {
					writeErr = nil
				} else if writeErr != nil {
					return fmt.Errorf("repair %d,%x on %s: %w", vid, uint64(needleId), target.info.server, writeErr)
				} else {
					repaired++
					fmt.Fprintf(c.writer, "%d,%x %s: copied %s -> %s\n", vid, uint64(needleId), itemPath, sourceServer, target.info.server)
				}
				// A second reference to the same needle must not write it again.
				if setErr := dbs[i].Set(needleId, source.Offset, source.Size); setErr != nil {
					return setErr
				}
			}
			return nil
		})
		if err != nil {
			return repaired, lost, err
		}
	}
	return repaired, lost, nil
}

func (c *commandVolumeFsck) readRepairBlob(server pb.ServerAddress, volumeId uint32, n needle_map.NeedleValue) (blob []byte, err error) {
	if c.readNeedleBlob != nil {
		return c.readNeedleBlob(server, volumeId, n)
	}
	err = operation.WithVolumeServerClient(false, server, c.env.option.GrpcDialOption, func(client volume_server_pb.VolumeServerClient) error {
		resp, err := client.ReadNeedleBlob(context.Background(), &volume_server_pb.ReadNeedleBlobRequest{
			VolumeId: volumeId,
			Offset:   n.Offset.ToActualOffset(),
			Size:     int32(n.Size),
		})
		if err != nil {
			return err
		}
		blob = resp.NeedleBlob
		return nil
	})
	return
}

func (c *commandVolumeFsck) writeRepairBlob(server pb.ServerAddress, volumeId uint32, n needle_map.NeedleValue, blob []byte) error {
	if c.writeNeedleBlobIfAbsent != nil {
		return c.writeNeedleBlobIfAbsent(server, volumeId, n, blob)
	}
	return operation.WithVolumeServerClient(false, server, c.env.option.GrpcDialOption, func(client volume_server_pb.VolumeServerClient) error {
		_, err := client.WriteNeedleBlobIfAbsent(context.Background(), &volume_server_pb.WriteNeedleBlobRequest{
			VolumeId:   volumeId,
			NeedleId:   uint64(n.Key),
			Size:       int32(n.Size),
			NeedleBlob: blob,
		})
		return err
	})
}
