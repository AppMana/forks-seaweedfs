package weed_server

import (
	"context"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// DirectoryChangePosition answers with the newest change to the directory's
// children that any filer of the cluster stamped. Peers are asked directly
// rather than read from the aggregated stream: a change a peer has already
// acknowledged to its writer may not have reached this filer's stream yet,
// and that lag is exactly what the caller is asking about. A peer that cannot
// answer fails the call, so the caller reads the directory from the store
// instead of trusting a position that may be missing that peer's changes;
// Aborted tells it this filer itself is reachable for that read.
func (fs *FilerServer) DirectoryChangePosition(ctx context.Context, req *filer_pb.DirectoryChangePositionRequest) (*filer_pb.DirectoryChangePositionResponse, error) {
	dir := util.FullPath(req.Directory)
	tsNs, remembered := fs.filer.DirectoryChangePosition(dir)
	resp := &filer_pb.DirectoryChangePositionResponse{TsNs: tsNs, Remembered: remembered}
	if req.LocalOnly || fs.filer.MetaAggregator == nil {
		return resp, nil
	}
	peers := fs.filer.MetaAggregator.RemotePeers()
	if len(peers) == 0 {
		return resp, nil
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)
	for _, peer := range peers {
		wg.Add(1)
		go func(peer pb.ServerAddress) {
			defer wg.Done()
			err := pb.WithFilerClient(false, 0, peer, fs.grpcDialOption, func(client filer_pb.SeaweedFilerClient) error {
				peerResp, err := client.DirectoryChangePosition(ctx, &filer_pb.DirectoryChangePositionRequest{Directory: req.Directory, LocalOnly: true})
				if err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				if peerResp.TsNs > resp.TsNs {
					resp.TsNs, resp.Remembered = peerResp.TsNs, peerResp.Remembered
				} else if peerResp.TsNs == resp.TsNs && peerResp.Remembered {
					resp.Remembered = true
				}
				return nil
			})
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = status.Errorf(codes.Aborted, "peer filer %s: %v", peer, err)
				}
				mu.Unlock()
			}
		}(peer)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return resp, nil
}
