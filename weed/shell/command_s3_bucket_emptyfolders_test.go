package shell

import (
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/filer/empty_folder_cleanup"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/s3api/s3_constants"
)

func TestBucketEmptyFolderPolicy(t *testing.T) {
	tests := []struct {
		name      string
		extended  map[string][]byte
		policy    emptyFolderPolicy
		attribute string
	}{
		{name: "unset keeps", extended: nil, policy: emptyFolderPolicyKeep, attribute: "(unset)"},
		{name: "true keeps", extended: map[string][]byte{s3_constants.ExtAllowEmptyFolders: []byte("true")}, policy: emptyFolderPolicyKeep, attribute: "true"},
		{name: "false removes", extended: map[string][]byte{s3_constants.ExtAllowEmptyFolders: []byte(" False ")}, policy: emptyFolderPolicyRemove, attribute: "False"},
		{name: "garbage keeps", extended: map[string][]byte{s3_constants.ExtAllowEmptyFolders: []byte("no")}, policy: emptyFolderPolicyKeep, attribute: "no"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, attribute := bucketEmptyFolderPolicy(&filer_pb.Entry{Name: "b", IsDirectory: true, Extended: tt.extended})
			if policy != tt.policy || attribute != tt.attribute {
				t.Fatalf("got %s/%q, want %s/%q", policy, attribute, tt.policy, tt.attribute)
			}
		})
	}
}

// The command writes exactly what the cleaner reads, and leaves every other
// attribute on the bucket alone.
func TestBucketEmptyFolderPolicy_roundTripKeepsOtherAttributes(t *testing.T) {
	entry := &filer_pb.Entry{Name: "b", IsDirectory: true, Extended: map[string][]byte{s3_constants.AmzIdentityId: []byte("owner")}}

	empty_folder_cleanup.SetBucketAllowEmptyFolders(entry, false)
	if policy, _ := bucketEmptyFolderPolicy(entry); policy != emptyFolderPolicyRemove {
		t.Fatalf("expected remove after -remove, got %s", policy)
	}
	empty_folder_cleanup.SetBucketAllowEmptyFolders(entry, true)
	if policy, _ := bucketEmptyFolderPolicy(entry); policy != emptyFolderPolicyKeep {
		t.Fatalf("expected keep after -keep, got %s", policy)
	}
	if string(entry.Extended[s3_constants.AmzIdentityId]) != "owner" {
		t.Fatalf("owner attribute lost: %v", entry.Extended)
	}
}
