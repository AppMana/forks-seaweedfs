package mount

import (
	"fmt"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/util"
)

// checkConsistency verifies the table's two directions agree: every mapped
// name belongs to the inode it maps to, every name an inode records maps back
// to it, and no directory state outlives its inode.
func (i *InodeToPath) checkConsistency() error {
	i.RLock()
	defer i.RUnlock()
	if root := i.inode2path[1]; root == nil || i.path2inode[root.path] != 1 {
		return fmt.Errorf("root inode missing or unmapped")
	}
	for p, inode := range i.path2inode {
		entry := i.inode2path[inode]
		if entry == nil {
			return fmt.Errorf("%s maps to forgotten inode %d", p, inode)
		}
		found := false
		for _, q := range entry.appendPaths(nil) {
			found = found || q == p
		}
		if !found {
			return fmt.Errorf("%s maps to inode %d, which does not record it (%v)", p, inode, entry.appendPaths(nil))
		}
	}
	for inode, entry := range i.inode2path {
		if entry.path == "" && entry.extraPaths != nil {
			return fmt.Errorf("inode %d has extra names but no primary", inode)
		}
		seen := map[util.FullPath]bool{}
		for _, p := range entry.appendPaths(nil) {
			if seen[p] {
				return fmt.Errorf("inode %d records %s twice", inode, p)
			}
			seen[p] = true
			if got, mapped := i.path2inode[p]; !mapped || got != inode {
				return fmt.Errorf("inode %d records %s, which maps to %d (mapped=%v)", inode, p, got, mapped)
			}
		}
	}
	for inode := range i.dirStates {
		if i.inode2path[inode] == nil {
			return fmt.Errorf("directory state outlives inode %d", inode)
		}
	}
	return nil
}

func TestInodeToPathConsistencyCheckCatchesDanglingNames(t *testing.T) {
	table := NewInodeToPath("/", 0)
	table.Lookup("/a", 1, false, false, 7, true)
	if err := table.checkConsistency(); err != nil {
		t.Fatal(err)
	}
	table.path2inode["/stale"] = 7
	if err := table.checkConsistency(); err == nil {
		t.Fatal("a name the inode does not record went unnoticed")
	}
	delete(table.path2inode, "/stale")
	table.path2inode["/gone"] = 99
	if err := table.checkConsistency(); err == nil {
		t.Fatal("a name mapped to a forgotten inode went unnoticed")
	}
}

