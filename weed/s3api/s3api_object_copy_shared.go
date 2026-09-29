package s3api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3_constants"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3err"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/protobuf/proto"
)

// A whole-object copy within an unversioned bucket can give the destination
// the source's chunks instead of copying the bytes (ShareCopyChunks). The filer
// counts the entries on a shared chunk list (weed/filer/filer_shared_chunks.go)
// and frees the chunks with the last of them. The gateway's part:
//
//  1. mark the source as a member of the group named by its chunk list, as a
//     PATCH routed to the source's owner filer and conditioned on the chunks it
//     read, so the marking cannot write back a chunk list that was replaced;
//  2. read the source back for the marker it actually carries (a concurrent
//     copy may have marked it first) and the chunks that marker covers;
//  3. write the destination with those chunks, a marker of its own in the same
//     group, and a link hint naming the source. The filer writes the
//     destination's reference, then checks that the source still carries that
//     marker and those chunks, and refuses the write otherwise.
//
// A refused link is copied the ordinary way. Encrypted objects, versioned
// buckets, cross-bucket copies and remote or inline objects are always copied:
// their chunks are re-encrypted, outlive the copy as versions, belong to another
// bucket's collection, or do not exist.

// errSharedCopyUnavailable: the source could not be linked; copy the bytes.
var errSharedCopyUnavailable = errors.New("shared copy unavailable")

// errSharedCopyRefused is the internal result of a destination write the filer
// refused as a link; the handler then copies the bytes. Never sent to a client.
const errSharedCopyRefused s3err.ErrorCode = -1

// sharedCopySource is a marked source whose chunk list a copy may share.
type sharedCopySource struct {
	path   util.FullPath
	ref    filer.SharedChunksRef
	chunks []*filer_pb.FileChunk
}

// shareableCopySource reports whether the copy of entry that r asks for may
// share entry's chunks, judged from the entry and the request alone.
func shareableCopySource(entry *filer_pb.Entry, r *http.Request) bool {
	if entry == nil || entry.IsDirectory || entry.RemoteEntry != nil || entry.Attributes == nil {
		return false
	}
	if entry.Attributes.FileSize == 0 || len(entry.GetChunks()) == 0 || len(entry.Content) > 0 || entry.Attributes.TtlSec != 0 {
		return false
	}
	if len(entry.HardLinkId) > 0 {
		return false
	}
	if IsSSECEncryptedWithEntry(entry) || IsSSEKMSEncryptedWithEntry(entry) || IsSSES3EncryptedInternal(entry.Extended) {
		return false
	}
	for _, chunk := range entry.GetChunks() {
		if len(chunk.CipherKey) > 0 || chunk.SseType != filer_pb.SSEType_NONE {
			return false
		}
	}
	if IsSSECRequest(r) || IsSSEKMSRequest(r) || IsSSES3RequestInternal(r) || r.Header.Get(s3_constants.AmzCopySourceServerSideEncryptionCustomerAlgorithm) != "" {
		return false
	}
	if sc := r.Header.Get(s3_constants.AmzStorageClass); sc != "" && sc != storedStorageClass(entry) {
		return false
	}
	// Without an MD5 the ETag is derived from the chunk list, and a byte copy
	// derives it from the resolved data chunks: keep the two the same.
	if len(entry.Attributes.Md5) == 0 && filer.HasChunkManifest(entry.GetChunks()) {
		return false
	}
	return true
}

func storedStorageClass(entry *filer_pb.Entry) string {
	if sc, ok := entry.Extended[s3_constants.AmzStorageClass]; ok && len(sc) > 0 {
		return string(sc)
	}
	return "STANDARD"
}

// canShareCopyChunks adds what shareableCopySource cannot see: the option,
// the buckets and their versioning, bucket default encryption, and an owner
// filer to serialize the source's marking with its other writes.
func (s3a *S3ApiServer) canShareCopyChunks(entry *filer_pb.Entry, r *http.Request, srcBucket, srcObject, srcVersionId, srcVersioningState, dstBucket, dstObject, dstVersioningState string) bool {
	if !s3a.option.ShareCopyChunks || srcBucket != dstBucket || srcObject == dstObject || srcVersionId != "" {
		return false
	}
	if srcVersioningState != "" || dstVersioningState != "" {
		return false
	}
	if !shareableCopySource(entry, r) {
		return false
	}
	srcPath := fmt.Sprintf("%s/%s", s3a.bucketDir(srcBucket), srcObject)
	dstPath := fmt.Sprintf("%s/%s", s3a.bucketDir(dstBucket), dstObject)
	state := DetectEncryptionStateWithEntry(entry, r, srcPath, dstPath)
	s3a.applyCopyBucketDefaultEncryption(state, dstBucket)
	if strategy, err := DetermineUnifiedCopyStrategy(state, entry.Extended, r); err != nil || strategy != CopyStrategyDirect || state.IsTargetEncrypted() {
		return false
	}
	return s3a.objectWriteOwner(srcBucket, srcObject) != ""
}

// markSharedCopySource makes the source a member of the group of its chunk
// list and returns the marker and chunks it carries afterwards.
func (s3a *S3ApiServer) markSharedCopySource(bucket, object string, entry *filer_pb.Entry) (*sharedCopySource, error) {
	srcPath := util.NewFullPath(s3a.bucketDir(bucket), object)
	dir, name := srcPath.DirAndName()
	chunks := entry.GetChunks()
	if _, member := filer.ParseSharedChunksRef(entry.Extended); !member {
		fids := make([]string, len(chunks))
		for i, chunk := range chunks {
			fids[i] = chunk.GetFileIdString()
		}
		ref := filer.NewSharedChunksRef(filer.SharedChunksGroupOf(chunks))
		resp, err := s3a.objectTxnOnFiler(s3a.objectWriteOwner(bucket, object), &filer_pb.ObjectTransactionRequest{
			LockKey:  string(srcPath),
			RouteKey: s3a.objectRouteKey(bucket, object),
			Condition: &filer_pb.WriteCondition{Clauses: []*filer_pb.WriteCondition_Clause{{
				Kind: filer_pb.WriteCondition_IF_CHUNKS_EQUAL,
				Fids: fids,
			}}},
			Mutations: []*filer_pb.ObjectMutation{{
				Type:        filer_pb.ObjectMutation_PATCH_EXTENDED,
				Directory:   dir,
				Name:        name,
				SetExtended: map[string][]byte{filer.SharedChunksExtKey: ref.Bytes()},
			}},
		})
		if err != nil {
			return nil, fmt.Errorf("%w: mark %s: %v", errSharedCopyUnavailable, srcPath, err)
		}
		if resp.Error != "" {
			return nil, fmt.Errorf("%w: mark %s: %s", errSharedCopyUnavailable, srcPath, resp.Error)
		}
	}
	current, err := s3a.getEntry(dir, name)
	if err != nil {
		return nil, fmt.Errorf("%w: read back %s: %v", errSharedCopyUnavailable, srcPath, err)
	}
	ref, member := filer.ParseSharedChunksRef(current.Extended)
	if !member || !sameChunkFileIds(current.GetChunks(), chunks) {
		return nil, fmt.Errorf("%w: %s changed while it was marked", errSharedCopyUnavailable, srcPath)
	}
	return &sharedCopySource{path: srcPath, ref: ref, chunks: current.GetChunks()}, nil
}

// link gives dst the source's chunks, a marker of its own in the source's
// group, and the hint the filer checks the source against.
func (src *sharedCopySource) link(dst *filer_pb.Entry) {
	dst.Chunks = make([]*filer_pb.FileChunk, len(src.chunks))
	for i, chunk := range src.chunks {
		dst.Chunks[i] = proto.Clone(chunk).(*filer_pb.FileChunk)
	}
	dst.Extended[filer.SharedChunksExtKey] = filer.NewSharedChunksRef(src.ref.Group).Bytes()
	dst.Extended[filer.SharedChunksLinkSourceExtKey] = filer.SharedChunksLinkSource(src.path, src.ref)
}

// isSharedCopyRefused reports a destination write the filer refused because
// the source no longer carries what the link was made from.
func isSharedCopyRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), filer.ErrSharedChunksSourceChanged.Error())
}

func sameChunkFileIds(a, b []*filer_pb.FileChunk) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, chunk := range a {
		counts[chunk.GetFileIdString()]++
	}
	for _, chunk := range b {
		fid := chunk.GetFileIdString()
		if counts[fid] == 0 {
			return false
		}
		counts[fid]--
	}
	return true
}
