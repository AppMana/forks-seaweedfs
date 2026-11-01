package mount

import (
	"strings"
	"sync"

	"github.com/seaweedfs/seaweedfs/weed/util"
)

// winfspOpenNames records the names Windows holds file handles under, and
// which of them another client changed while they were held.
//
// WinFsp keys the Windows file object state, and with it the cache section a
// mapped view shares, by name, and keeps it for as long as any handle on the
// name is open. A change made elsewhere reaches Windows as a notification
// whose cache purge cannot succeed while a process maps the file, so every
// further cached open of the name reads the pages the mapping pinned. An open
// of such a name has to bypass the Windows cache until the last handle on it
// is gone; every other open keeps it.
type winfspOpenNames struct {
	caseSensitive bool

	mu sync.Mutex
	// opens counts the opens each handle carries, by name: WFS reuses a
	// handle for every open of the same inode, under any of its names.
	opens map[uint64]map[util.FullPath]int
	// inodes is the inode each handle is open on.
	inodes map[uint64]uint64
	// held counts the opens per name.
	held map[util.FullPath]int
	// stale holds the names changed elsewhere while held.
	stale map[util.FullPath]struct{}
}

func newWinfspOpenNames(caseSensitive bool) *winfspOpenNames {
	return &winfspOpenNames{
		caseSensitive: caseSensitive,
		opens:         map[uint64]map[util.FullPath]int{},
		inodes:        map[uint64]uint64{},
		held:          map[util.FullPath]int{},
		stale:         map[util.FullPath]struct{}{},
	}
}

// opened records an open of inode under name, carried by handle fh, and
// reports whether it has to bypass the Windows cache.
func (n *winfspOpenNames) opened(fh, inode uint64, name util.FullPath) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, uncached := n.stale[name]
	names := n.opens[fh]
	if names == nil {
		names = map[util.FullPath]int{}
		n.opens[fh] = names
	}
	names[name]++
	n.inodes[fh] = inode
	n.held[name]++
	return uncached
}

// released drops one open carried by fh. WinFsp reports the name the handle
// was opened under, spelled as the opener spelled it, which a rename may have
// changed since; any other name the handle carries stands in for it then.
func (n *winfspOpenNames) released(fh uint64, name util.FullPath) {
	n.mu.Lock()
	defer n.mu.Unlock()
	names := n.opens[fh]
	if len(names) == 0 {
		return
	}
	if names[name] == 0 {
		spelled := name
		name = ""
		for candidate := range names {
			if !n.caseSensitive && strings.EqualFold(string(candidate), string(spelled)) {
				name = candidate
				break
			}
			if name == "" || candidate < name {
				name = candidate
			}
		}
	}
	if names[name]--; names[name] == 0 {
		delete(names, name)
	}
	if len(names) == 0 {
		delete(n.opens, fh)
		delete(n.inodes, fh)
	}
	n.drop(name, 1)
}

// drop releases count opens of name; the last one takes its staleness along.
func (n *winfspOpenNames) drop(name util.FullPath, count int) {
	if n.held[name] -= count; n.held[name] <= 0 {
		delete(n.held, name)
		delete(n.stale, name)
	}
}

// changed records a change another client made to names, and to inode under
// whatever names it is held.
func (n *winfspOpenNames) changed(inode uint64, names ...util.FullPath) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, name := range names {
		if n.held[name] > 0 {
			n.stale[name] = struct{}{}
		}
	}
	if inode == 0 {
		return
	}
	for fh, held := range n.inodes {
		if held != inode {
			continue
		}
		for name := range n.opens[fh] {
			n.stale[name] = struct{}{}
		}
	}
}

// renamed moves the opens held under from, and under the names beneath it, to
// to, the way WinFsp renames the open file objects.
func (n *winfspOpenNames) renamed(from, to util.FullPath) {
	if from == to || from == "/" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	move := func(name util.FullPath) util.FullPath {
		if name == from {
			return to
		}
		if rest, found := strings.CutPrefix(string(name), string(from)+"/"); found {
			return util.FullPath(string(to) + "/" + rest)
		}
		return name
	}
	held, stale := map[util.FullPath]int{}, map[util.FullPath]struct{}{}
	for name, count := range n.held {
		target := move(name)
		held[target] += count
	}
	for name := range n.stale {
		target := move(name)
		stale[target] = struct{}{}
	}
	n.held, n.stale = held, stale
	for fh, names := range n.opens {
		moved := map[util.FullPath]int{}
		for name, count := range names {
			target := move(name)
			moved[target] += count
		}
		n.opens[fh] = moved
	}
}
