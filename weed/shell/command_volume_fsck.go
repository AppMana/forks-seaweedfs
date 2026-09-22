package shell

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/operation"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/security"
	"github.com/seaweedfs/seaweedfs/weed/storage"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle_map"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
	"golang.org/x/sync/errgroup"
)

func init() {
	Commands = append(Commands, &commandVolumeFsck{})
}

const (
	readbufferSize                 = 16
	jwtFilerTokenExpirationSeconds = 300
	// mtime is only second resolution, so a directory touched this recently is
	// left for the next run rather than compared against a timestamp a
	// concurrent write could share
	directoryQuietPeriod = 5 * time.Second
)

type commandVolumeFsck struct {
	env                       *CommandEnv
	writer                    io.Writer
	bucketsPath               string
	collection                *string
	volumeIds                 map[uint32]bool
	scopedFilerPath           string
	tempFolder                string
	verbose                   *bool
	forcePurging              *bool
	skipEcVolumes             *bool
	findMissingChunksInFiler  *bool
	verifyNeedle              *bool
	filerSigningKey           string
	unresolvedManifestEntries atomic.Int64
	// readNeedleMeta returns a needle's append time as the volume server
	// reads it at the copied index offset; tests replace it.
	readNeedleMeta func(server pb.ServerAddress, volumeId uint32, n needle_map.NeedleValue) (appendAtNs uint64, err error)
	// Test seams for purge transport; production uses MasterClient and operation.
	volumeServers  func(volumeId uint32) (servers []pb.ServerAddress, found bool)
	deleteFileIds  func(server pb.ServerAddress, fileIds []string) []*volume_server_pb.DeleteResult
	purgedDirsLock sync.Mutex
	purgedDirs     map[util.FullPath]struct{}
}

func (c *commandVolumeFsck) Name() string {
	return "volume.fsck"
}

func (c *commandVolumeFsck) Help() string {
	return `check all volumes to find entries not used by the filer. It is optional and resource intensive.

	Important assumption!!!
		the system is all used by one filer.

	This command works this way:
	1. collect all file ids from all volumes, as set A
	2. collect all file ids from the filer, as set B
	3. find out the set A subtract B

	If -findMissingChunksInFiler is enabled, this works
	in a reverse way:
	1. collect all file ids from all volumes, as set A
	2. collect all file ids from the filer, as set B
	3. find out the set B subtract A

	-cutoffTimeAgo is used to only check chunks older than the cutoff time.
	This is important because:
		Chunks are uploaded to volume servers before metadata is committed to filer.
		A newly uploaded chunk may appear as orphan if metadata commit is still pending.
		The default 5h cutoff provides sufficient buffer for metadata commits.

`
}

func (c *commandVolumeFsck) HasTag(tag CommandTag) bool {
	return tag == ResourceHeavy
}

func (c *commandVolumeFsck) Do(args []string, commandEnv *CommandEnv, writer io.Writer) (err error) {

	fsckCommand := flag.NewFlagSet(c.Name(), flag.ContinueOnError)
	c.verbose = fsckCommand.Bool("v", false, "verbose mode")
	c.skipEcVolumes = fsckCommand.Bool("skipEcVolumes", false, "skip erasure coded volumes")
	c.findMissingChunksInFiler = fsckCommand.Bool("findMissingChunksInFiler", false, "see \"help volume.fsck\"")
	c.collection = fsckCommand.String("collection", "", "the collection name")
	volumeIds := fsckCommand.String("volumeId", "", "comma separated the volume id")
	applyPurging := fsckCommand.Bool("reallyDeleteFromVolume", false, "<expert only!> after detection, delete missing data from volumes / delete missing file entries from filer. Currently this only works with default filerGroup.")
	c.forcePurging = fsckCommand.Bool("forcePurging", false, "delete missing data from volumes in one replica used together with applyPurging")
	purgeAbsent := fsckCommand.Bool("reallyDeleteFilerEntries", false, "<expert only!> delete missing file entries from filer if the corresponding volume is missing for any reason, please ensure all still existing/expected volumes are connected! used together with findMissingChunksInFiler")
	tempPath := fsckCommand.String("tempPath", path.Join(os.TempDir()), "path for temporary idx files")
	cutoffTimeAgo := fsckCommand.Duration("cutoffTimeAgo", 5*time.Hour, "only include entries on volume servers before this cutoff time to check orphan chunks")
	modifyTimeAgo := fsckCommand.Duration("modifyTimeAgo", 0, "only include entries after this modify time to check orphan chunks")
	c.verifyNeedle = fsckCommand.Bool("verifyNeedles", false, "check needles status from volume server")

	if err = fsckCommand.Parse(args); err != nil {
		return nil
	}

	// The command struct is a singleton registered in init(), so any state
	// not bound to a flag persists across shell invocations. Reset the
	// unresolved-manifest counter so a previous failed run can't permanently
	// suppress -reallyDeleteFromVolume in this session.
	c.unresolvedManifestEntries.Store(0)
	c.purgedDirs = make(map[util.FullPath]struct{})

	if err = commandEnv.confirmIsLocked(args); err != nil {
		return
	}
	c.volumeIds = make(map[uint32]bool)
	if *volumeIds != "" {
		for _, volumeIdStr := range strings.Split(*volumeIds, ",") {
			volumeIdStr = strings.TrimSpace(volumeIdStr)
			if volumeIdInt, err := strconv.ParseUint(volumeIdStr, 10, 32); err == nil {
				c.volumeIds[uint32(volumeIdInt)] = true
			} else {
				return fmt.Errorf("parse volumeId string %s to int: %v", volumeIdStr, err)
			}
		}
	}
	c.env = commandEnv
	c.writer = writer

	c.bucketsPath, err = readFilerBucketsPath(commandEnv)
	if err != nil {
		return fmt.Errorf("read filer buckets path: %w", err)
	}

	// create a temp folder
	c.tempFolder, err = os.MkdirTemp(util.ResolvePath(*tempPath), "sw_fsck")
	if err != nil {
		return fmt.Errorf("failed to create temp folder: %w", err)
	}
	if *c.verbose {
		fmt.Fprintf(c.writer, "working directory: %s\n", c.tempFolder)
	}
	defer os.RemoveAll(c.tempFolder)

	c.filerSigningKey = util.GetViper().GetString("jwt.filer_signing.key")

	// collect all volume id locations
	dataNodeVolumeIdToVInfo, err := c.collectVolumeIds()
	if err != nil {
		return fmt.Errorf("failed to collect all volume locations: %w", err)
	}

	// If the operator specified -volumeId, reject unknown ids up front instead
	// of silently filtering them out and reporting "no orphan data". A silent
	// success on a missing volume looks identical to a clean volume and hides
	// typos, already-deleted volumes, and stale scripts.
	if len(c.volumeIds) > 0 {
		known := make(map[uint32]bool)
		for _, vidMap := range dataNodeVolumeIdToVInfo {
			for vid := range vidMap {
				known[vid] = true
			}
		}
		var missing []uint32
		for vid := range c.volumeIds {
			if !known[vid] {
				missing = append(missing, vid)
			}
		}
		if len(missing) > 0 {
			sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
			return fmt.Errorf("volume(s) not found on master: %v", missing)
		}
	}

	c.scopedFilerPath = c.resolveScopedFilerPath(dataNodeVolumeIdToVInfo)
	if *c.verbose && c.scopedFilerPath != "/" {
		fmt.Fprintf(c.writer, "scoping filer walk to %s\n", c.scopedFilerPath)
	}

	var collectCutoffFromAtNs int64 = 0
	if cutoffTimeAgo.Seconds() != 0 {
		collectCutoffFromAtNs = time.Now().Add(-*cutoffTimeAgo).UnixNano()
	}
	var collectModifyFromAtNs int64 = 0
	if modifyTimeAgo.Seconds() != 0 {
		collectModifyFromAtNs = time.Now().Add(-*modifyTimeAgo).UnixNano()
	}
	// collect each volume file ids
	eg, _ := errgroup.WithContext(context.Background())
	for _dataNodeId, _volumeIdToVInfo := range dataNodeVolumeIdToVInfo {
		dataNodeId, volumeIdToVInfo := _dataNodeId, _volumeIdToVInfo
		eg.Go(func() error {
			for volumeId, vinfo := range volumeIdToVInfo {
				if *c.skipEcVolumes && vinfo.isEcVolume {
					delete(volumeIdToVInfo, volumeId)
					continue
				}
				if len(c.volumeIds) > 0 {
					if _, ok := c.volumeIds[volumeId]; !ok {
						delete(volumeIdToVInfo, volumeId)
						continue
					}
				}
				if *c.collection != "" && vinfo.collection != *c.collection {
					delete(volumeIdToVInfo, volumeId)
					continue
				}
				err = c.collectOneVolumeFileIds(dataNodeId, volumeId, vinfo)
				if err != nil {
					return fmt.Errorf("failed to collect file ids from volume %d on %s: %v", volumeId, vinfo.server, err)
				}
			}
			if *c.verbose {
				fmt.Fprintf(c.writer, "dn %+v filtered %d volumes and locations.\n", dataNodeId, len(dataNodeVolumeIdToVInfo[dataNodeId]))
			}
			return nil
		})
	}
	err = eg.Wait()
	if err != nil {
		fmt.Fprintf(c.writer, "got error: %v", err)
		return err
	}

	if *c.findMissingChunksInFiler {
		// collect all filer file ids and paths

		if err = c.collectFilerFileIdAndPaths(dataNodeVolumeIdToVInfo, *purgeAbsent, collectModifyFromAtNs, collectCutoffFromAtNs); err != nil {
			return fmt.Errorf("collectFilerFileIdAndPaths: %w", err)
		}
		for dataNodeId, volumeIdToVInfo := range dataNodeVolumeIdToVInfo {
			// for each volume, check filer file ids
			if err = c.findFilerChunksMissingInVolumeServers(volumeIdToVInfo, dataNodeId, *applyPurging || *purgeAbsent); err != nil {
				return fmt.Errorf("findFilerChunksMissingInVolumeServers: %w", err)
			}
		}
		c.purgeEmptyDirectories()
	} else {
		// collect all filer file ids
		if err = c.collectFilerFileIdAndPaths(dataNodeVolumeIdToVInfo, false, 0, 0); err != nil {
			return fmt.Errorf("failed to collect file ids from filer: %w", err)
		}
		// If any entry's manifest could not be resolved, our in-use fid set
		// is missing the sub-chunks behind it. Purging orphans now would
		// delete live data referenced only via the unresolved manifest, so
		// disable -reallyDeleteFromVolume for this run and tell the operator
		// to fix the broken entries first.
		applyPurgingEffective := *applyPurging
		if unresolved := c.unresolvedManifestEntries.Load(); unresolved > 0 && applyPurgingEffective {
			fmt.Fprintf(c.writer, "WARNING: %d entry(ies) had unresolvable chunk manifests; refusing to apply -reallyDeleteFromVolume to avoid deleting live sub-chunks. Fix the entries listed above (e.g. delete or repair them) and re-run.\n",
				unresolved)
			applyPurgingEffective = false
		}
		// volume file ids subtract filer file ids
		if err = c.findExtraChunksInVolumeServers(dataNodeVolumeIdToVInfo, applyPurgingEffective, uint64(collectModifyFromAtNs), uint64(collectCutoffFromAtNs)); err != nil {
			return fmt.Errorf("findExtraChunksInVolumeServers: %w", err)
		}
		if unresolved := c.unresolvedManifestEntries.Load(); unresolved > 0 {
			return fmt.Errorf("incomplete fsck: %d entries have unresolved chunk manifests; no purge was authorized", unresolved)
		}
	}

	return nil
}

func (c *commandVolumeFsck) collectFilerFileIdAndPaths(dataNodeVolumeIdToVInfo map[string]map[uint32]VInfo, purgeAbsent bool, collectModifyFromAtNs int64, cutoffFromAtNs int64) error {
	if *c.verbose {
		fmt.Fprintf(c.writer, "checking each file from filer path %s...\n", c.getCollectFilerFilePath())
	}

	files := make(map[uint32]*os.File)
	for _, volumeIdToServer := range dataNodeVolumeIdToVInfo {
		for vid := range volumeIdToServer {
			if _, ok := files[vid]; ok {
				continue
			}
			dst, openErr := os.OpenFile(getFilerFileIdFile(c.tempFolder, vid), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
			if openErr != nil {
				return fmt.Errorf("failed to create file %s: %v", getFilerFileIdFile(c.tempFolder, vid), openErr)
			}
			files[vid] = dst
		}
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()

	return doTraverseBfsAndSaving(c.env, c.writer, c.getCollectFilerFilePath(), false, false,
		func(ctx context.Context, entry *filer_pb.FullEntry, outputChan chan interface{}) (err error) {
			if *c.verbose && entry.Entry.IsDirectory {
				fmt.Fprintf(c.writer, "checking directory %s\n", util.NewFullPath(entry.Dir, entry.Entry.Name))
			}
			dataChunks, manifestChunks, resolveErr := filer.ResolveChunkManifest(ctx, filer.LookupFn(c.env), entry.Entry.GetChunks(), 0, math.MaxInt64, nil)
			if resolveErr != nil {
				// Cancellation/deadline isn't manifest corruption; surface it
				// so the BFS bails out cleanly without polluting the
				// unresolved-manifest counter (which would otherwise block
				// purges and mislead the operator about the failure cause).
				if errors.Is(resolveErr, context.Canceled) || errors.Is(resolveErr, context.DeadlineExceeded) {
					return resolveErr
				}
				// A single broken manifest used to abort the whole traversal,
				// leaving the operator with no way to identify orphans without
				// first fixing the broken file. Instead, record only the
				// top-level chunk fids (data chunks plus the manifest needles
				// themselves — sub-chunks behind the unreadable manifest are
				// unknown), warn, and keep going. The unresolved counter blocks
				// any purge step downstream so we never delete a sub-chunk we
				// couldn't account for.
				fmt.Fprintf(c.writer, "WARNING: ResolveChunkManifest failed for %s: %v — recording top-level chunk fids only; purging will be disabled\n",
					util.NewFullPath(entry.Dir, entry.Entry.Name), resolveErr)
				c.unresolvedManifestEntries.Add(1)
				dataChunks = entry.Entry.GetChunks()
				manifestChunks = nil
			}
			dataChunks = append(dataChunks, manifestChunks...)
			for _, chunk := range dataChunks {
				if cutoffFromAtNs != 0 && chunk.ModifiedTsNs > cutoffFromAtNs {
					continue
				}
				if collectModifyFromAtNs != 0 && chunk.ModifiedTsNs < collectModifyFromAtNs {
					continue
				}
				select {
				case outputChan <- &Item{
					vid:     chunk.Fid.VolumeId,
					fileKey: chunk.Fid.FileKey,
					cookie:  chunk.Fid.Cookie,
					path:    util.NewFullPath(entry.Dir, entry.Entry.Name),
				}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
		func(outputChan chan interface{}) error {
			buffer := make([]byte, readbufferSize)
			for item := range outputChan {
				i := item.(*Item)
				if f, ok := files[i.vid]; ok {
					util.Uint64toBytes(buffer, i.fileKey)
					util.Uint32toBytes(buffer[8:], i.cookie)
					util.Uint32toBytes(buffer[12:], uint32(len(i.path)))
					if _, err := f.Write(buffer); err != nil {
						return err
					}
					if _, err := f.Write([]byte(i.path)); err != nil {
						return err
					}
				} else if *c.findMissingChunksInFiler {
					// check if the volume matches the filter
					if len(c.volumeIds) > 0 {
						if _, ok := c.volumeIds[i.vid]; !ok {
							continue
						}
					}
					fmt.Fprintf(c.writer, "%d,%x%08x %s volume not found\n", i.vid, i.fileKey, i.cookie, i.path)
					if purgeAbsent {
						fmt.Fprintf(c.writer, "deleting path %s after volume not found\n", i.path)
						if err := c.httpDelete(i.path); err != nil {
							return err
						}
					}
				}
			}
			return nil
		})
}

func (c *commandVolumeFsck) findFilerChunksMissingInVolumeServers(volumeIdToVInfo map[uint32]VInfo, dataNodeId string, applyPurging bool) error {

	for volumeId, vinfo := range volumeIdToVInfo {
		checkErr := c.oneVolumeFileIdsCheckOneVolume(dataNodeId, volumeId, applyPurging)
		if checkErr != nil {
			return fmt.Errorf("failed to collect file ids from volume %d on %s: %v", volumeId, vinfo.server, checkErr)
		}
	}
	return nil
}

func (c *commandVolumeFsck) findExtraChunksInVolumeServers(dataNodeVolumeIdToVInfo map[string]map[uint32]VInfo, applyPurging bool, modifyFrom, cutoffFrom uint64) error {

	var totalInUseCount, totalOrphanChunkCount, totalOrphanDataSize uint64
	// map[volumeId]map[fid]replicaCount — counts how many replicas reported
	// this fid as orphan. A fid is safe to purge without -forcePurging only
	// when replicaCount == volumeReplicaCounts[volumeId] (i.e. every replica
	// agrees it's orphan). The previous bool-based tracking treated "seen on
	// any 2 replicas" as "seen on all replicas", which was wrong for
	// 3+-replica volumes.
	volumeIdOrphanFileIds := make(map[uint32]map[string]int)
	volumeReplicaCounts := make(map[uint32]int)
	isEcVolumeReplicas := make(map[uint32]bool)
	// Track which specific replicas were read-only so we only flip those
	// back on exit. The old `isReadOnlyReplicas[volumeId] = bool` leaked
	// read-only state across replicas: if one replica was RO and another RW,
	// the deferred cleanup would mark the originally-RW replica RO too.
	readOnlyServerReplicas := make(map[uint32][]pb.ServerAddress)
	// Phase 1: collect orphan fids from every replica of every volume.
	// The purge step runs in Phase 2, AFTER every replica has contributed.
	// Running purge inside this loop (as the original code did) meant the
	// first replica's orphans were deleted before later replicas could
	// participate in the intersection — so the "only purge fids seen on
	// all replicas" safety net only worked by accident, and purge also
	// fired multiple times per volume (once per data-node iteration).
	for dataNodeId, volumeIdToVInfo := range dataNodeVolumeIdToVInfo {
		for volumeId, vinfo := range volumeIdToVInfo {
			inUseCount, orphanFileIds, orphanDataSize, checkErr := c.oneVolumeFileIdsSubtractFilerFileIds(dataNodeId, volumeId, &vinfo, modifyFrom, cutoffFrom)
			if checkErr != nil {
				return fmt.Errorf("failed to collect file ids from volume %d on %s: %v", volumeId, vinfo.server, checkErr)
			}
			if _, found := volumeIdOrphanFileIds[volumeId]; !found {
				volumeIdOrphanFileIds[volumeId] = make(map[string]int)
			}
			volumeReplicaCounts[volumeId]++
			for _, fid := range orphanFileIds {
				volumeIdOrphanFileIds[volumeId][fid]++
			}

			totalInUseCount += inUseCount
			totalOrphanChunkCount += uint64(len(orphanFileIds))
			totalOrphanDataSize += orphanDataSize

			if *c.verbose {
				for _, fid := range orphanFileIds {
					fmt.Fprintf(c.writer, "%s:%s\n", vinfo.collection, fid)
				}
			}
			isEcVolumeReplicas[volumeId] = vinfo.isEcVolume
			if vinfo.isReadOnly {
				readOnlyServerReplicas[volumeId] = append(readOnlyServerReplicas[volumeId], vinfo.server)
			}
		}
	}

	// Phase 2: purge. At most one call to purgeFileIdsForOneVolume per
	// volume — that helper already fans out to all replica locations via
	// MasterClient.GetLocations, so iterating per replica here (as the old
	// code did) would issue N*N delete RPCs for N replicas.
	if applyPurging {
		var skippedVolumeIds []uint32
		for volumeId, orphanReplicaFileIds := range volumeIdOrphanFileIds {
			if len(orphanReplicaFileIds) == 0 {
				continue
			}
			if isEcVolumeReplicas[volumeId] {
				fmt.Fprintf(c.writer, "skip purging for Erasure Coded volume %d.\n", volumeId)
				continue
			}
			// Call out to a closure per volume so the deferred "mark
			// readonly again" fires between volumes instead of piling up
			// until findExtraChunksInVolumeServers returns. Per-volume
			// failures (e.g. a replica stuck read-only) don't halt the
			// rest of the run; the volume is remembered and its deletes
			// are skipped.
			if err := c.purgeOneVolume(volumeId, orphanReplicaFileIds, volumeReplicaCounts[volumeId], readOnlyServerReplicas[volumeId]); err != nil {
				fmt.Fprintf(c.writer, "skip purging volume %d: %v\n", volumeId, err)
				skippedVolumeIds = append(skippedVolumeIds, volumeId)
			}
		}
		if len(skippedVolumeIds) > 0 {
			sort.Slice(skippedVolumeIds, func(i, j int) bool { return skippedVolumeIds[i] < skippedVolumeIds[j] })
			fmt.Fprintf(c.writer, "skipped purge on %d volume(s): %v\n", len(skippedVolumeIds), skippedVolumeIds)
			return fmt.Errorf("incomplete purge on volumes %v", skippedVolumeIds)
		}
	}

	if !applyPurging {
		var pct float64

		if totalCount := totalOrphanChunkCount + totalInUseCount; totalCount > 0 {
			pct = float64(totalOrphanChunkCount) * 100 / (float64(totalCount))
		}

		fmt.Fprintf(c.writer, "\nTotal\t\tentries:%d\torphan:%d\t%.2f%%\t%dB\n",
			totalOrphanChunkCount+totalInUseCount, totalOrphanChunkCount, pct, totalOrphanDataSize)

		fmt.Fprintf(c.writer, "This could be normal if multiple filers or no filers are used.\n")
	}

	if totalOrphanChunkCount == 0 {
		fmt.Fprintf(c.writer, "no orphan data\n")
	}

	return nil
}

// purgeOneVolume picks the orphan fids to delete for a single volume and
// fires the delete RPC. It's split out of findExtraChunksInVolumeServers so
// the `defer markVolumeWritable(context.Background(), ..., false, false)` at the bottom fires
// between volumes — putting that defer inside the caller's for-loop would
// leave every processed volume writable until the whole fsck run finished.
func (c *commandVolumeFsck) purgeOneVolume(volumeId uint32, orphanReplicaFileIds map[string]int, replicaCount int, readOnlyReplicas []pb.ServerAddress) error {
	orphanFileIds := make([]string, 0, len(orphanReplicaFileIds))
	for fid, foundInReplicaCount := range orphanReplicaFileIds {
		// Default safety net: only purge fids every replica reported as
		// orphan. -forcePurging bypasses this for operators who've already
		// decided they're OK with the single-replica evidence.
		if foundInReplicaCount == replicaCount || *c.forcePurging {
			orphanFileIds = append(orphanFileIds, fid)
		}
	}
	if len(orphanFileIds) == 0 {
		return nil
	}
	if *c.verbose {
		fmt.Fprintf(c.writer, "purging process for volume %d.\n", volumeId)
	}

	needleVID := needle.VolumeId(volumeId)
	for _, server := range readOnlyReplicas {
		if err := markVolumeWritable(context.Background(), c.env.option.GrpcDialOption, needleVID, server, true, false); err != nil {
			// Replicas flipped writable earlier roll back via the defer.
			return fmt.Errorf("mark %v writable: %v", server, err)
		}
		fmt.Fprintf(c.writer, "temporarily marked %d on server %v writable for forced purge\n", volumeId, server)
		defer markVolumeWritable(context.Background(), c.env.option.GrpcDialOption, needleVID, server, false, false)
	}

	if *c.verbose {
		fmt.Fprintf(c.writer, "purging files from volume %d\n", volumeId)
	}

	if err := c.purgeFileIdsForOneVolume(volumeId, orphanFileIds); err != nil {
		return fmt.Errorf("purging volume %d: %v", volumeId, err)
	}
	return nil
}

func (c *commandVolumeFsck) collectOneVolumeFileIds(dataNodeId string, volumeId uint32, vinfo VInfo) error {

	if *c.verbose {
		fmt.Fprintf(c.writer, "collecting volume %d file ids from %s ...\n", volumeId, vinfo.server)
	}

	return operation.WithVolumeServerClient(false, vinfo.server, c.env.option.GrpcDialOption,
		func(volumeServerClient volume_server_pb.VolumeServerClient) error {
			ext := ".idx"
			if vinfo.isEcVolume {
				ext = ".ecx"
			}

			copyFileClient, err := volumeServerClient.CopyFile(context.Background(), &volume_server_pb.CopyFileRequest{
				VolumeId:                 volumeId,
				Ext:                      ext,
				CompactionRevision:       math.MaxUint32,
				StopOffset:               math.MaxInt64,
				Collection:               vinfo.collection,
				IsEcVolume:               vinfo.isEcVolume,
				IgnoreSourceFileNotFound: false,
			})
			if err != nil {
				return fmt.Errorf("failed to start copying volume %d%s: %v", volumeId, ext, err)
			}

			var buf bytes.Buffer
			for {
				resp, err := copyFileClient.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
				buf.Write(resp.FileContent)
			}
			idxFilename := getVolumeFileIdFile(c.tempFolder, dataNodeId, volumeId)
			err = writeToFile(buf.Bytes(), idxFilename)
			if err != nil {
				return fmt.Errorf("failed to copy %d%s from %s: %v", volumeId, ext, vinfo.server, err)
			}

			return nil
		})

}

type Item struct {
	vid     uint32
	fileKey uint64
	cookie  uint32
	path    util.FullPath
}

func (c *commandVolumeFsck) readFilerFileIdFile(volumeId uint32, fn func(needleId types.NeedleId, itemPath util.FullPath) error) error {
	fp, err := os.Open(getFilerFileIdFile(c.tempFolder, volumeId))
	if err != nil {
		return err
	}
	defer fp.Close()

	br := bufio.NewReader(fp)
	buffer := make([]byte, readbufferSize)
	var readSize int
	var readErr error
	item := &Item{vid: volumeId}
	for {
		readSize, readErr = io.ReadFull(br, buffer)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read fid header for volume %d: %w", volumeId, readErr)
		}
		if readSize != readbufferSize {
			return fmt.Errorf("read fid header size mismatch for volume %d: got %d want %d", volumeId, readSize, readbufferSize)
		}
		item.fileKey = util.BytesToUint64(buffer[:8])
		item.cookie = util.BytesToUint32(buffer[8:12])
		pathSize := util.BytesToUint32(buffer[12:16])
		pathBytes := make([]byte, int(pathSize))
		n, err := io.ReadFull(br, pathBytes)
		if err != nil {
			return fmt.Errorf("read fid path for volume %d,%x%08x: %w", volumeId, item.fileKey, item.cookie, err)
		}
		if n != int(pathSize) {
			return fmt.Errorf("read fid path size mismatch for volume %d,%x%08x: got %d want %d", volumeId, item.fileKey, item.cookie, n, pathSize)
		}
		item.path = util.FullPath(pathBytes)
		needleId := types.NeedleId(item.fileKey)
		if err := fn(needleId, item.path); err != nil {
			return fmt.Errorf("process fid %d,%x%08x path %q: %w", volumeId, item.fileKey, item.cookie, item.path, err)
		}
	}
	return nil
}

func (c *commandVolumeFsck) oneVolumeFileIdsCheckOneVolume(dataNodeId string, volumeId uint32, applyPurging bool) (err error) {
	if *c.verbose {
		fmt.Fprintf(c.writer, "find missing file chunks in dataNodeId %s volume %d ...\n", dataNodeId, volumeId)
	}

	db := needle_map.NewMemDb()
	defer db.Close()

	if err = db.LoadFromIdx(getVolumeFileIdFile(c.tempFolder, dataNodeId, volumeId)); err != nil {
		return
	}
	if err = c.readFilerFileIdFile(volumeId, func(needleId types.NeedleId, itemPath util.FullPath) error {
		if _, found := db.Get(needleId); !found {
			fmt.Fprintf(c.writer, "%s\n", itemPath)
			if applyPurging {
				return c.httpDelete(itemPath)
			}
		}
		return nil
	}); err != nil {
		return
	}
	return nil
}

func (c *commandVolumeFsck) httpDelete(path util.FullPath) error {
	req, err := http.NewRequest(http.MethodDelete, "", nil)
	if err != nil {
		return fmt.Errorf("create HTTP DELETE request for %q: %w", path, err)
	}

	req.URL = &url.URL{
		Scheme: "http",
		Host:   c.env.option.FilerAddress.ToHttpAddress(),
		Path:   string(path),
	}

	if c.filerSigningKey != "" {
		encodedJwt := security.GenJwtForFilerServer(security.SigningKey(c.filerSigningKey), jwtFilerTokenExpirationSeconds)
		req.Header.Set("Authorization", security.BearerPrefix+string(encodedJwt))
	}

	if *c.verbose {
		fmt.Fprintf(c.writer, "full HTTP delete request to be sent: %v\n", req)
	}

	resp, err := util_http.GetGlobalHttpClient().Do(req)
	if err != nil {
		return fmt.Errorf("DELETE %q: %w", path, err)
	}
	defer resp.Body.Close()

	if _, err = io.ReadAll(resp.Body); err != nil {
		return fmt.Errorf("read DELETE %q response: %w", path, err)
	}

	if *c.verbose {
		fmt.Fprintln(c.writer, "delete response Status : ", resp.Status)
		fmt.Fprintln(c.writer, "delete response Headers : ", resp.Header)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("DELETE %q returned %s", path, resp.Status)
	}
	dir, _ := path.DirAndName()
	c.purgedDirsLock.Lock()
	c.purgedDirs[util.FullPath(dir)] = struct{}{}
	c.purgedDirsLock.Unlock()
	return nil
}

// purgeEmptyDirectories removes the directories emptied by the purged entries, walking up while each parent is empty too.
func (c *commandVolumeFsck) purgeEmptyDirectories() {
	candidates := make(map[util.FullPath]struct{})
	c.purgedDirsLock.Lock()
	for dir := range c.purgedDirs {
		for d := dir; c.canPurgeDirectory(d); {
			candidates[d] = struct{}{}
			parent, _ := d.DirAndName()
			d = util.FullPath(parent)
		}
	}
	c.purgedDirsLock.Unlock()

	dirs := make([]util.FullPath, 0, len(candidates))
	for dir := range candidates {
		dirs = append(dirs, dir)
	}
	// deepest first, so a directory is only tried once its children are gone
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })

	for _, dir := range dirs {
		entry, _, _, lookupErr := filer_pb.GetEntry(context.Background(), c.env, dir)
		if lookupErr != nil && !errors.Is(lookupErr, filer_pb.ErrNotFound) {
			fmt.Fprintf(c.writer, "lookup directory %s: %v\n", dir, lookupErr)
			continue
		}
		// a directory key object is an S3 object of its own
		if entry == nil || entry.IsDirectoryKeyObject() {
			continue
		}
		// a zero mtime turns the delete's condition off, leaving nothing to hold it to
		mtime := entry.Attributes.GetMtime()
		if mtime <= 0 || mtime >= time.Now().Add(-directoryQuietPeriod).Unix() {
			continue
		}
		if err := c.deleteEmptyDirectory(dir, mtime); err != nil {
			if !errors.Is(err, filer.ErrNonEmptyFolder) {
				fmt.Fprintf(c.writer, "delete empty directory %s: %v\n", dir, err)
			}
			continue
		}
		fmt.Fprintf(c.writer, "deleted empty directory %s\n", dir)
	}
}

// deleteEmptyDirectory deletes dir unless it changed since it was looked up at mtime,
// so a directory promoted to an S3 object meanwhile survives. The delete is not
// recursive, leaving the filer to reject a directory that is not empty.
func (c *commandVolumeFsck) deleteEmptyDirectory(dir util.FullPath, mtime int64) error {
	parent, name := dir.DirAndName()
	return c.env.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
		resp, err := client.DeleteEntry(context.Background(), &filer_pb.DeleteEntryRequest{
			Directory:          parent,
			Name:               name,
			IfNotModifiedAfter: mtime,
		})
		if err != nil {
			return err
		}
		if resp.Error != "" {
			return filer.DeleteEntryError(resp.Error)
		}
		return nil
	})
}

func (c *commandVolumeFsck) canPurgeDirectory(dir util.FullPath) bool {
	root := c.getCollectFilerFilePath()
	if string(dir) == root || !strings.HasPrefix(string(dir), strings.TrimSuffix(root, "/")+"/") {
		return false
	}
	// deleting a bucket drops its whole collection
	parent, _ := dir.DirAndName()
	return string(dir) != c.bucketsPath && parent != c.bucketsPath
}

func (c *commandVolumeFsck) oneVolumeFileIdsSubtractFilerFileIds(dataNodeId string, volumeId uint32, vinfo *VInfo, modifyFrom, cutoffFrom uint64) (inUseCount uint64, orphanFileIds []string, orphanDataSize uint64, err error) {

	volumeFileIdDb := needle_map.NewMemDb()
	defer volumeFileIdDb.Close()

	if err = volumeFileIdDb.LoadFromIdx(getVolumeFileIdFile(c.tempFolder, dataNodeId, volumeId)); err != nil {
		err = fmt.Errorf("failed to LoadFromIdx %+v", err)
		return
	}

	if err = c.readFilerFileIdFile(volumeId, func(filerNeedleId types.NeedleId, itemPath util.FullPath) error {
		inUseCount++
		if *c.verifyNeedle && !vinfo.isEcVolume {
			if needleValue, ok := volumeFileIdDb.Get(filerNeedleId); ok && !needleValue.Size.IsDeleted() {
				if _, err := readNeedleStatus(c.env.option.GrpcDialOption, vinfo.server, volumeId, *needleValue); err != nil {
					// files may be deleted during copying filesIds
					if !strings.Contains(err.Error(), storage.ErrorDeleted.Error()) {
						fmt.Fprintf(c.writer, "failed to read %d:%s needle status of file %s: %+v\n",
							volumeId, filerNeedleId.String(), itemPath, err)
						if *c.forcePurging {
							return nil
						}
					}
				}
			}
		}

		if err = volumeFileIdDb.Delete(filerNeedleId); err != nil && *c.verbose {
			fmt.Fprintf(c.writer, "failed to nm.delete %s(%+v): %+v", itemPath, filerNeedleId, err)
		}
		return nil
	}); err != nil {
		err = fmt.Errorf("failed to readFilerFileIdFile %+v", err)
		return
	}

	var orphanFileCount, staleNeedleCount uint64
	if err = volumeFileIdDb.AscendingVisit(func(n needle_map.NeedleValue) error {
		if n.Size.IsDeleted() {
			return nil
		}
		if !vinfo.isEcVolume && (cutoffFrom > 0 || modifyFrom > 0) {
			appendAtNs, readErr := c.needleAppendAtNs(vinfo.server, volumeId, n)
			if readErr != nil {
				// This may be a stale index, but can also be an I/O or RPC
				// failure. Finish collecting diagnostics, then fail closed.
				staleNeedleCount++
				if *c.verbose {
					fmt.Fprintf(c.writer, "volume %d needle %d metadata unknown: %v\n", volumeId, n.Key, readErr)
				}
				return nil
			}
			if (modifyFrom == 0 || modifyFrom <= appendAtNs) && (cutoffFrom == 0 || appendAtNs <= cutoffFrom) {
				orphanFileIds = append(orphanFileIds, n.Key.FileId(volumeId))
				orphanFileCount++
				orphanDataSize += uint64(n.Size)
			}
			return nil
		} else {
			if vinfo.isEcVolume && (cutoffFrom > 0 || modifyFrom > 0) {
				if *c.verbose {
					fmt.Fprintf(c.writer, "skipping time-based filtering for EC volume %d (cutoffFrom=%d, modifyFrom=%d)\n", volumeId, cutoffFrom, modifyFrom)
				}
			}
			orphanFileIds = append(orphanFileIds, n.Key.FileId(volumeId))
			orphanFileCount++
			orphanDataSize += uint64(n.Size)
		}
		return nil
	}); err != nil {
		err = fmt.Errorf("failed to AscendingVisit %+v", err)
		return
	}

	if orphanFileCount > 0 {
		pct := float64(orphanFileCount*100) / (float64(orphanFileCount + inUseCount))
		fmt.Fprintf(c.writer, "dataNode:%s\tvolume:%d\tentries:%d\torphan:%d\t%.2f%%\t%dB\n",
			dataNodeId, volumeId, orphanFileCount+inUseCount, orphanFileCount, pct, orphanDataSize)
	}
	if staleNeedleCount > 0 {
		fmt.Fprintf(c.writer, "dataNode:%s\tvolume:%d\t%d needle(s) have unknown metadata; scan incomplete, refusing purge; investigate and rerun\n",
			dataNodeId, volumeId, staleNeedleCount)
		return inUseCount, nil, 0, fmt.Errorf("incomplete fsck for volume %d: %d needle metadata reads failed", volumeId, staleNeedleCount)
	}

	return

}

// needleAppendAtNs reads one needle's append time from its volume server at
// the offset the copied index recorded.
func (c *commandVolumeFsck) needleAppendAtNs(server pb.ServerAddress, volumeId uint32, n needle_map.NeedleValue) (uint64, error) {
	if c.readNeedleMeta != nil {
		return c.readNeedleMeta(server, volumeId, n)
	}
	var appendAtNs uint64
	err := operation.WithVolumeServerClient(false, server, c.env.option.GrpcDialOption,
		func(volumeServerClient volume_server_pb.VolumeServerClient) error {
			resp, err := volumeServerClient.ReadNeedleMeta(context.Background(), &volume_server_pb.ReadNeedleMetaRequest{
				VolumeId: volumeId,
				NeedleId: types.NeedleIdToUint64(n.Key),
				Offset:   n.Offset.ToActualOffset(),
				Size:     int32(n.Size),
			})
			if err != nil {
				return fmt.Errorf("read needle meta with id %d from volume %d: %v", n.Key, volumeId, err)
			}
			appendAtNs = resp.AppendAtNs
			return nil
		})
	return appendAtNs, err
}

type VInfo struct {
	server     pb.ServerAddress
	collection string
	isEcVolume bool
	isReadOnly bool
}

func (c *commandVolumeFsck) collectVolumeIds() (volumeIdToServer map[string]map[uint32]VInfo, err error) {

	if *c.verbose {
		fmt.Fprintf(c.writer, "collecting volume id and locations from master ...\n")
	}

	volumeIdToServer = make(map[string]map[uint32]VInfo)
	// collect topology information
	topologyInfo, _, err := collectTopologyInfo(c.env, 0)
	if err != nil {
		return
	}

	eachDataNode(topologyInfo, func(dc DataCenterId, rack RackId, t *master_pb.DataNodeInfo) {
		var volumeCount, ecShardCount int
		dataNodeId := t.GetId()
		for _, diskInfo := range t.DiskInfos {
			if _, ok := volumeIdToServer[dataNodeId]; !ok {
				volumeIdToServer[dataNodeId] = make(map[uint32]VInfo)
			}
			for _, vi := range diskInfo.VolumeInfos {
				volumeIdToServer[dataNodeId][vi.Id] = VInfo{
					server:     pb.NewServerAddressFromDataNode(t),
					collection: vi.Collection,
					isEcVolume: false,
					isReadOnly: vi.ReadOnly,
				}
				volumeCount += 1
			}
			for _, ecShardInfo := range diskInfo.EcShardInfos {
				volumeIdToServer[dataNodeId][ecShardInfo.Id] = VInfo{
					server:     pb.NewServerAddressFromDataNode(t),
					collection: ecShardInfo.Collection,
					isEcVolume: true,
					isReadOnly: true,
				}
				ecShardCount += 1
			}
		}
		if *c.verbose {
			fmt.Fprintf(c.writer, "dn %+v collected %d volumes and %d ec shards.\n", dataNodeId, volumeCount, ecShardCount)
		}
	})
	return
}

func (c *commandVolumeFsck) purgeFileIdsForOneVolume(volumeId uint32, fileIds []string) (err error) {
	fmt.Fprintf(c.writer, "purging orphan data for volume %d...\n", volumeId)
	var servers []pb.ServerAddress
	var found bool
	if c.volumeServers != nil {
		servers, found = c.volumeServers(volumeId)
	} else {
		locations, locationsFound := c.env.MasterClient.GetLocations(volumeId)
		found = locationsFound
		for _, location := range locations {
			servers = append(servers, location.ServerAddress())
		}
	}
	if !found {
		return fmt.Errorf("failed to find volume %d locations", volumeId)
	}
	if len(servers) == 0 {
		return fmt.Errorf("incomplete purge for volume %d: no volume servers found", volumeId)
	}

	type serverDeleteResults struct {
		server  pb.ServerAddress
		results []*volume_server_pb.DeleteResult
	}
	resultChan := make(chan serverDeleteResults, len(servers))
	var wg sync.WaitGroup
	for _, server := range servers {
		wg.Add(1)
		go func(server pb.ServerAddress, fidList []string) {
			defer wg.Done()

			var deleteResults []*volume_server_pb.DeleteResult
			if c.deleteFileIds != nil {
				deleteResults = c.deleteFileIds(server, fidList)
			} else {
				deleteResults = operation.DeleteFileIdsAtOneVolumeServer(server, c.env.option.GrpcDialOption, fidList, false)
			}
			resultChan <- serverDeleteResults{server: server, results: deleteResults}
		}(server, fileIds)
	}
	wg.Wait()
	close(resultChan)

	expected := make(map[string]int, len(fileIds))
	for _, fileId := range fileIds {
		expected[fileId]++
	}
	var purgeErrors []error
	for response := range resultChan {
		seen := make(map[string]int, len(response.results))
		for resultIndex, result := range response.results {
			if result == nil {
				purgeErrors = append(purgeErrors, fmt.Errorf("server %s returned nil result at index %d", response.server, resultIndex))
				continue
			}
			if expected[result.FileId] == 0 {
				purgeErrors = append(purgeErrors, fmt.Errorf("server %s returned result for unexpected file %q", response.server, result.FileId))
				continue
			}
			seen[result.FileId]++
			if seen[result.FileId] > expected[result.FileId] {
				purgeErrors = append(purgeErrors, fmt.Errorf("server %s returned too many results for file %q", response.server, result.FileId))
			}
			if result.Error != "" {
				purgeErrors = append(purgeErrors, fmt.Errorf("server %s failed to purge file %q: %s", response.server, result.FileId, result.Error))
			} else if result.Status != http.StatusAccepted && result.Status != http.StatusNotModified {
				purgeErrors = append(purgeErrors, fmt.Errorf("server %s returned status %d for file %q", response.server, result.Status, result.FileId))
			}
		}
		for fileId, count := range expected {
			if seen[fileId] < count {
				purgeErrors = append(purgeErrors, fmt.Errorf("server %s returned %d of %d results for file %q", response.server, seen[fileId], count, fileId))
			}
		}
	}
	if len(purgeErrors) == 0 {
		return nil
	}
	for _, purgeErr := range purgeErrors {
		fmt.Fprintf(c.writer, "purge error: %s\n", purgeErr)
	}
	return fmt.Errorf("incomplete purge for volume %d: %w", volumeId, errors.Join(purgeErrors...))
}

func (c *commandVolumeFsck) getCollectFilerFilePath() string {
	return c.scopedFilerPath
}

func (c *commandVolumeFsck) resolveScopedFilerPath(dataNodeVolumeIdToVInfo map[string]map[uint32]VInfo) string {
	if *c.collection != "" {
		return fmt.Sprintf("%s/%s", c.bucketsPath, *c.collection)
	}
	if len(c.volumeIds) == 0 {
		return "/"
	}
	collections := make(map[string]struct{})
	for _, vidMap := range dataNodeVolumeIdToVInfo {
		for vid, vinfo := range vidMap {
			if _, ok := c.volumeIds[vid]; !ok {
				continue
			}
			// empty collection: volume can be referenced from anywhere
			if vinfo.collection == "" {
				return "/"
			}
			collections[vinfo.collection] = struct{}{}
			if len(collections) > 1 {
				return "/"
			}
		}
	}
	if len(collections) != 1 {
		return "/"
	}
	var collection string
	for col := range collections {
		collection = col
	}
	exists, err := c.bucketDirExists(collection)
	if err != nil || !exists {
		return "/"
	}
	return fmt.Sprintf("%s/%s", c.bucketsPath, collection)
}

func (c *commandVolumeFsck) bucketDirExists(name string) (bool, error) {
	var found bool
	err := c.env.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
		resp, err := client.LookupDirectoryEntry(context.Background(), &filer_pb.LookupDirectoryEntryRequest{
			Directory: c.bucketsPath,
			Name:      name,
		})
		if err != nil {
			if strings.Contains(err.Error(), filer_pb.ErrNotFound.Error()) {
				return nil
			}
			return err
		}
		if resp.Entry != nil && resp.Entry.IsDirectory {
			found = true
		}
		return nil
	})
	return found, err
}

func getVolumeFileIdFile(tempFolder string, dataNodeid string, vid uint32) string {
	return filepath.Join(tempFolder, fmt.Sprintf("%s_%d.idx", dataNodeid, vid))
}

func getFilerFileIdFile(tempFolder string, vid uint32) string {
	return filepath.Join(tempFolder, fmt.Sprintf("%d.fid", vid))
}

func writeToFile(bytes []byte, fileName string) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	dst, err := os.OpenFile(fileName, flags, 0644)
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = dst.Write(bytes)
	return err
}
