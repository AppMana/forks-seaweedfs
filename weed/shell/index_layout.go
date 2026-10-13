package shell

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/operation"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/grpc"
)

// requireServerIndexLayout refuses to go on unless every server stores index
// entries at this shell's offset width. The version string starts with the
// size class (util.SizeLimit: "30GB" for 4-byte offsets, "8000GB" for the
// 5BytesOffset build), and a shell that disagrees misreads every .idx entry.
func requireServerIndexLayout(servers []pb.ServerAddress, version func(pb.ServerAddress) (string, error)) error {
	var mismatched []string
	for _, server := range servers {
		v, err := version(server)
		if err != nil {
			return fmt.Errorf("read version of %s to check its index layout: %w", server, err)
		}
		if class, _, _ := strings.Cut(v, " "); class != util.SizeLimit {
			mismatched = append(mismatched, fmt.Sprintf("%s (%s)", server, class))
		}
	}
	if len(mismatched) > 0 {
		sort.Strings(mismatched)
		return fmt.Errorf("this shell reads %s index entries, but %s use another offset size; rebuild the shell with the servers' build tags",
			util.SizeLimit, strings.Join(mismatched, ", "))
	}
	return nil
}

func volumeServerVersion(dialOption grpc.DialOption) func(pb.ServerAddress) (string, error) {
	return func(server pb.ServerAddress) (version string, err error) {
		err = operation.WithVolumeServerClient(false, server, dialOption, func(client volume_server_pb.VolumeServerClient) error {
			resp, err := client.VolumeServerStatus(context.Background(), &volume_server_pb.VolumeServerStatusRequest{})
			if err != nil {
				return err
			}
			version = resp.Version
			return nil
		})
		return
	}
}

func topologyVolumeServers(topology *master_pb.TopologyInfo) []pb.ServerAddress {
	var servers []pb.ServerAddress
	eachDataNode(topology, func(dc DataCenterId, rack RackId, dn *master_pb.DataNodeInfo) {
		servers = append(servers, pb.NewServerAddressFromDataNode(dn))
	})
	return servers
}
