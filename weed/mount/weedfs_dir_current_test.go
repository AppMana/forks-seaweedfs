package mount

import "testing"

// The kernel keeps the listing it holds only when no change has landed since
// the open that let it read that listing.
func TestKeepKernelListingOnlyWithoutNewerChange(t *testing.T) {
	var p directoryPositions
	if p.keepKernelListing(7, 100, 100) {
		t.Fatal("first open kept a listing the kernel never read through the mount")
	}
	if !p.keepKernelListing(7, 100, 100) {
		t.Fatal("unchanged directory dropped the kernel's listing")
	}
	if p.keepKernelListing(7, 150, 150) {
		t.Fatal("kept a listing older than the newest change")
	}
	if !p.keepKernelListing(7, 120, 150) {
		t.Fatal("dropped a listing already current past the newest change")
	}
	p.dropKernelListing(7)
	if p.keepKernelListing(7, 150, 150) {
		t.Fatal("kept a listing after it was dropped")
	}
}
