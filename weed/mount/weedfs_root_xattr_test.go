//go:build !freebsd && !windows

package mount

import (
	"testing"

	"github.com/seaweedfs/go-fuse/v2/fuse"
)

func TestRootSetXAttrKeepsBucketAttributes(t *testing.T) {
	wfs, testServer := newCreateTestWFSWithRoot(t, "/buckets/pvc-test")
	testServer.rootEntry = newBucketRootEntry("pvc-test")

	status := wfs.SetXAttr(make(chan struct{}), &fuse.SetXAttrIn{
		InHeader: fuse.InHeader{NodeId: 1},
	}, "user.build", []byte("jr8lm"))
	if status != fuse.OK {
		t.Fatalf("SetXAttr status = %v, want OK", status)
	}

	updates := testServer.rootUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected one root update, got %d", len(updates))
	}
	extended := updates[0].GetEntry().GetExtended()
	if got := string(extended[XATTR_PREFIX+"user.build"]); got != "jr8lm" {
		t.Fatalf("new xattr missing from root update, extended = %v", extended)
	}
	if got := string(extended["Seaweed-X-Amz-Allow-Empty-Folders"]); got != "true" {
		t.Fatalf("root xattr update dropped the bucket policy attribute, extended = %v", extended)
	}
	if got := string(extended[XATTR_PREFIX+"user.owner"]); got != "ci" {
		t.Fatalf("root xattr update dropped an existing xattr, extended = %v", extended)
	}

	// The stored root is re-read after a save, so a later xattr operation
	// sees its own write.
	if removeStatus := wfs.RemoveXAttr(make(chan struct{}), &fuse.InHeader{NodeId: 1}, "user.owner"); removeStatus != fuse.OK {
		t.Fatalf("RemoveXAttr status = %v, want OK", removeStatus)
	}
	updates = testServer.rootUpdates()
	if len(updates) != 2 {
		t.Fatalf("expected two root updates, got %d", len(updates))
	}
	extended = updates[1].GetEntry().GetExtended()
	if _, still := extended[XATTR_PREFIX+"user.owner"]; still {
		t.Fatalf("removed xattr still present in root update, extended = %v", extended)
	}
	if got := string(extended[XATTR_PREFIX+"user.build"]); got != "jr8lm" {
		t.Fatalf("earlier xattr lost across root updates, extended = %v", extended)
	}
}
