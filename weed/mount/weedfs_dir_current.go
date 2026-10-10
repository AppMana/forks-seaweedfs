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

// directoryPositions answers, per directory, how far the kernel's cached
// listing is current: OpenDir lets the kernel keep its listing only while no
// change has landed since the listing it already holds.
type directoryPositions struct {
	sync.Mutex
	kernelListing map[uint64]int64
	unsupported   bool
}

const directoryPositionsLimit = 1 << 16

func (p *directoryPositions) keepKernelListing(inode uint64, changeTsNs, currentTsNs int64) bool {
	p.Lock()
	defer p.Unlock()
	if p.kernelListing == nil || len(p.kernelListing) >= directoryPositionsLimit {
		p.kernelListing = make(map[uint64]int64)
	}
	kept, found := p.kernelListing[inode]
	p.kernelListing[inode] = currentTsNs
	return found && changeTsNs <= kept
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

// directoryChangePosition asks the filers for the newest change to dir's
// children.
func (wfs *WFS) directoryChangePosition(ctx context.Context, dir util.FullPath) (*filer_pb.DirectoryChangePositionResponse, error) {
	if wfs.dirPositions.isUnsupported() {
		return nil, errDirectoryPositionUnsupported
	}
	resp, err := awaitFilerReply(ctx, "directory position "+string(dir), func() (*filer_pb.DirectoryChangePositionResponse, error) {
		var resp *filer_pb.DirectoryChangePositionResponse
		err := wfs.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
			var callErr error
			resp, callErr = client.DirectoryChangePosition(ctx, &filer_pb.DirectoryChangePositionRequest{Directory: string(dir)})
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
	// changeTsNs is the newest change the filers named.
	changeTsNs int64
}

// awaitDirectoryCurrent makes a cached directory's listing show every change
// the filers acknowledged before the call: close-to-open across mounts. A
// change still on its way through the metadata stream is waited for; one the
// stream does not bring in time, or a position missing a peer filer's
// changes, has the directory read from the filer again.
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
		return wfs.relistDirectory(ctx, dir, 0)
	}
	if err != nil {
		glog.V(1).Infof("directory position %s: %v; serving the cached listing", dir, err)
		return directoryCurrency{cached: true}
	}
	if wfs.metaCache.DirectoryCurrentThrough(dir) >= resp.TsNs {
		return directoryCurrency{cached: true, verified: true, changeTsNs: resp.TsNs}
	}
	if resp.Remembered {
		waitCtx, cancel := context.WithTimeout(ctx, directoryCurrentWait)
		current := wfs.metaCache.AwaitDirectoryCurrentThrough(waitCtx, dir, resp.TsNs)
		cancel()
		if current {
			return directoryCurrency{cached: true, verified: true, changeTsNs: resp.TsNs}
		}
		glog.V(1).Infof("metadata stream has not brought %s to %d within %v; reading it from the filer", dir, resp.TsNs, directoryCurrentWait)
	}
	return wfs.relistDirectory(ctx, dir, resp.TsNs)
}

// relistDirectory rebuilds dir's cached listing from the filer. The listing is
// read after the filers reported changeTsNs, so it is current through it even
// when it carries no snapshot of its own (an empty directory). A directory not
// cached again reads through.
func (wfs *WFS) relistDirectory(ctx context.Context, dir util.FullPath, changeTsNs int64) directoryCurrency {
	wfs.inodeToPath.InvalidateChildrenCache(dir)
	_, err := awaitFilerReply(ctx, "relist "+string(dir), func() (struct{}, error) {
		if err := wfs.ensureDirectoryVisited(dir); err != nil {
			return struct{}{}, err
		}
		if changeTsNs != 0 {
			wfs.metaCache.NoteDirectoryCurrentThrough(dir, changeTsNs)
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
	return directoryCurrency{cached: cached, verified: cached, changeTsNs: changeTsNs}
}
