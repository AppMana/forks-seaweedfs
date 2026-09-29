package s3api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/seaweedfs/seaweedfs/weed/cluster"
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3_constants"
)

func shareableTestEntry() *filer_pb.Entry {
	return &filer_pb.Entry{
		Name:       "src",
		Attributes: &filer_pb.FuseAttributes{FileSize: 3 << 20, Md5: []byte("0123456789abcdef")},
		Chunks: []*filer_pb.FileChunk{
			{FileId: "3,0a01", Offset: 0, Size: 1 << 20},
			{FileId: "3,0a02", Offset: 1 << 20, Size: 2 << 20},
		},
		Extended: map[string][]byte{},
	}
}

func TestShareableCopySource(t *testing.T) {
	cases := []struct {
		name  string
		entry func(*filer_pb.Entry)
		req   func(*http.Request)
		want  bool
	}{
		{name: "plain chunked object", want: true},
		{name: "multipart object without an MD5", entry: func(e *filer_pb.Entry) { e.Attributes.Md5 = nil }, want: true},
		{name: "already a member", entry: func(e *filer_pb.Entry) {
			e.Extended[filer.SharedChunksExtKey] = filer.NewSharedChunksRef(filer.SharedChunksGroupOf(e.Chunks)).Bytes()
		}, want: true},
		{name: "same storage class", entry: func(e *filer_pb.Entry) { e.Extended[s3_constants.AmzStorageClass] = []byte("STANDARD_IA") },
			req: func(r *http.Request) { r.Header.Set(s3_constants.AmzStorageClass, "STANDARD_IA") }, want: true},
		{name: "directory", entry: func(e *filer_pb.Entry) { e.IsDirectory = true }},
		{name: "remote object", entry: func(e *filer_pb.Entry) { e.RemoteEntry = &filer_pb.RemoteEntry{RemoteSize: 3 << 20} }},
		{name: "inline content", entry: func(e *filer_pb.Entry) { e.Chunks = nil; e.Content = []byte("x") }},
		{name: "empty object", entry: func(e *filer_pb.Entry) { e.Attributes.FileSize = 0 }},
		{name: "expiring object", entry: func(e *filer_pb.Entry) { e.Attributes.TtlSec = 60 }},
		{name: "hard link", entry: func(e *filer_pb.Entry) { e.HardLinkId = []byte("hl") }},
		{name: "volume-encrypted chunk", entry: func(e *filer_pb.Entry) { e.Chunks[1].CipherKey = []byte("k") }},
		{name: "SSE-C chunk", entry: func(e *filer_pb.Entry) { e.Chunks[0].SseType = filer_pb.SSEType_SSE_C }},
		{name: "SSE-KMS chunk", entry: func(e *filer_pb.Entry) { e.Chunks[0].SseType = filer_pb.SSEType_SSE_KMS }},
		{name: "SSE-S3 object", entry: func(e *filer_pb.Entry) {
			e.Extended[s3_constants.AmzServerSideEncryption] = []byte("AES256")
			e.Extended[s3_constants.SeaweedFSSSES3Key] = []byte("key")
		}},
		{name: "SSE-C destination", req: func(r *http.Request) {
			r.Header.Set(s3_constants.AmzServerSideEncryptionCustomerAlgorithm, "AES256")
		}},
		{name: "SSE-KMS destination", req: func(r *http.Request) { r.Header.Set(s3_constants.AmzServerSideEncryption, "aws:kms") }},
		{name: "SSE-S3 destination", req: func(r *http.Request) { r.Header.Set(s3_constants.AmzServerSideEncryption, "AES256") }},
		{name: "SSE-C source key", req: func(r *http.Request) {
			r.Header.Set(s3_constants.AmzCopySourceServerSideEncryptionCustomerAlgorithm, "AES256")
		}},
		{name: "storage class change", req: func(r *http.Request) { r.Header.Set(s3_constants.AmzStorageClass, "STANDARD_IA") }},
		{name: "manifest chunks without an MD5", entry: func(e *filer_pb.Entry) {
			e.Attributes.Md5 = nil
			e.Chunks[0].IsChunkManifest = true
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := shareableTestEntry()
			if tc.entry != nil {
				tc.entry(entry)
			}
			r := httptest.NewRequest(http.MethodPut, "/bkt/dst", nil)
			if tc.req != nil {
				tc.req(r)
			}
			if got := shareableCopySource(entry, r); got != tc.want {
				t.Fatalf("shareableCopySource = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCanShareCopyChunks(t *testing.T) {
	ring := cluster.NewLockClient(grpc.WithTransportCredentials(insecure.NewCredentials()), "filer:8888")
	ring.SetRing([]pb.ServerAddress{"filer:8888"}, 1)
	s3a := &S3ApiServer{option: &S3ApiServerOption{ShareCopyChunks: true, BucketsPath: "/buckets"}, objectWriteLockClient: ring}
	r := httptest.NewRequest(http.MethodPut, "/bkt/dst", nil)
	if !s3a.canShareCopyChunks(shareableTestEntry(), r, "bkt", "src", "", "", "bkt", "dst", "") {
		t.Fatal("a plain copy within an unversioned bucket was not shared")
	}
	cases := []struct {
		name                                                    string
		option                                                  bool
		srcBucket, srcObject, srcVersionId, srcState, dstBucket string
		dstObject, dstState                                     string
	}{
		{name: "option off", option: false, srcBucket: "bkt", srcObject: "src", dstBucket: "bkt", dstObject: "dst"},
		{name: "cross bucket", option: true, srcBucket: "bkt", srcObject: "src", dstBucket: "other", dstObject: "dst"},
		{name: "self copy", option: true, srcBucket: "bkt", srcObject: "src", dstBucket: "bkt", dstObject: "src"},
		{name: "source version", option: true, srcBucket: "bkt", srcObject: "src", srcVersionId: "v1", dstBucket: "bkt", dstObject: "dst"},
		{name: "versioned bucket", option: true, srcBucket: "bkt", srcObject: "src", srcState: s3_constants.VersioningEnabled, dstBucket: "bkt", dstObject: "dst", dstState: s3_constants.VersioningEnabled},
		{name: "suspended bucket", option: true, srcBucket: "bkt", srcObject: "src", srcState: s3_constants.VersioningSuspended, dstBucket: "bkt", dstObject: "dst", dstState: s3_constants.VersioningSuspended},
		// no object-write ring: the marking would not be serialized with the
		// source's other writes
		{name: "no owner filer", option: true, srcBucket: "bkt", srcObject: "src", dstBucket: "bkt", dstObject: "dst"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s3a.option.ShareCopyChunks = tc.option
			s3a.objectWriteLockClient = ring
			if tc.name == "no owner filer" {
				s3a.objectWriteLockClient = nil
			}
			if s3a.canShareCopyChunks(shareableTestEntry(), r, tc.srcBucket, tc.srcObject, tc.srcVersionId, tc.srcState, tc.dstBucket, tc.dstObject, tc.dstState) {
				t.Fatal("shared a copy that must copy the bytes")
			}
		})
	}
}

func TestSharedCopySourceLinkGivesTheDestinationItsOwnMarker(t *testing.T) {
	entry := shareableTestEntry()
	ref := filer.NewSharedChunksRef(filer.SharedChunksGroupOf(entry.Chunks))
	src := &sharedCopySource{path: "/buckets/bkt/src", ref: ref, chunks: entry.Chunks}
	dst := &filer_pb.Entry{Extended: map[string][]byte{}}
	src.link(dst)

	dstRef, member := filer.ParseSharedChunksRef(dst.Extended)
	if !member || dstRef.Group != ref.Group || dstRef == ref {
		t.Fatalf("destination marker %v, source marker %v", dstRef, ref)
	}
	if string(dst.Extended[filer.SharedChunksLinkSourceExtKey]) != string(filer.SharedChunksLinkSource(src.path, ref)) {
		t.Fatalf("link hint %q", dst.Extended[filer.SharedChunksLinkSourceExtKey])
	}
	if !sameChunkFileIds(dst.Chunks, entry.Chunks) {
		t.Fatal("destination chunks differ from the source's")
	}
	dst.Chunks[0].Offset = 99
	if entry.Chunks[0].Offset == 99 {
		t.Fatal("destination chunks alias the source's")
	}
}

func TestIsSharedCopyRefused(t *testing.T) {
	if !isSharedCopyRefused(errorString("CreateEntry /buckets/bkt/dst: " + filer.ErrSharedChunksSourceChanged.Error() + ": /buckets/bkt/src")) {
		t.Fatal("a refused link from the filer was not recognized")
	}
	if isSharedCopyRefused(errorString("CreateEntry /buckets/bkt/dst: existing is a directory")) || isSharedCopyRefused(nil) {
		t.Fatal("another failure was taken for a refused link")
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
