package mount

import (
	"context"
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// directoryCurrentWait bounds how long a listing or a negative lookup waits
// for the metadata stream to deliver a change the filers have already
// acknowledged, before reading the directory from the filer instead. The
// stream normally trails a change by a link latency plus the peer filers'
// delivery claims, a fraction of this.
var directoryCurrentWait = time.Second

// directoryPositions answers, per directory, whether the kernel's cached
// listing is still the cache's: OpenDir lets the kernel keep its listing only
// while no change has been applied since the open that let it read it.
type directoryPositions struct {
	sync.Mutex
	kernelListing map[uint64]uint64
	unsupported   bool
}

const directoryPositionsLimit = 1 << 16

// keepKernelListing reports whether the listing the kernel read after the
// last open of inode is still the cache's at generation, and records
// generation for the next open.
func (p *directoryPositions) keepKernelListing(inode uint64, generation uint64) bool {
	p.Lock()
	defer p.Unlock()
	if p.kernelListing == nil || len(p.kernelListing) >= directoryPositionsLimit {
		p.kernelListing = make(map[uint64]uint64)
	}
	kept, found := p.kernelListing[inode]
	p.kernelListing[inode] = generation
	return found && kept == generation
}

func (p *directoryPositions) dropKernelListing(inode uint64) {
	p.Lock()
	defer p.Unlock()
	delete(p.kernelListing, inode)
}

func (p *directoryPositions) isUnsupported() bool {
	p.Lock()
	defer p.Unlock()
	return p.unsupported
}

func (p *directoryPositions) markUnsupported() {
	p.Lock()
	defer p.Unlock()
	p.unsupported = true
}

var errDirectoryPositionUnsupported = errors.New("filer does not report directory change positions")

// directoryChangePosition asks the filers how far each one's log reaches for
// dir's children, leaving out this mount's own changes.
func (wfs *WFS) directoryChangePosition(ctx context.Context, dir util.FullPath) (*filer_pb.DirectoryChangePositionResponse, error) {
	if wfs.dirPositions.isUnsupported() {
		return nil, errDirectoryPositionUnsupported
	}
	resp, err := awaitFilerReply(ctx, "directory position "+string(dir), func() (*filer_pb.DirectoryChangePositionResponse, error) {
		var resp *filer_pb.DirectoryChangePositionResponse
		err := wfs.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
			var callErr error
			resp, callErr = client.DirectoryChangePosition(ctx, &filer_pb.DirectoryChangePositionRequest{Directory: string(dir), Signature: wfs.signature})
			return callErr
		})
		return resp, err
	})
	if status.Code(err) == codes.Unimplemented {
		glog.Warningf("filer does not report directory change positions; listings from other mounts' changes follow the metadata stream: %v", err)
		wfs.dirPositions.markUnsupported()
		return nil, errDirectoryPositionUnsupported
	}
	return resp, err
}

// directoryFilerPositions is directoryChangePosition for a directory build,
// bounded on its own; nil positions leave the build without a basis.
func (wfs *WFS) directoryFilerPositions(ctx context.Context, dir util.FullPath) ([]*filer_pb.FilerDirectoryPosition, error) {
	positionCtx, cancel := context.WithTimeout(ctx, directoryPositionTimeout)
	defer cancel()
	resp, err := wfs.directoryChangePosition(positionCtx, dir)
	if err != nil {
		return nil, err
	}
	return resp.GetFilers(), nil
}

// directoryPositionTimeout bounds the position request on its own: with the
// filer unreachable a cached listing is served as it is, as it was before
// positions existed, rather than holding the open for the request deadline.
var directoryPositionTimeout = 5 * time.Second

// directoryCurrency is what awaitDirectoryCurrent found.
type directoryCurrency struct {
	// cached: the directory is answered from the local cache; otherwise it
	// reads through to the filer.
	cached bool
	// verified: the cached listing shows every change the filers acknowledged
	// before the call. Unverified means the filer could not be asked, and the
	// cache is served as it is.
	verified bool
}

// awaitDirectoryCurrent makes a cached directory's listing show every change
// the filers acknowledged before the call: close-to-open across mounts. Each
// filer names how far its log reaches for the directory, leaving out this
// mount's own changes, and the listing is current once it holds every filer's
// log up to there. A change still on its way through the metadata stream is
// waited for; one the stream does not bring in time, a filer that names no
// change to wait for, or an answer without per-filer positions, has the
// directory read from the filer again.
func (wfs *WFS) awaitDirectoryCurrent(ctx context.Context, dir util.FullPath) directoryCurrency {
	if !wfs.metaCache.IsDirectoryCached(dir) {
		return directoryCurrency{}
	}
	positionCtx, cancel := context.WithTimeout(ctx, directoryPositionTimeout)
	resp, err := wfs.directoryChangePosition(positionCtx, dir)
	cancel()
	if errors.Is(err, errDirectoryPositionUnsupported) {
		return directoryCurrency{cached: true, verified: true}
	}
	if status.Code(err) == codes.Aborted {
		glog.V(1).Infof("directory position %s: %v; reading it from the filer", dir, err)
		return wfs.relistDirectory(ctx, dir, nil)
	}
	if err != nil {
		glog.V(1).Infof("directory position %s: %v; serving the cached listing", dir, err)
		return directoryCurrency{cached: true}
	}
	if len(resp.Filers) == 0 {
		// a filer predating per-filer positions: its single newest timestamp
		// cannot tell one filer's log from another's
		return wfs.relistDirectory(ctx, dir, nil)
	}
	basis, pending := wfs.metaCache.DirectoryPending(dir, resp.Filers)
	if !basis {
		return wfs.relistDirectory(ctx, dir, resp.Filers)
	}
	if len(pending) == 0 {
		return directoryCurrency{cached: true, verified: true}
	}
	for _, p := range pending {
		if !p.Remembered {
			// a floor names no event the stream will bring
			return wfs.relistDirectory(ctx, dir, resp.Filers)
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, directoryCurrentWait)
	current := wfs.metaCache.AwaitDirectoryPositions(waitCtx, dir, pending)
	cancel()
	if current {
		return directoryCurrency{cached: true, verified: true}
	}
	glog.V(1).Infof("metadata stream has not brought %s up to %v within %v; reading it from the filer", dir, pending, directoryCurrentWait)
	return wfs.relistDirectory(ctx, dir, resp.Filers)
}

// relistDirectory rebuilds dir's cached listing from the filer. The listing is
// read after the filers named positions, so it holds every change up to them.
// A directory not cached again reads through.
func (wfs *WFS) relistDirectory(ctx context.Context, dir util.FullPath, positions []*filer_pb.FilerDirectoryPosition) directoryCurrency {
	wfs.inodeToPath.InvalidateChildrenCache(dir)
	_, err := awaitFilerReply(ctx, "relist "+string(dir), func() (struct{}, error) {
		if err := wfs.ensureDirectoryVisited(dir); err != nil {
			return struct{}{}, err
		}
		if len(positions) > 0 {
			wfs.metaCache.NoteDirectoryBasis(dir, positions)
		}
		return struct{}{}, nil
	})
	if err != nil {
		var tooLarge *meta_cache.DirectoryTooLargeError
		if !errors.As(err, &tooLarge) {
			glog.V(1).Infof("relist %s: %v", dir, err)
		}
		return directoryCurrency{}
	}
	cached := wfs.metaCache.IsDirectoryCached(dir)
	return directoryCurrency{cached: cached, verified: cached}
}
