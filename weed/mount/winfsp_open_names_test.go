package mount

import (
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/util"
)

// A name changed elsewhere while Windows holds it open keeps its stale cache
// section until the last handle goes, so every open of it until then has to
// bypass the cache; names nobody changed keep caching.
func TestWinfspOpenNamesBypassCacheOnlyWhileAChangedNameIsHeld(t *testing.T) {
	n := newWinfspOpenNames()
	const data, other = util.FullPath("/d/data"), util.FullPath("/d/other")
	if n.opened(10, 5, data) {
		t.Fatal("first open of an unchanged name bypasses the cache")
	}
	if n.opened(11, 6, other) {
		t.Fatal("open of an unrelated name bypasses the cache")
	}
	n.changed(5, data)
	if !n.opened(10, 5, data) {
		t.Fatal("reopen of a name changed while held keeps the cache")
	}
	if n.opened(12, 6, other) {
		t.Fatal("a change to one name made another bypass the cache")
	}
	n.released(10, data)
	if !n.opened(13, 5, data) {
		t.Fatal("releasing one of several opens forgot the change")
	}
	n.released(13, data)
	n.released(10, data)
	if n.opened(14, 5, data) {
		t.Fatal("the change outlived every handle on the name")
	}
}

// A change to a name nobody holds leaves no section behind to go stale.
func TestWinfspOpenNamesIgnoreChangesToNamesNotHeld(t *testing.T) {
	n := newWinfspOpenNames()
	n.changed(5, "/d/data")
	if n.opened(10, 5, "/d/data") {
		t.Fatal("a change made before the open bypasses the cache")
	}
}

// Windows holds a hard link's other name as its own file object; a change
// reported under one name outdates every held name of the inode.
func TestWinfspOpenNamesChangeReachesEveryHeldNameOfTheInode(t *testing.T) {
	n := newWinfspOpenNames()
	n.opened(10, 5, "/d/link")
	n.changed(5, "/d/data")
	if !n.opened(10, 5, "/d/link") {
		t.Fatal("a change under another name of the inode kept the cache")
	}
}

// A replaced or vacated name is stale for the file object Windows still
// holds under it, whichever inode the name now resolves to.
func TestWinfspOpenNamesReplacementOfAHeldName(t *testing.T) {
	n := newWinfspOpenNames()
	n.opened(10, 5, "/d/data")
	n.changed(9, "/d/data")
	if !n.opened(11, 9, "/d/data") {
		t.Fatal("open of a replaced held name keeps the cache")
	}
}

// WinFsp renames the open file objects along with the name, and reports the
// name a handle was opened under when it is released.
func TestWinfspOpenNamesFollowLocalRenames(t *testing.T) {
	n := newWinfspOpenNames()
	n.opened(10, 5, "/d/data")
	n.opened(11, 6, "/d/sub/file")
	n.renamed("/d/data", "/d/moved")
	n.renamed("/d/sub", "/e/sub")
	n.changed(5, "/d/moved")
	n.changed(6, "/e/sub/file")
	if !n.opened(12, 5, "/d/moved") {
		t.Fatal("renamed held file lost its change")
	}
	if !n.opened(13, 6, "/e/sub/file") {
		t.Fatal("file under a renamed directory lost its change")
	}
	if n.opened(14, 7, "/d/data") {
		t.Fatal("the vacated name stayed held")
	}
	n.released(14, "/d/data")
	n.released(12, "/d/moved")
	n.released(10, "/d/data")
	if n.opened(15, 5, "/d/moved") {
		t.Fatal("release under the opened name left the renamed name held")
	}
}
