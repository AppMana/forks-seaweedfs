package s3api

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/stretchr/testify/assert"
)

// Empty filer directories must not synthesize S3 objects by default.
// Clients walking trailing-slash prefixes can interpret those markers
// as objects with empty basenames.

// registryUploadSessionTree is the on-filer shape left behind after an upload
// session's files are deleted: the session directory still exists, with no
// children and no MIME type.
func registryUploadSessionTree() *testFilerClient {
	const repoDir = "/buckets/registry/docker/registry/v2/repositories/example/image/_uploads"
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

func TestEmptiedDirectoryIsNotListedAsKeyByDefault(t *testing.T) {
	s3a := &S3ApiServer{option: &S3ApiServerOption{BucketsPath: "/buckets"}}

	const repoDir = "/buckets/registry/docker/registry/v2/repositories/example/image/_uploads"
	cursor := &ListingCursor{maxKeys: 1000, prefixEndsOnDelimiter: true}

	var seen []*filer_pb.Entry
	_, err := s3a.doListFilerEntries(context.Background(), registryUploadSessionTree(),
		listDirectoryRequest{
			dir:       repoDir,
			prefix:    "b3a1c0de-0000-4a00-9f00-000000000001",
			delimiter: "/",
			bucket:    "registry",
		}, cursor,
		func(dir string, entry *filer_pb.Entry) { seen = append(seen, entry) })

	assert.NoError(t, err)
	assert.Empty(t, seen,
		"an emptied directory must list as nothing, the way real S3 answers a prefix whose objects were deleted; "+
			"synthesising a directory marker hands docker/distribution a key with an empty basename")
}

func TestListedKeysAlwaysHaveANonEmptyBasename(t *testing.T) {
	s3a := &S3ApiServer{option: &S3ApiServerOption{BucketsPath: "/buckets"}}

	const repoDir = "/buckets/registry/docker/registry/v2/repositories/example/image/_uploads"
	const bucketPrefix = "/buckets/registry/"
	cursor := &ListingCursor{maxKeys: 1000, prefixEndsOnDelimiter: true}

	_, err := s3a.doListFilerEntries(context.Background(), registryUploadSessionTree(),
		listDirectoryRequest{
			dir:       repoDir,
			prefix:    "b3a1c0de-0000-4a00-9f00-000000000001",
			delimiter: "/",
			bucket:    "registry",
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
