package s3api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/operation"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/security"
)

// copyChunkAttempts bounds how many destination volumes one chunk copy is
// offered to before the copy gives up.
const copyChunkAttempts = 3

// copySourceError is a chunk copy attempt that failed reading the source
// chunk, as opposed to writing the destination.
type copySourceError struct {
	// statusCode is the source volume server's HTTP status, 0 when it never
	// answered.
	statusCode int
	err        error
}

func (e *copySourceError) Error() string { return e.err.Error() }
func (e *copySourceError) Unwrap() error { return e.err }

// stalled reports whether the failure points at the source server rather
// than at the chunk: no answer, a full admission, or a server error.
func (e *copySourceError) stalled() bool {
	return e.statusCode == 0 || e.statusCode == http.StatusTooManyRequests || e.statusCode >= 500
}

// copySourceFailure wraps a source read error that carries no HTTP status. A
// transport failure or deadline counts as no answer; anything else, such as a
// failed decryption, is about the chunk.
func copySourceFailure(err error) error {
	var netErr net.Error
	if errors.Is(err, operation.ErrUploadStalled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) {
		return &copySourceError{statusCode: 0, err: err}
	}
	return &copySourceError{statusCode: -1, err: err}
}

// assignmentServers lists the servers holding an assigned volume, the
// primary first.
func assignmentServers(assignResult *filer_pb.AssignVolumeResponse) []string {
	servers := []string{assignResult.GetLocation().GetUrl()}
	for _, replica := range assignResult.GetReplicas() {
		servers = append(servers, replica.GetUrl())
	}
	return servers
}

// readLocation picks the location to read a chunk from: the first one whose
// server has not recently stalled, or the first one when all have.
func (s3a *S3ApiServer) readLocation(locations []*filer_pb.Location) *filer_pb.Location {
	for _, location := range locations {
		if _, stalled := s3a.stalledServers.FirstStalled([]string{location.Url}); !stalled {
			return location
		}
	}
	return locations[0]
}

// copyChunkToNewVolume retries failed copies with a new assignment,
// avoiding recently stalled source and destination servers.
func (s3a *S3ApiServer) copyChunkToNewVolume(ctx context.Context, sourceFileId, dstPath string, size uint64, write func(ctx context.Context, srcUrl string, assignResult *filer_pb.AssignVolumeResponse) error) (*filer_pb.AssignVolumeResponse, error) {
	var lastErr error
	for attempt := 1; attempt <= copyChunkAttempts; attempt++ {
		var assignResult *filer_pb.AssignVolumeResponse
		if err := s3a.stalledServers.Pick(func(avoid []string) ([]string, error) {
			result, err := s3a.assignNewVolume(dstPath, size, avoid...)
			if err != nil {
				return nil, err
			}
			assignResult = result
			return assignmentServers(result), nil
		}); err != nil {
			return nil, fmt.Errorf("assign volume: %w", err)
		}
		srcUrl, err := s3a.lookupVolumeUrl(sourceFileId)
		if err != nil {
			return nil, fmt.Errorf("lookup source URL: %w", err)
		}

		err = write(ctx, srcUrl, assignResult)
		if err == nil {
			return assignResult, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, err
		}

		// The destination may hold a needle a failed write left behind; no
		// chunk will name this fid, so drop it without holding up the retry.
		go operation.DeleteChunkFromHolders(assignmentServers(assignResult), assignResult.FileId, security.EncodedJwt(assignResult.Auth))

		var sourceErr *copySourceError
		if errors.As(err, &sourceErr) {
			if !sourceErr.stalled() {
				return nil, err
			}
			if host := urlHost(srcUrl); host != "" {
				s3a.stalledServers.Mark(host)
			}
		} else {
			s3a.stalledServers.NoteFailure(assignmentServers(assignResult), err)
			if !operation.ShouldReassignUpload(err) {
				return nil, err
			}
		}
		glog.V(1).Infof("copy chunk %s: attempt %d/%d failed, reassigning: %v", sourceFileId, attempt, copyChunkAttempts, err)
	}
	return nil, lastErr
}

// uploadChunkDataOnce uploads chunk data to the assigned destination in one
// attempt; copyChunkToNewVolume retries elsewhere instead of against the
// same fid.
func (s3a *S3ApiServer) uploadChunkDataOnce(ctx context.Context, chunkData []byte, assignResult *filer_pb.AssignVolumeResponse, isCompressed bool) error {
	uploadOption := newChunkUploadOption(assignResult, isCompressed)
	uploadOption.MaxAttempts = 1
	uploadOption.StallTimeout = operation.UploadStallTimeout
	uploadOption.ResponseTimeout = operation.GatewayUploadResponseTimeout
	uploader, err := operation.NewUploader()
	if err != nil {
		return fmt.Errorf("create uploader: %w", err)
	}
	if _, err = uploader.UploadData(ctx, chunkData, uploadOption); err != nil {
		return fmt.Errorf("upload chunk: %w", err)
	}
	return nil
}

// urlHost returns the host:port of a volume server URL.
func urlHost(rawUrl string) string {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return ""
	}
	return u.Host
}
