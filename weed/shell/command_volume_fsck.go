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
	"github.com/seaweedfs/seaweedfs/weed/util/wildcard"
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
	collectionMatcher         *wildcard.CollectionMatcher
	knownVolumeIds            map[uint32]bool
	volumeIds                 map[uint32]bool
	tempFolder                string
	verbose                   *bool
	forcePurging              *bool
	purgeConcurrency          *int
	skipEcVolumes             *bool
	findMissingChunksInFiler  *bool
	verifyNeedle              *bool
	filerSigningKey           string
	unresolvedManifestEntries atomic.Int64
	// tombstonedReferenced counts filer-referenced needles live on one replica
	// and tombstoned on another; a deletion sync would remove the live copy.
	tombstonedReferenced int
	// Missing-reference scans may encounter chunks outside the bucket's
	// collection. Keep their real locations before filtering the topology;
	// lazily inspect those referenced volumes instead of calling them absent.
	outsideCollection map[uint32]map[string]VInfo
	// readNeedleMeta returns a needle's append time as the volume server
	// reads it at the copied index offset; tests replace it.
	readNeedleMeta func(server pb.ServerAddress, volumeId uint32, n needle_map.NeedleValue) (appendAtNs uint64, err error)
	// Test seams for purge transport; production uses MasterClient and operation.
	volumeServers func(volumeId uint32) (servers []pb.ServerAddress, found bool)
	deleteFileIds func(server pb.ServerAddress, fileIds []string) []*volume_server_pb.DeleteResult
	// Repair transport for -repairMissingFromReplicas; tests replace it.
	readNeedleBlob          func(server pb.ServerAddress, volumeId uint32, n needle_map.NeedleValue) ([]byte, error)
	writeNeedleBlobIfAbsent func(server pb.ServerAddress, volumeId uint32, n needle_map.NeedleValue, blob []byte) error
	walkFiler               func(path string, genFn func(ctx context.Context, entry *filer_pb.FullEntry, outputChan chan interface{}) error, saveFn func(outputChan chan interface{}) error) error
	purgedDirsLock          sync.Mutex
	purgedDirs              map[util.FullPath]struct{}
	// compaction revision of each copied index, by dataNodeId and volume
	indexRevisionsLock sync.Mutex
	indexRevisions     map[string]map[uint32]uint32
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

	With -repairMissingFromReplicas, each chunk in B that a replica lacks is
	copied to that replica from one holding it. The filer reference is the
	proof it is a missed write rather than a deletion; the copy uses an
	absent-only write, so existing needles and tombstones are never replaced.
	Without -findMissingChunksInFiler the same filer walk then reports, and
	with -reallyDeleteFromVolume purges, A subtract B: one walk does both.

	-maxOrphanPercent refuses -reallyDeleteFromVolume when orphans exceed that
	share of all entries: a walk that missed part of the namespace reports
	live chunks as orphans.

	-cutoffTimeAgo is used to only check chunks older than the cutoff time.
	This is important because:
		Chunks are uploaded to volume servers before metadata is committed to filer.
		A newly uploaded chunk may appear as orphan if metadata commit is still pending.
		The default 5h cutoff provides sufficient buffer for metadata commits.

	Step 2 reads the filer from one consistent snapshot when its store supports
	that (the SQL stores do), so a file renamed while the filer is being read is
	still counted and its chunks are not mistaken for orphans. Other stores are
	walked directory by directory; there, avoid running with -reallyDeleteFromVolume
	while files are being renamed or moved.

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
	c.collection = fsckCommand.String("collection", "", "comma-separated collection names, wildcards, or regex patterns; empty or \"*\" matches all, \""+CollectionDefault+"\" the collection with no name")
	volumeIds := fsckCommand.String("volumeId", "", "comma separated the volume id; the filer is still read from its root, since a file under any path may reference these volumes")
	applyPurging := fsckCommand.Bool("reallyDeleteFromVolume", false, "<expert only!> after detection, delete missing data from volumes / delete missing file entries from filer. Currently this only works with default filerGroup.")
	c.forcePurging = fsckCommand.Bool("forcePurging", false, "delete missing data from volumes in one replica used together with applyPurging")
	purgeAbsent := fsckCommand.Bool("reallyDeleteFilerEntries", false, "<expert only!> delete missing file entries from filer if the corresponding volume is missing for any reason, please ensure all still existing/expected volumes are connected! used together with findMissingChunksInFiler")
	c.purgeConcurrency = fsckCommand.Int("purgeConcurrency", 16, "parallel filer deletes for -reallyDeleteFilerEntries")
	tempPath := fsckCommand.String("tempPath", path.Join(os.TempDir()), "path for temporary idx files")
	cutoffTimeAgo := fsckCommand.Duration("cutoffTimeAgo", 5*time.Hour, "only include entries on volume servers before this cutoff time to check orphan chunks")
	modifyTimeAgo := fsckCommand.Duration("modifyTimeAgo", 0, "only include entries after this modify time to check orphan chunks")
	c.verifyNeedle = fsckCommand.Bool("verifyNeedles", false, "check needles status from volume server")
	repairFromReplicas := fsckCommand.Bool("repairMissingFromReplicas", false, "copy each filer-referenced needle a replica lacks from a replica that holds it, never overwriting or undeleting; without -findMissingChunksInFiler the orphan check follows in the same filer walk")
	maxOrphanPercent := fsckCommand.Float64("maxOrphanPercent", 100, "with -reallyDeleteFromVolume: purge nothing and fail when orphans exceed this percentage of all entries")

	if err = fsckCommand.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *repairFromReplicas && *c.findMissingChunksInFiler && (*applyPurging || *purgeAbsent) {
		return fmt.Errorf("-findMissingChunksInFiler -repairMissingFromReplicas cannot be combined with -reallyDeleteFromVolume or -reallyDeleteFilerEntries, which would delete filer entries")
	}
	if math.IsNaN(*maxOrphanPercent) || *maxOrphanPercent < 0 || *maxOrphanPercent > 100 {
		return fmt.Errorf("-maxOrphanPercent %v is not between 0 and 100", *maxOrphanPercent)
	}

	// The command struct is a singleton registered in init(), so any state
	// not bound to a flag persists across shell invocations. Reset the
	// unresolved-manifest counter so a previous failed run can't permanently
	// suppress -reallyDeleteFromVolume in this session.
	c.unresolvedManifestEntries.Store(0)
	c.tombstonedReferenced = 0
	c.purgedDirs = make(map[util.FullPath]struct{})
	c.indexRevisions = nil

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
	c.writer = &lockedWriter{w: writer}

	if c.collectionMatcher, err = wildcard.CompileCollectionMatcher(*c.collection); err != nil {
		return fmt.Errorf("invalid collection pattern %q: %w", *c.collection, err)
	}

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

	// Every volume the master knows about, before any filtering: a filer
	// entry pointing at a volume that was filtered out of this run is
	// excluded, not missing — it must never be reported or purged.
	c.knownVolumeIds = make(map[uint32]bool)
	for _, vidMap := range dataNodeVolumeIdToVInfo {
		for vid := range vidMap {
			c.knownVolumeIds[vid] = true
		}
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

	c.outsideCollection = make(map[uint32]map[string]VInfo)
	if *c.findMissingChunksInFiler && *c.collection != "" {
		for node, volumes := range dataNodeVolumeIdToVInfo {
			for vid, info := range volumes {
				if c.collectionMatcher.Matches(info.collection) {
					continue
				}
				if c.outsideCollection[vid] == nil {
					c.outsideCollection[vid] = make(map[string]VInfo)
				}
				c.outsideCollection[vid][node] = info
			}
		}
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
				if !c.collectionMatcher.Matches(vinfo.collection) {
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

	return c.checkVolumes(dataNodeVolumeIdToVInfo, fsckChecks{
		applyPurging:     *applyPurging,
		purgeAbsent:      *purgeAbsent,
		repair:           *repairFromReplicas,
		modifyFromAtNs:   collectModifyFromAtNs,
		cutoffFromAtNs:   collectCutoffFromAtNs,
		maxOrphanPercent: *maxOrphanPercent,
	})
}

// fsckChecks are the parsed actions for checkVolumes.
type fsckChecks struct {
	applyPurging   bool
	purgeAbsent    bool
	repair         bool
	modifyFromAtNs int64
	cutoffFromAtNs int64
	// with applyPurging: purge only when orphans are at most this share
	// of all entries
	maxOrphanPercent float64
}

// referenceProvesWrite reports whether a filer reference written at
// modifiedTsNs is old enough to prove its chunk was written, rather than
// uploaded and still awaiting the metadata commit.
func (checks fsckChecks) referenceProvesWrite(modifiedTsNs int64) bool {
	return (checks.cutoffFromAtNs == 0 || modifiedTsNs <= checks.cutoffFromAtNs) &&
		(checks.modifyFromAtNs == 0 || modifiedTsNs >= checks.modifyFromAtNs)
}

// checkVolumes walks the filer once, the volume indexes having been copied
// into the temp folder, then runs the requested checks against them.
func (c *commandVolumeFsck) checkVolumes(dataNodeVolumeIdToVInfo map[string]map[uint32]VInfo, checks fsckChecks) (err error) {
	if *c.findMissingChunksInFiler {
		// collect all filer file ids and paths

		var queuePurge func(purgeItem)
		var finishPurge func() error
		if checks.applyPurging || checks.purgeAbsent {
			queuePurge, finishPurge = startPurgeWorkers(*c.purgeConcurrency, c.httpDelete)
		}
		err = func() error {
			if err := c.collectFilerFileIdAndPaths(dataNodeVolumeIdToVInfo, checks, true, queuePurge); err != nil {
				return fmt.Errorf("collectFilerFileIdAndPaths: %w", err)
			}
			if checks.repair {
				return nil
			}
			for dataNodeId, volumeIdToVInfo := range dataNodeVolumeIdToVInfo {
				// for each volume, check filer file ids
				if err := c.findFilerChunksMissingInVolumeServers(volumeIdToVInfo, dataNodeId, queuePurge); err != nil {
					return fmt.Errorf("findFilerChunksMissingInVolumeServers: %w", err)
				}
			}
			return nil
		}()
		if finishPurge != nil {
			// A purge that failed leaves the run incomplete.
			if purgeErr := finishPurge(); err == nil && purgeErr != nil {
				err = fmt.Errorf("purge: %w", purgeErr)
			}
		}
		if err != nil {
			return err
		}
		if checks.repair {
			return c.repairAndReport(dataNodeVolumeIdToVInfo, getFilerFileIdFile)
		}
		c.purgeEmptyDirectories()
	} else {
		// collect all filer file ids: any reference, however new, keeps
		// its needle; only those old enough to prove a write are repaired
		if err = c.collectFilerFileIdAndPaths(dataNodeVolumeIdToVInfo, checks, false, nil); err != nil {
			return fmt.Errorf("failed to collect file ids from filer: %w", err)
		}
		if checks.repair {
			if err = c.repairAndReport(dataNodeVolumeIdToVInfo, getFilerRepairFileIdFile); err != nil {
				return err
			}
		}
		// If any entry's manifest could not be resolved, our in-use fid set
		// is missing the sub-chunks behind it. Purging orphans now would
		// delete live data referenced only via the unresolved manifest, so
		// disable -reallyDeleteFromVolume for this run and tell the operator
		// to fix the broken entries first.
		applyPurgingEffective := checks.applyPurging
		if unresolved := c.unresolvedManifestEntries.Load(); unresolved > 0 && applyPurgingEffective {
			fmt.Fprintf(c.writer, "WARNING: %d entry(ies) had unresolvable chunk manifests; refusing to apply -reallyDeleteFromVolume to avoid deleting live sub-chunks. Fix the entries listed above (e.g. delete or repair them) and re-run.\n",
				unresolved)
			applyPurgingEffective = false
		}
		// volume file ids subtract filer file ids
		extraErr := c.findExtraChunksInVolumeServers(dataNodeVolumeIdToVInfo, applyPurgingEffective, uint64(checks.modifyFromAtNs), uint64(checks.cutoffFromAtNs), checks.maxOrphanPercent)
		if err := c.runError(extraErr); err != nil {
			return err
		}
	}

	return nil
}

// runError assembles the run's failure. A tombstoned referenced needle is
// reported first, so a caller matching only the leading error does not take
// this run for one that is safe to sync deletions after.
func (c *commandVolumeFsck) runError(extraErr error) error {
	var errs []string
	if c.tombstonedReferenced > 0 {
		errs = append(errs, fmt.Sprintf("%d referenced needle(s) are tombstoned on a replica while live on another", c.tombstonedReferenced))
	}
	if unresolved := c.unresolvedManifestEntries.Load(); unresolved > 0 {
		errs = append(errs, fmt.Sprintf("incomplete fsck: %d entries have unresolved chunk manifests; no purge was authorized", unresolved))
	}
	if extraErr != nil {
		errs = append(errs, fmt.Sprintf("findExtraChunksInVolumeServers: %v", extraErr))
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

func (c *commandVolumeFsck) repairAndReport(dataNodeVolumeIdToVInfo map[string]map[uint32]VInfo, refsFile func(tempFolder string, vid uint32) string) error {
	repaired, lost, err := c.repairMissingFromReplicas(dataNodeVolumeIdToVInfo, refsFile)
	fmt.Fprintf(c.writer, "repaired %d missing replica needle(s); %d referenced needle(s) on no replica; %d referenced needle(s) tombstoned on a replica\n", repaired, lost, c.tombstonedReferenced)
	return err
}

// collectFilerFileIdAndPaths writes each volume's filer references to its
// .fid file. With onlyProvenWrites, references too new to prove a write are
// left out; otherwise they are all kept, and with checks.repair the proven
// ones are also written to the volume's .repair.fid file. queuePurge, when
// set, receives the entries of volumes that no longer exist.
func (c *commandVolumeFsck) collectFilerFileIdAndPaths(dataNodeVolumeIdToVInfo map[string]map[uint32]VInfo, checks fsckChecks, onlyProvenWrites bool, queuePurge func(purgeItem)) error {
	purgeAbsent := checks.purgeAbsent
	repairRefs := checks.repair && !onlyProvenWrites
	if *c.verbose {
		fmt.Fprintf(c.writer, "checking each file from filer path /...\n")
	}

	files := make(map[uint32]*os.File)
	repairFiles := make(map[uint32]*os.File)
	defer func() {
		for _, f := range files {
			f.Close()
		}
		for _, f := range repairFiles {
			f.Close()
		}
	}()
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
			if repairRefs {
				dst, openErr = os.OpenFile(getFilerRepairFileIdFile(c.tempFolder, vid), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
				if openErr != nil {
					return fmt.Errorf("failed to create file %s: %v", getFilerRepairFileIdFile(c.tempFolder, vid), openErr)
				}
				repairFiles[vid] = dst
			}
		}
	}

	return c.traverseFiler("/",
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
				// An unreadable manifest hides sub-chunk references. Keep its top-level
				// fids for reporting and block purging with the unresolved counter.
				fmt.Fprintf(c.writer, "WARNING: ResolveChunkManifest failed for %s: %v — recording top-level chunk fids only; purging will be disabled\n",
					util.NewFullPath(entry.Dir, entry.Entry.Name), resolveErr)
				c.unresolvedManifestEntries.Add(1)
				dataChunks = entry.Entry.GetChunks()
				manifestChunks = nil
			}
			dataChunks = append(dataChunks, manifestChunks...)
			for _, chunk := range dataChunks {
				provesWrite := checks.referenceProvesWrite(chunk.ModifiedTsNs)
				if onlyProvenWrites && !provesWrite {
					continue
				}
				select {
				case outputChan <- &Item{
					vid:         chunk.Fid.VolumeId,
					fileKey:     chunk.Fid.FileKey,
					cookie:      chunk.Fid.Cookie,
					path:        util.NewFullPath(entry.Dir, entry.Entry.Name),
					hardLinkId:  entry.Entry.GetHardLinkId(),
					provesWrite: provesWrite,
				}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
		func(outputChan chan interface{}) error {
			buffer := make([]byte, readbufferSize)
			var linkIdSize [4]byte
			for item := range outputChan {
				i := item.(*Item)
				if *c.findMissingChunksInFiler && files[i.vid] == nil &&
					(len(c.volumeIds) == 0 || c.volumeIds[i.vid]) && len(c.outsideCollection[i.vid]) > 0 {
					// The bucket can legitimately reference a shared/default
					// collection. A filter exclusion is not a missing volume,
					// especially when purgeAbsent would delete the intact file.
					for node, info := range c.outsideCollection[i.vid] {
						if *c.skipEcVolumes && info.isEcVolume {
							return fmt.Errorf("referenced volume %d excluded by -skipEcVolumes; scan incomplete", i.vid)
						}
						if err := c.collectOneVolumeFileIds(node, i.vid, info); err != nil {
							return fmt.Errorf("collect referenced cross-collection volume %d on %s: %w", i.vid, node, err)
						}
						dataNodeVolumeIdToVInfo[node][i.vid] = info
					}
					f, err := os.OpenFile(getFilerFileIdFile(c.tempFolder, i.vid), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
					if err != nil {
						return fmt.Errorf("create cross-collection reference file for volume %d: %w", i.vid, err)
					}
					files[i.vid] = f
				}
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
					util.Uint32toBytes(linkIdSize[:], uint32(len(i.hardLinkId)))
					if _, err := f.Write(linkIdSize[:]); err != nil {
						return err
					}
					if _, err := f.Write(i.hardLinkId); err != nil {
						return err
					}
					if rf := repairFiles[i.vid]; rf != nil && i.provesWrite {
						if _, err := rf.Write(buffer); err != nil {
							return err
						}
						if _, err := rf.Write([]byte(i.path)); err != nil {
							return err
						}
						if _, err := rf.Write(linkIdSize[:]); err != nil {
							return err
						}
						if _, err := rf.Write(i.hardLinkId); err != nil {
							return err
						}
					}
				} else if *c.findMissingChunksInFiler {
					// check if the volume matches the filter
					if len(c.volumeIds) > 0 {
						if _, ok := c.volumeIds[i.vid]; !ok {
							continue
						}
					}
					// A volume the master knows but this run filtered out is not a
					// missing volume: its files are healthy and stay untouched.
					if c.knownVolumeIds[i.vid] {
						continue
					}
					fmt.Fprintf(c.writer, "%d,%x%08x %s volume not found\n", i.vid, i.fileKey, i.cookie, i.path)
					if purgeAbsent {
						fmt.Fprintf(c.writer, "deleting path %s after volume not found\n", i.path)
						queuePurge(purgeItem{path: i.path, hardLinkId: i.hardLinkId})
					}
				}
			}
			return nil
		})
}

// traverseFiler walks the filer namespace under path; tests replace walkFiler.
func (c *commandVolumeFsck) traverseFiler(path string, genFn func(ctx context.Context, entry *filer_pb.FullEntry, outputChan chan interface{}) error, saveFn func(outputChan chan interface{}) error) error {
	if c.walkFiler != nil {
		return c.walkFiler(path, genFn, saveFn)
	}
	return doTraverseAndSaving(c.env, c.writer, path, false, false, genFn, saveFn)
}

func (c *commandVolumeFsck) findFilerChunksMissingInVolumeServers(volumeIdToVInfo map[uint32]VInfo, dataNodeId string, queuePurge func(purgeItem)) error {

	for volumeId, vinfo := range volumeIdToVInfo {
		checkErr := c.oneVolumeFileIdsCheckOneVolume(dataNodeId, volumeId, queuePurge)
		if checkErr != nil {
			return fmt.Errorf("failed to collect file ids from volume %d on %s: %v", volumeId, vinfo.server, checkErr)
		}
	}
	return nil
}

func (c *commandVolumeFsck) findExtraChunksInVolumeServers(dataNodeVolumeIdToVInfo map[string]map[uint32]VInfo, applyPurging bool, modifyFrom, cutoffFrom uint64, maxOrphanPercent float64) error {

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
	// A volume whose scan is incomplete on any replica has no trustworthy
	// orphan set, so it is never purged; every other volume still is.
	incompleteVolumes := make(map[uint32]error)
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
				fmt.Fprintf(c.writer, "volume %d on %s: not purging the volume: %v\n", volumeId, vinfo.server, checkErr)
				incompleteVolumes[volumeId] = errors.Join(incompleteVolumes[volumeId], fmt.Errorf("volume %d on %s: %v", volumeId, vinfo.server, checkErr))
				continue
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

	var pct float64
	if totalCount := totalOrphanChunkCount + totalInUseCount; totalCount > 0 {
		pct = float64(totalOrphanChunkCount) * 100 / (float64(totalCount))
	}
	fmt.Fprintf(c.writer, "\nTotal\t\tentries:%d\torphan:%d\t%.2f%%\t%dB\n",
		totalOrphanChunkCount+totalInUseCount, totalOrphanChunkCount, pct, totalOrphanDataSize)
	if applyPurging && pct > maxOrphanPercent {
		fmt.Fprintf(c.writer, "%d orphans of %d entries exceed -maxOrphanPercent %v; purging nothing\n",
			totalOrphanChunkCount, totalOrphanChunkCount+totalInUseCount, maxOrphanPercent)
		return fmt.Errorf("orphans are %.2f%% of entries, above -maxOrphanPercent %v; no purge was authorized", pct, maxOrphanPercent)
	}

	// Phase 2: purge. At most one call to purgeFileIdsForOneVolume per
	// volume — that helper already fans out to all replica locations via
	// MasterClient.GetLocations, so iterating per replica here (as the old
	// code did) would issue N*N delete RPCs for N replicas.
	var purgeErr error
	if applyPurging {
		var skippedVolumeIds []uint32
		for volumeId, orphanReplicaFileIds := range volumeIdOrphanFileIds {
			if len(orphanReplicaFileIds) == 0 {
				continue
			}
			if _, incomplete := incompleteVolumes[volumeId]; incomplete {
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
			purgeErr = fmt.Errorf("incomplete purge on volumes %v", skippedVolumeIds)
		}
	}

	if !applyPurging {
		fmt.Fprintf(c.writer, "This could be normal if multiple filers or no filers are used.\n")
	}

	if totalOrphanChunkCount == 0 {
		fmt.Fprintf(c.writer, "no orphan data\n")
	}

	var scanErr error
	if len(incompleteVolumes) > 0 {
		volumeIds := make([]uint32, 0, len(incompleteVolumes))
		for volumeId := range incompleteVolumes {
			volumeIds = append(volumeIds, volumeId)
		}
		sort.Slice(volumeIds, func(i, j int) bool { return volumeIds[i] < volumeIds[j] })
		errs := make([]error, 0, len(volumeIds))
		for _, volumeId := range volumeIds {
			errs = append(errs, incompleteVolumes[volumeId])
		}
		fmt.Fprintf(c.writer, "incomplete scan on %d volume(s), not purged: %v\n", len(volumeIds), volumeIds)
		scanErr = fmt.Errorf("incomplete scan on volumes %v: %w", volumeIds, errors.Join(errs...))
	}
	if purgeErr != nil && scanErr != nil {
		return fmt.Errorf("%w; %w", purgeErr, scanErr)
	}
	if purgeErr != nil {
		return purgeErr
	}
	return scanErr
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
	if vinfo.isEcVolume {
		return c.copyVolumeIndex(dataNodeId, volumeId, vinfo, math.MaxUint32)
	}
	return c.copyVolumeIndexAtRevision(dataNodeId, volumeId, vinfo)
}

// copyVolumeIndex copies the volume's index into the temp folder; unless
// compactionRevision is math.MaxUint32, the volume server refuses the copy
// once the volume has been compacted past it.
func (c *commandVolumeFsck) copyVolumeIndex(dataNodeId string, volumeId uint32, vinfo VInfo, compactionRevision uint32) error {
	return operation.WithVolumeServerClient(false, vinfo.server, c.env.option.GrpcDialOption,
		func(volumeServerClient volume_server_pb.VolumeServerClient) error {
			ext := ".idx"
			if vinfo.isEcVolume {
				ext = ".ecx"
			}

			copyFileClient, err := volumeServerClient.CopyFile(context.Background(), &volume_server_pb.CopyFileRequest{
				VolumeId:                 volumeId,
				Ext:                      ext,
				CompactionRevision:       compactionRevision,
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
	vid         uint32
	fileKey     uint64
	cookie      uint32
	path        util.FullPath
	hardLinkId  []byte
	provesWrite bool
}

// lockedWriter serializes concurrent writers — the traversal, the consumer,
// and the purge workers all log through c.writer at the same time.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (lw *lockedWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.w.Write(p)
}

func (c *commandVolumeFsck) readFilerFileIdFile(volumeId uint32, fn func(needleId types.NeedleId, itemPath util.FullPath, hardLinkId []byte) error) error {
	return c.readFidFile(getFilerFileIdFile, volumeId, fn)
}

func (c *commandVolumeFsck) readFidFile(refsFile func(tempFolder string, vid uint32) string, volumeId uint32, fn func(needleId types.NeedleId, itemPath util.FullPath, hardLinkId []byte) error) error {
	fp, err := os.Open(refsFile(c.tempFolder, volumeId))
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
		item.hardLinkId = nil
		if _, err := io.ReadFull(br, buffer[:4]); err != nil {
			return fmt.Errorf("read fid hard link id size for volume %d,%x%08x: %w", volumeId, item.fileKey, item.cookie, err)
		}
		if hardLinkIdSize := util.BytesToUint32(buffer[:4]); hardLinkIdSize > 0 {
			item.hardLinkId = make([]byte, int(hardLinkIdSize))
			if _, err := io.ReadFull(br, item.hardLinkId); err != nil {
				return fmt.Errorf("read fid hard link id for volume %d,%x%08x: %w", volumeId, item.fileKey, item.cookie, err)
			}
		}
		needleId := types.NeedleId(item.fileKey)
		if err := fn(needleId, item.path, item.hardLinkId); err != nil {
			return fmt.Errorf("process fid %d,%x%08x path %q: %w", volumeId, item.fileKey, item.cookie, item.path, err)
		}
	}
	return nil
}

func (c *commandVolumeFsck) oneVolumeFileIdsCheckOneVolume(dataNodeId string, volumeId uint32, queuePurge func(purgeItem)) (err error) {
	if *c.verbose {
		fmt.Fprintf(c.writer, "find missing file chunks in dataNodeId %s volume %d ...\n", dataNodeId, volumeId)
	}

	db := needle_map.NewMemDb()
	defer db.Close()

	if err = db.LoadFromIdx(getVolumeFileIdFile(c.tempFolder, dataNodeId, volumeId)); err != nil {
		return
	}
	if err = c.readFilerFileIdFile(volumeId, func(needleId types.NeedleId, itemPath util.FullPath, hardLinkId []byte) error {
		if _, found := db.Get(needleId); !found {
			fmt.Fprintf(c.writer, "%s\n", itemPath)
			if queuePurge != nil {
				queuePurge(purgeItem{path: itemPath, hardLinkId: hardLinkId})
			}
		}
		return nil
	}); err != nil {
		return
	}
	return nil
}

type purgeItem struct {
	path       util.FullPath
	hardLinkId []byte
}

// startPurgeWorkers fans deletes across at least one worker and returns a
// queue function plus a finish function that closes the channels and waits
// for in-flight deletes. Paths that share a hard link id always land on the
// same worker, so their shared hard-link counter updates stay serialized —
// concurrent deletes of the same id could both read count N and write N-1,
// leaving the shared record behind after every path is gone.
//
// finish returns the first delete that failed. After a failure the remaining
// queued deletes are dropped, not attempted: a purge that cannot complete
// stops, as a single failed delete stopped the sequential purge.
func startPurgeWorkers(workers int, deleteFn func(util.FullPath) error) (queue func(purgeItem), finish func() error) {
	if workers < 1 {
		workers = 1
	}
	chans := make([]chan purgeItem, workers)
	var wg sync.WaitGroup
	var failed atomic.Bool
	var firstErrOnce sync.Once
	var firstErr error
	for w := range chans {
		chans[w] = make(chan purgeItem, 128)
		wg.Add(1)
		go func(ch <-chan purgeItem) {
			defer wg.Done()
			for it := range ch {
				if failed.Load() {
					continue
				}
				if err := deleteFn(it.path); err != nil {
					firstErrOnce.Do(func() { firstErr = err })
					failed.Store(true)
				}
			}
		}(chans[w])
	}
	return func(it purgeItem) {
			key := []byte(it.path)
			if len(it.hardLinkId) > 0 {
				key = it.hardLinkId
			}
			chans[int(uint32(util.HashToInt32(key))%uint32(workers))] <- it
		}, func() error {
			for _, ch := range chans {
				close(ch)
			}
			wg.Wait()
			return firstErr
		}
}

func (c *commandVolumeFsck) httpDelete(path util.FullPath) error {
	req, err := http.NewRequest(http.MethodDelete, "", nil)
	if err != nil {
		return fmt.Errorf("create HTTP DELETE request for %q: %w", path, err)
	}

	req.URL = &url.URL{
		Scheme: "http",
		Host:   c.env.FilerAddress().ToHttpAddress(),
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
		if err := c.deleteEmptyDirectory(dir, entry.Attributes); err != nil {
			if !errors.Is(err, filer.ErrNonEmptyFolder) {
				fmt.Fprintf(c.writer, "delete empty directory %s: %v\n", dir, err)
			}
			continue
		}
		fmt.Fprintf(c.writer, "deleted empty directory %s\n", dir)
	}
}

// deleteEmptyDirectory deletes dir unless it changed since it was looked up,
// so a directory promoted to an S3 object meanwhile survives. The delete is
// not recursive, leaving the filer to reject a directory that is not empty.
func (c *commandVolumeFsck) deleteEmptyDirectory(dir util.FullPath, attr *filer_pb.FuseAttributes) error {
	parent, name := dir.DirAndName()
	sec, ns, ok := deleteEntryCutoff(attr.GetMtime(), attr.GetMtimeNs())
	if !ok {
		return nil
	}
	return c.env.WithFilerClient(false, func(client filer_pb.SeaweedFilerClient) error {
		resp, err := client.DeleteEntry(context.Background(), &filer_pb.DeleteEntryRequest{
			Directory:            parent,
			Name:                 name,
			IfNotModifiedAfter:   sec,
			IfNotModifiedAfterNs: ns,
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
	if string(dir) == "/" {
		return false
	}
	// deleting a bucket drops its whole collection
	parent, _ := dir.DirAndName()
	return string(dir) != c.bucketsPath && parent != c.bucketsPath
}

func (c *commandVolumeFsck) oneVolumeFileIdsSubtractFilerFileIds(dataNodeId string, volumeId uint32, vinfo *VInfo, modifyFrom, cutoffFrom uint64) (inUseCount uint64, orphanFileIds []string, orphanDataSize uint64, err error) {
	var staleNeedleCount uint64
	for recopies := 0; ; recopies++ {
		inUseCount, orphanFileIds, orphanDataSize, staleNeedleCount, err = c.subtractFilerFileIdsFromIndex(dataNodeId, volumeId, vinfo, modifyFrom, cutoffFrom)
		if err != nil || staleNeedleCount == 0 || recopies == fsckIndexRecopyAttempts {
			break
		}
		// A vacuum since the copy moves needles off their copied offsets;
		// only then is a fresh index a reason to read them again.
		if recopyErr := c.recopyCompactedIndex(dataNodeId, volumeId, *vinfo); recopyErr != nil {
			fmt.Fprintf(c.writer, "dataNode:%s\tvolume:%d\tnot copying the index again: %v\n", dataNodeId, volumeId, recopyErr)
			break
		}
	}
	if err != nil {
		return
	}

	if orphanCount := uint64(len(orphanFileIds)); orphanCount > 0 {
		pct := float64(orphanCount*100) / (float64(orphanCount + inUseCount))
		fmt.Fprintf(c.writer, "dataNode:%s\tvolume:%d\tentries:%d\torphan:%d\t%.2f%%\t%dB\n",
			dataNodeId, volumeId, orphanCount+inUseCount, orphanCount, pct, orphanDataSize)
	}
	if staleNeedleCount > 0 {
		fmt.Fprintf(c.writer, "dataNode:%s\tvolume:%d\t%d needle(s) have unknown metadata; scan incomplete, refusing purge; investigate and rerun\n",
			dataNodeId, volumeId, staleNeedleCount)
		return inUseCount, nil, 0, fmt.Errorf("incomplete fsck for volume %d: %d needle metadata reads failed", volumeId, staleNeedleCount)
	}
	return
}

// subtractFilerFileIdsFromIndex lists the needles of the copied index that
// no filer reference keeps, counting those whose metadata could not be read.
func (c *commandVolumeFsck) subtractFilerFileIdsFromIndex(dataNodeId string, volumeId uint32, vinfo *VInfo, modifyFrom, cutoffFrom uint64) (inUseCount uint64, orphanFileIds []string, orphanDataSize uint64, staleNeedleCount uint64, err error) {

	volumeFileIdDb := needle_map.NewMemDb()
	defer volumeFileIdDb.Close()

	if err = volumeFileIdDb.LoadFromIdx(getVolumeFileIdFile(c.tempFolder, dataNodeId, volumeId)); err != nil {
		err = fmt.Errorf("failed to LoadFromIdx %+v", err)
		return
	}

	if err = c.readFilerFileIdFile(volumeId, func(filerNeedleId types.NeedleId, itemPath util.FullPath, _ []byte) error {
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
			orphanDataSize += uint64(n.Size)
		}
		return nil
	}); err != nil {
		err = fmt.Errorf("failed to AscendingVisit %+v", err)
		return
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
	if err = requireServerIndexLayout(topologyVolumeServers(topologyInfo), volumeServerVersion(c.env.option.GrpcDialOption)); err != nil {
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
				deleteResults = operation.DeleteFileIdsAtOneVolumeServer(context.Background(), server, c.env.option.GrpcDialOption, fidList, false)
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

func getVolumeFileIdFile(tempFolder string, dataNodeid string, vid uint32) string {
	return filepath.Join(tempFolder, fmt.Sprintf("%s_%d.idx", dataNodeid, vid))
}

func getFilerFileIdFile(tempFolder string, vid uint32) string {
	return filepath.Join(tempFolder, fmt.Sprintf("%d.fid", vid))
}

func getFilerRepairFileIdFile(tempFolder string, vid uint32) string {
	return filepath.Join(tempFolder, fmt.Sprintf("%d.repair.fid", vid))
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
