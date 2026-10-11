package mount

import (
	"fmt"
	"io"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/operation"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// chunkUploadBaseTimeout and chunkUploadMinRate size chunkUploadAttemptTimeout.
// A close() or fsync waits for every chunk still uploading and must be
// answered within the FUSE request deadline (60 s), so one attempt has to end
// well inside it with room left for a second attempt elsewhere. A healthy
// 2 MiB chunk upload measured 0.06 to 0.24 s on the production cluster under
// load; one that has not finished in 10 s is held by a stalled server.
const (
	chunkUploadBaseTimeout = 10 * time.Second
	chunkUploadMinRate     = 16 << 20 // bytes per second
)

// chunkUploadAttemptTimeout bounds one attempt to upload a chunk of up to
// chunkSize bytes. It stays above the volume server's replica write timeout
// for the same size, so a stalled replica fails the primary first and the
// primary's 500 names the replica to avoid.
func chunkUploadAttemptTimeout(chunkSize int64) time.Duration {
	return chunkUploadBaseTimeout + time.Duration(chunkSize*int64(time.Second)/chunkUploadMinRate)
}

// stalledVolumeServerTTL is how long chunk uploads avoid a volume server after
// an attempt stalled on it.
const stalledVolumeServerTTL = time.Minute

func (wfs *WFS) saveDataAsChunk(fullPath util.FullPath) filer.SaveDataAsChunkFunctionType {

	// Backstop: FUSE entry points sanitize names before they reach
	// inodeToPath, but async flush paths (e.g. writebackCache, handles whose
	// RememberPath was set from an older code path) may still carry bytes
	// that predate sanitization. Proto3 string fields require valid UTF-8,
	// so scrub the full path once here before every AssignVolume call.
	assignPath := fullPath.Sanitized()

	return func(reader io.Reader, filename string, offset int64, tsNs int64, _ uint64) (chunk *filer_pb.FileChunk, err error) {
		uploader, err := operation.NewUploader()
		if err != nil {
			return
		}

		// WantMd5 gives mount writes a real filer.ETag (server echoes Content-MD5 back).
		// One attempt per assignment: a retry against the same fid goes back
		// to the server that just failed, so every retry gets a new
		// assignment that avoids the servers recent attempts stalled on.
		uploadOption := &operation.UploadOption{
			Filename:          filename,
			Cipher:            wfs.option.Cipher,
			IsInputCompressed: false,
			MimeType:          "",
			PairMap:           nil,
			WantMd5:           true,
			MaxAttempts:       1,
			AttemptTimeout:    chunkUploadAttemptTimeout(wfs.option.ChunkSizeLimit),
			StalledServers:    wfs.stalledVolumeServers,
		}
		if wfs.option.VolumeServerAccess == "filerProxy" {
			uploadOption.GenUploadUrl = func(host, fileId string) string {
				return wfs.filerProxyChunkUrl(fileId)
			}
		}

		fileId, uploadResult, err, data := uploader.UploadWithRetry(
			wfs,
			&filer_pb.AssignVolumeRequest{
				Count:       1,
				Replication: wfs.option.Replication,
				Collection:  wfs.option.Collection,
				TtlSec:      wfs.option.TtlSec,
				DiskType:    string(wfs.option.DiskType),
				DataCenter:  wfs.option.DataCenter,
				Path:        assignPath,
			},
			uploadOption, reader,
		)

		if err != nil {
			glog.V(0).Infof("upload data %v: %v", filename, err)
			return nil, fmt.Errorf("upload data: %w", err)
		}
		if uploadResult.Error != "" {
			glog.V(0).Infof("upload failure %v: %v", filename, err)
			return nil, fmt.Errorf("upload result: %v", uploadResult.Error)
		}

		// When peer sharing is enabled we need EVERY chunk in the
		// local cache so we can actually serve it back to peers on
		// FetchChunk — otherwise the directory would advertise us as
		// a holder and the fetcher would get NOT_FOUND from our
		// chunk cache. When peer sharing is off we preserve the
		// original behavior of caching only the first chunk (small
		// files) to avoid blowing the cache on large uploads. Both
		// paths gate on chunkCache != nil: -cacheCapacityMB=0 disables
		// the cache entirely, in which case SetChunk would panic.
		shouldCache := wfs.chunkCache != nil && (offset == 0 || wfs.peerAnnouncer != nil)
		if shouldCache {
			wfs.chunkCache.SetChunk(fileId, data)
		}
		// Announce every uploaded chunk so the tier-2 directory fills
		// in as the file is written. Without this, the per-fetch
		// announce path only bootstraps after someone else has already
		// pulled a chunk via peer — which can't happen if nobody has
		// told the directory who holds the chunk. Skip the announce
		// when we couldn't cache (no point advertising bytes we can't
		// actually serve back).
		if wfs.peerAnnouncer != nil && shouldCache {
			wfs.peerAnnouncer.EnqueueAnnounce(fileId)
		}

		chunk = uploadResult.ToPbFileChunk(fileId, offset, tsNs)
		return chunk, nil
	}
}
