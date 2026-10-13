package shell

import (
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// A shell built without the servers' offset tag parses every .idx entry at
// the wrong width: keys become garbage, so fsck sees live needles as orphans
// and absent, and repairs read blobs at wrong offsets. Refuse to run.
func TestRequireServerIndexLayoutRejectsAnotherOffsetSize(t *testing.T) {
	other := "30GB"
	if util.SizeLimit == other {
		other = "8000GB"
	}
	versions := map[pb.ServerAddress]string{
		"same:8080":  util.SizeLimit + " 4.48 abc",
		"other:8080": other + " 4.48 abc",
	}
	status := func(server pb.ServerAddress) (string, error) { return versions[server], nil }

	if err := requireServerIndexLayout([]pb.ServerAddress{"same:8080"}, status); err != nil {
		t.Fatalf("matching layout: %v", err)
	}
	err := requireServerIndexLayout([]pb.ServerAddress{"same:8080", "other:8080"}, status)
	if err == nil || !strings.Contains(err.Error(), "other:8080") || !strings.Contains(err.Error(), other) {
		t.Fatalf("mismatched layout must name the server and its size class, got %v", err)
	}
}
