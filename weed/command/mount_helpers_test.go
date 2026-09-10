package command

import (
	"context"
	"os"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3_constants"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// fakeFilerGrpc answers the three filer RPCs mount start needs, keyed by
// directory + "/" + name.
type fakeFilerGrpc struct {
	filer_pb.SeaweedFilerClient
	entries map[string]*filer_pb.Entry
	creates int
	updates int
}

func (f *fakeFilerGrpc) LookupDirectoryEntry(_ context.Context, req *filer_pb.LookupDirectoryEntryRequest, _ ...grpc.CallOption) (*filer_pb.LookupDirectoryEntryResponse, error) {
	entry, found := f.entries[req.GetDirectory()+"/"+req.GetName()]
	if !found {
		return nil, filer_pb.ErrNotFound
	}
	return &filer_pb.LookupDirectoryEntryResponse{Entry: proto.Clone(entry).(*filer_pb.Entry)}, nil
}

func (f *fakeFilerGrpc) CreateEntry(_ context.Context, req *filer_pb.CreateEntryRequest, _ ...grpc.CallOption) (*filer_pb.CreateEntryResponse, error) {
	f.creates++
	f.entries[req.GetDirectory()+"/"+req.GetEntry().GetName()] = proto.Clone(req.GetEntry()).(*filer_pb.Entry)
	return &filer_pb.CreateEntryResponse{}, nil
}

func (f *fakeFilerGrpc) UpdateEntry(_ context.Context, req *filer_pb.UpdateEntryRequest, _ ...grpc.CallOption) (*filer_pb.UpdateEntryResponse, error) {
	f.updates++
	f.entries[req.GetDirectory()+"/"+req.GetEntry().GetName()] = proto.Clone(req.GetEntry()).(*filer_pb.Entry)
	return &filer_pb.UpdateEntryResponse{}, nil
}

type fakeFilerClient struct {
	grpc *fakeFilerGrpc
}

func (f *fakeFilerClient) WithFilerClient(_ bool, fn func(filer_pb.SeaweedFilerClient) error) error {
	return fn(f.grpc)
}

func (f *fakeFilerClient) AdjustedUrl(location *filer_pb.Location) string { return location.Url }

func (f *fakeFilerClient) GetDataCenter() string { return "" }

func newFakeFilerClient(entries map[string]*filer_pb.Entry) *fakeFilerClient {
	if entries == nil {
		entries = make(map[string]*filer_pb.Entry)
	}
	return &fakeFilerClient{grpc: &fakeFilerGrpc{entries: entries}}
}

// An existing bucket is left untouched: CreateEntry without O_EXCL rewrote it
// on every mount start, resetting its mode, owner and extended attributes.
func TestEnsureMountRoot_leavesExistingRootUntouched(t *testing.T) {
	existing := &filer_pb.Entry{
		Name:        "pvc-test",
		IsDirectory: true,
		Attributes: &filer_pb.FuseAttributes{
			FileMode: uint32(0o750) | uint32(os.ModeDir),
			Uid:      1000,
			Gid:      1000,
			Crtime:   1700000000,
		},
		Extended: map[string][]byte{s3_constants.ExtAllowEmptyFolders: []byte("true"), "xattr-user.k": []byte("v")},
	}
	client := newFakeFilerClient(map[string]*filer_pb.Entry{"/buckets/pvc-test": proto.Clone(existing).(*filer_pb.Entry)})

	if err := ensureMountRoot(context.Background(), client, "/buckets/pvc-test", "/buckets"); err != nil {
		t.Fatalf("ensureMountRoot: %v", err)
	}
	if client.grpc.creates != 0 || client.grpc.updates != 0 {
		t.Fatalf("existing root must not be rewritten, got creates=%d updates=%d", client.grpc.creates, client.grpc.updates)
	}
	if !proto.Equal(client.grpc.entries["/buckets/pvc-test"], existing) {
		t.Fatalf("existing root changed: %v", client.grpc.entries["/buckets/pvc-test"])
	}
}

func TestEnsureMountRoot_createsMissingBucketWithKeepFoldersPolicy(t *testing.T) {
	client := newFakeFilerClient(nil)

	if err := ensureMountRoot(context.Background(), client, "/buckets/pvc-new", "/buckets"); err != nil {
		t.Fatalf("ensureMountRoot: %v", err)
	}
	created := client.grpc.entries["/buckets/pvc-new"]
	if created == nil || !created.IsDirectory {
		t.Fatalf("expected the bucket directory to be created, got %v", created)
	}
	if got := string(created.Extended[s3_constants.ExtAllowEmptyFolders]); got != "true" {
		t.Fatalf("a bucket created for a mount must keep empty folders, got %q", got)
	}
}

func TestEnsureMountRoot_createsMissingNonBucketRootWithoutPolicy(t *testing.T) {
	client := newFakeFilerClient(map[string]*filer_pb.Entry{})

	if err := ensureMountRoot(context.Background(), client, "/data/scratch", "/buckets"); err != nil {
		t.Fatalf("ensureMountRoot: %v", err)
	}
	created := client.grpc.entries["/data/scratch"]
	if created == nil || !created.IsDirectory {
		t.Fatalf("expected the directory to be created, got %v", created)
	}
	if _, stamped := created.Extended[s3_constants.ExtAllowEmptyFolders]; stamped {
		t.Fatalf("a non-bucket root must not carry the bucket policy, got %v", created.Extended)
	}
}
