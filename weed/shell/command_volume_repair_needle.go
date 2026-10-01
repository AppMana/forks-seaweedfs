package shell

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"path"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/operation"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/protobuf/proto"
)

func init() { Commands = append(Commands, &commandVolumeRepairNeedle{}) }

type commandVolumeRepairNeedle struct{}

func (*commandVolumeRepairNeedle) Name() string           { return "volume.repair.needle" }
func (*commandVolumeRepairNeedle) HasTag(CommandTag) bool { return false }
func (*commandVolumeRepairNeedle) Help() string {
	return `Repair one absent replica record for a verified immutable single-chunk filer file.
 volume.repair.needle -path /absolute/file -fid 123,keycookie -sha256 <payload hash> -source host:port -target host:port [-apply]
 Dry-run by default. Requires the admin lock, but the lock does NOT stop application writes.
 Quiesce changes to this immutable file during repair. Existing records and tombstones
 are never overwritten. Old servers refuse the distinct repair RPC; no unsafe fallback.
 A changed reference after repair reports an uncertain result without destructive rollback.
 Encrypted, manifest, multi-chunk, inline, TTL and files over 64 MiB are unsupported.
`
}

const maxRepairPayload = 64 * 1024 * 1024

// Replication assigns a fresh local append timestamp in v3. Every other byte,
// including framing, identity, metadata, checksum and padding, must survive.
func sameRepairRecord(source, target []byte, size types.Size, version needle.Version) bool {
	if size < 0 || int64(len(source)) != needle.GetActualSize(size, version) || len(source) != len(target) {
		return false
	}
	if version != needle.Version3 {
		return bytes.Equal(source, target)
	}
	stamp := types.NeedleHeaderSize + int(size) + needle.NeedleChecksumSize
	if stamp+types.TimestampSize > len(source) {
		return false
	}
	return bytes.Equal(source[:stamp], target[:stamp]) && bytes.Equal(source[stamp+types.TimestampSize:], target[stamp+types.TimestampSize:])
}

func validateRepairReference(e *filer_pb.Entry, fid *needle.FileId) (uint64, error) {
	if e == nil || e.IsDirectory || len(e.Content) != 0 || e.Attributes == nil || len(e.Chunks) != 1 {
		return 0, fmt.Errorf("repair requires one complete, non-inline file chunk")
	}
	c := e.Chunks[0]
	if c == nil || c.IsChunkManifest || len(c.CipherKey) != 0 || c.Offset != 0 || c.Size == 0 || c.Size > maxRepairPayload || c.Size != e.Attributes.FileSize {
		return 0, fmt.Errorf("unsupported or inconsistent filer chunk layout")
	}
	// GetFileIdString caches its result in FileId; do not mutate the entry
	// whose exact metadata is compared again before and after repair.
	actual, err := needle.ParseFileIdFromString(proto.Clone(c).(*filer_pb.FileChunk).GetFileIdString())
	if err != nil || *actual != *fid {
		return 0, fmt.Errorf("current filer reference does not match pinned file id")
	}
	return c.Size, nil
}

func validateRepairPayload(n *needle.Needle, fid *needle.FileId, size uint64, hash string) error {
	if n.Id != fid.Key || n.Cookie != fid.Cookie || n.HasTtl() || n.IsChunkedManifest() {
		return fmt.Errorf("source identity or record flags not safe for repair")
	}
	if size == 0 || size > maxRepairPayload {
		return fmt.Errorf("payload size outside repair limit")
	}
	data := n.Data
	if n.IsCompressed() {
		r, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return err
		}
		data, err = io.ReadAll(io.LimitReader(r, int64(size)+1))
		r.Close()
		if err != nil {
			return err
		}
	}
	if uint64(len(data)) != size || fmt.Sprintf("%x", sha256.Sum256(data)) != hash {
		return fmt.Errorf("source payload size or SHA-256 differs from pinned file")
	}
	return nil
}

// repairIndex snapshots only the index length, preserving the final mapping
// (including tombstones). It never interprets an RPC/read error as absence.
func repairIndex(ctx context.Context, c volume_server_pb.VolumeServerClient, fid *needle.FileId) (*volume_server_pb.ReadVolumeFileStatusResponse, types.Offset, types.Size, bool, error) {
	s, err := c.ReadVolumeFileStatus(ctx, &volume_server_pb.ReadVolumeFileStatusRequest{VolumeId: uint32(fid.VolumeId)})
	if err != nil {
		return nil, types.Offset{}, 0, false, err
	}
	const row = types.NeedleIdSize + types.OffsetSize + types.SizeSize
	if s.VolumeId != uint32(fid.VolumeId) || s.IdxFileSize%row != 0 || s.Version < 2 || s.Version > 3 {
		return nil, types.Offset{}, 0, false, fmt.Errorf("unsupported volume identity, version or index width")
	}
	stream, err := c.CopyFile(ctx, &volume_server_pb.CopyFileRequest{VolumeId: uint32(fid.VolumeId), Ext: ".idx", Collection: s.Collection, CompactionRevision: s.CompactionRevision, StopOffset: s.IdxFileSize})
	if err != nil {
		return nil, types.Offset{}, 0, false, err
	}
	var pending []byte
	var count uint64
	var offset types.Offset
	var size types.Size
	var found bool
	for {
		reply, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, offset, 0, false, err
		}
		count += uint64(len(reply.FileContent))
		if count > s.IdxFileSize {
			return nil, offset, 0, false, fmt.Errorf("index exceeded snapshot length")
		}
		pending = append(pending, reply.FileContent...)
		for len(pending) >= row {
			if types.BytesToNeedleId(pending[:types.NeedleIdSize]) == fid.Key {
				offset = types.BytesToOffset(pending[types.NeedleIdSize:])
				size = types.BytesToSize(pending[types.NeedleIdSize+types.OffsetSize:])
				found = true
			}
			pending = pending[row:]
		}
	}
	if count != s.IdxFileSize || len(pending) != 0 {
		return nil, offset, 0, false, fmt.Errorf("incomplete index snapshot")
	}
	after, err := c.ReadVolumeFileStatus(ctx, &volume_server_pb.ReadVolumeFileStatusRequest{VolumeId: uint32(fid.VolumeId)})
	if err != nil {
		return nil, offset, 0, false, err
	}
	if after.CompactionRevision != s.CompactionRevision || after.Version != s.Version || after.Collection != s.Collection {
		return nil, offset, 0, false, fmt.Errorf("volume changed during index snapshot")
	}
	return s, offset, size, found, nil
}

func (cmd *commandVolumeRepairNeedle) Do(args []string, env *CommandEnv, w io.Writer) error {
	flags := flag.NewFlagSet(cmd.Name(), flag.ContinueOnError)
	filePath := flags.String("path", "", "immutable filer file path")
	fileID := flags.String("fid", "", "expected file id including cookie")
	hash := flags.String("sha256", "", "expected uncompressed payload SHA-256")
	source := flags.String("source", "", "source volume server")
	target := flags.String("target", "", "target volume server")
	apply := flags.Bool("apply", false, "perform absent-only repair after validation")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	digest, err := hex.DecodeString(*hash)
	if err != nil || len(digest) != sha256.Size || fmt.Sprintf("%x", digest) != *hash || flags.NArg() != 0 || *filePath == "/" || !path.IsAbs(*filePath) || path.Clean(*filePath) != *filePath || *source == "" || *target == "" || *source == *target {
		return fmt.Errorf("require canonical absolute path, lowercase SHA-256, and distinct source/target")
	}
	fid, err := needle.ParseFileIdFromString(*fileID)
	if err != nil {
		return err
	}
	if err = env.confirmIsLocked(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	lookup := func() (*filer_pb.Entry, error) {
		var entry *filer_pb.Entry
		err := env.WithFilerClient(false, func(c filer_pb.SeaweedFilerClient) error {
			dir, name := util.FullPath(*filePath).DirAndName()
			r, err := filer_pb.LookupEntry(ctx, c, &filer_pb.LookupDirectoryEntryRequest{Directory: dir, Name: name})
			if err != nil {
				return err
			}
			entry = r.Entry
			return nil
		})
		return entry, err
	}
	original, err := lookup()
	if err != nil {
		return err
	}
	expectedSize, err := validateRepairReference(original, fid)
	if err != nil {
		return err
	}
	return operation.WithVolumeServerClient(false, pb.ServerAddress(*source), env.option.GrpcDialOption, func(sc volume_server_pb.VolumeServerClient) error {
		sourceState, offset, size, found, err := repairIndex(ctx, sc, fid)
		if err != nil {
			return fmt.Errorf("source index: %w", err)
		}
		if !found || size <= 0 || offset.IsZero() || int64(size) > maxRepairPayload+1024*1024 {
			return fmt.Errorf("source has no bounded live record")
		}
		blob, err := sc.ReadNeedleBlob(ctx, &volume_server_pb.ReadNeedleBlobRequest{VolumeId: uint32(fid.VolumeId), Offset: offset.ToActualOffset(), Size: int32(size)})
		if err != nil {
			return err
		}
		version := needle.Version(sourceState.Version)
		if int64(len(blob.NeedleBlob)) != needle.GetActualSize(size, version) {
			return fmt.Errorf("source record length mismatch")
		}
		var decoded needle.Needle
		if err = decoded.ReadBytes(blob.NeedleBlob, offset.ToActualOffset(), size, version); err != nil {
			return err
		}
		if err = validateRepairPayload(&decoded, fid, expectedSize, *hash); err != nil {
			return err
		}
		return operation.WithVolumeServerClient(false, pb.ServerAddress(*target), env.option.GrpcDialOption, func(tc volume_server_pb.VolumeServerClient) error {
			targetState, _, _, exists, err := repairIndex(ctx, tc, fid)
			if err != nil {
				return fmt.Errorf("target index: %w", err)
			}
			if exists {
				return fmt.Errorf("target has an existing entry or tombstone; refusing overwrite")
			}
			if targetState.Version != sourceState.Version || targetState.Collection != sourceState.Collection {
				return fmt.Errorf("source/target volume format or collection mismatch")
			}
			current, err := lookup()
			if err != nil {
				return err
			}
			if !proto.Equal(original, current) {
				return fmt.Errorf("filer reference changed before repair")
			}
			if !*apply {
				_, err = fmt.Fprintf(w, "verified dry-run: %s source=%s target=%s size=%d sha256=%s; no writes\n", fid, *source, *target, expectedSize, *hash)
				return err
			}
			_, err = tc.WriteNeedleBlobIfAbsent(ctx, &volume_server_pb.WriteNeedleBlobRequest{VolumeId: uint32(fid.VolumeId), NeedleId: uint64(fid.Key), Size: int32(size), NeedleBlob: blob.NeedleBlob})
			if err != nil {
				return fmt.Errorf("repair RPC failed (no fallback; recheck target before retry): %w", err)
			}
			// Verify the exact raw record on the named target, not a proxied HTTP read.
			_, to, ts, ok, err := repairIndex(ctx, tc, fid)
			if err != nil {
				return fmt.Errorf("repair acknowledged, verification uncertain: %w", err)
			}
			if !ok || ts != size {
				return fmt.Errorf("repair acknowledged, target mapping verification failed")
			}
			readback, err := tc.ReadNeedleBlob(ctx, &volume_server_pb.ReadNeedleBlobRequest{VolumeId: uint32(fid.VolumeId), Offset: to.ToActualOffset(), Size: int32(ts)})
			if err != nil {
				return fmt.Errorf("repair acknowledged, readback uncertain: %w", err)
			}
			if !sameRepairRecord(blob.NeedleBlob, readback.NeedleBlob, size, version) {
				return fmt.Errorf("repair acknowledged, target record differs")
			}
			current, err = lookup()
			if err != nil {
				return fmt.Errorf("repair acknowledged, reference verification uncertain: %w", err)
			}
			if !proto.Equal(original, current) {
				return fmt.Errorf("repair acknowledged but filer reference changed; no destructive rollback attempted")
			}
			_, err = fmt.Fprintf(w, "repaired and verified: %s target=%s sha256=%s\n", fid, *target, *hash)
			return err
		})
	})
}
