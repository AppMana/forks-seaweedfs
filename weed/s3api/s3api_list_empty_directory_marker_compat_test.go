package s3api

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/stretchr/testify/assert"
)

// These tests model the 2026-08-24 Harbor registry outage on appmana-cluster-03.
//
// Harbor drives docker/distribution with the s3-aws storage driver pointed at
// SeaweedFS. Its upload purger walks docker/registry/v2/repositories/ and, for
// every directory it descends into, issues ListObjectsV2 with a TRAILING-SLASH
// prefix and delimiter "/". For each returned key it does:
//
//	_, file := path.Split(fileInfo.Path())
//	if file[0] == '_' { ... }          // registry/storage/purgeuploads.go:73
//
// On real S3 that is safe: S3 has no directories, so a prefix whose objects
// have all been deleted simply returns KeyCount 0 and no key can ever have an
// empty basename.
//
// SeaweedFS answers the same probe for a directory that is real but empty by
// synthesising a directory marker - a zero-byte key ending in "/" (upstream
// seaweedfs #9615, "list empty directories as directory markers"). path.Split
// on that key yields file == "", and file[0] panics:
//
//	panic: runtime error: index out of range [0] with length 0
//	  storage.getOutstandingUploads.func1  purgeuploads.go:73
//	  s3-aws.(*driver).doWalk.func1        s3.go:1023
//
// The failure is self-perpetuating: purging an upload session deletes the files
// under _uploads/<uuid>/ and leaves the now-empty directory behind, which is
// exactly the shape that panics the next run.
//
// AWS parity is the contract these tests assert. The marker synthesis is a
// SeaweedFS-only concept (it exists so out-of-band mkdir via FUSE/filer is
// visible to hadoop-aws style getFileStatus probes) and must not be the
// default, because it hands S3 clients a key shape the S3 API cannot produce.

// registryUploadSessionTree is the on-filer shape left behind after an upload
// session's files are deleted: the session directory still exists, with no
// children and no MIME type.
func registryUploadSessionTree() *testFilerClient {
	const repoDir = "/buckets/harbor-registry/docker/registry/v2/repositories/appmana/vllm-ampere/_uploads"
	return &testFilerClient{
		entriesByDir: map[string][]*filer_pb.Entry{
			repoDir: {
				{
					Name:        "b3a1c0de-0000-4a00-9f00-000000000001",
					IsDirectory: true,
					Attributes:  &filer_pb.FuseAttributes{Mime: ""},
				},
			},
			repoDir + "/b3a1c0de-0000-4a00-9f00-000000000001": {},
		},
	}
}

// TestEmptiedDirectoryIsNotListedAsKeyByDefault asserts AWS parity: once the
// objects under a prefix are deleted, listing that prefix returns nothing.
//
// RED against the current tree: doListFilerEntries stamps FolderMimeType on the
// empty directory and emits it, so the caller renders a "<dir>/" key.
func TestEmptiedDirectoryIsNotListedAsKeyByDefault(t *testing.T) {
	s3a := &S3ApiServer{option: &S3ApiServerOption{BucketsPath: "/buckets"}}

	const repoDir = "/buckets/harbor-registry/docker/registry/v2/repositories/appmana/vllm-ampere/_uploads"
	cursor := &ListingCursor{maxKeys: 1000, prefixEndsOnDelimiter: true}

	var seen []*filer_pb.Entry
	_, err := s3a.doListFilerEntries(context.Background(), registryUploadSessionTree(),
		listDirectoryRequest{
			dir:       repoDir,
			prefix:    "b3a1c0de-0000-4a00-9f00-000000000001",
			delimiter: "/",
			bucket:    "harbor-registry",
		}, cursor,
		func(dir string, entry *filer_pb.Entry) { seen = append(seen, entry) })

	assert.NoError(t, err)
	assert.Empty(t, seen,
		"an emptied directory must list as nothing, the way real S3 answers a prefix whose objects were deleted; "+
			"synthesising a directory marker hands docker/distribution a key with an empty basename")
}

// TestListedKeysAlwaysHaveANonEmptyBasename is the direct model of the panic:
// whatever a listing returns, path.Split of the resulting S3 key must yield a
// non-empty file component, because that is the invariant purgeuploads.go
// relies on.
//
// RED against the current tree: the synthesised marker produces the key
// ".../_uploads/<uuid>/", whose basename is "".
func TestListedKeysAlwaysHaveANonEmptyBasename(t *testing.T) {
	s3a := &S3ApiServer{option: &S3ApiServerOption{BucketsPath: "/buckets"}}

	const repoDir = "/buckets/harbor-registry/docker/registry/v2/repositories/appmana/vllm-ampere/_uploads"
	const bucketPrefix = "/buckets/harbor-registry/"
	cursor := &ListingCursor{maxKeys: 1000, prefixEndsOnDelimiter: true}

	_, err := s3a.doListFilerEntries(context.Background(), registryUploadSessionTree(),
		listDirectoryRequest{
			dir:       repoDir,
			prefix:    "b3a1c0de-0000-4a00-9f00-000000000001",
			delimiter: "/",
			bucket:    "harbor-registry",
		}, cursor,
		func(dir string, entry *filer_pb.Entry) {
			key := strings.TrimPrefix(dir+"/"+entry.Name, bucketPrefix)
			if entry.IsDirectoryKeyObject() {
				// This is how the listing renders a directory key object.
				key += "/"
			}
			_, file := path.Split(key)
			assert.NotEmpty(t, file,
				"key %q has an empty basename; docker/distribution purgeuploads.go:73 "+
					"indexes file[0] on this and panics with index out of range [0] with length 0", key)
		})

	assert.NoError(t, err)
}

// TestDirectoryKeyObjectStillListed guards the legitimate case that must keep
// working: a directory created deliberately via PutObject with a trailing "/"
// is a real zero-byte object on real S3 too, so it must still be returned.
func TestDirectoryKeyObjectStillListed(t *testing.T) {
	s3a := &S3ApiServer{option: &S3ApiServerOption{BucketsPath: "/buckets"}}

	client := &testFilerClient{
		entriesByDir: map[string][]*filer_pb.Entry{
			"/buckets/test": {
				{
					Name:        "explicit",
					IsDirectory: true,
					// FolderMimeType is what PutObject("explicit/") records.
					Attributes: &filer_pb.FuseAttributes{Mime: "httpd/unix-directory"},
				},
			},
			"/buckets/test/explicit": {},
		},
	}

	cursor := &ListingCursor{maxKeys: 1000, prefixEndsOnDelimiter: true}
	var seen []*filer_pb.Entry
	_, err := s3a.doListFilerEntries(context.Background(), client,
		listDirectoryRequest{dir: "/buckets/test", prefix: "explicit", delimiter: "/", bucket: "test"},
		cursor,
		func(dir string, entry *filer_pb.Entry) { seen = append(seen, entry) })

	assert.NoError(t, err)
	assert.Len(t, seen, 1, "a directory created via PutObject with a trailing slash is a real object and must still list")
}
