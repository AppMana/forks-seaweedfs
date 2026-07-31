//go:build windows

package mount

import (
	"testing"

	cgofuse "github.com/winfsp/cgofuse/fuse"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// winfspFS is the fork's cgofuse adapter: it is the entire Windows mount, and
// at ~680 lines it is the largest piece of code upstream does not have. Its
// two pure translation helpers -- attrToStat and fullPath -- are where an
// upstream merge can do silent damage, because both straddle a type boundary
// the fork does not own (go-fuse's fuse.Attr on one side, cgofuse's Stat_t on
// the other). A field added, renamed, or re-typed upstream compiles fine here
// while quietly producing wrong file metadata on the mount.

func TestAttrToStatMapsEveryField(t *testing.T) {
	attr := &fuse.Attr{
		Ino:       42,
		Size:      123456,
		Blocks:    241,
		Atime:     1000,
		Mtime:     2000,
		Ctime:     3000,
		Atimensec: 111,
		Mtimensec: 222,
		Ctimensec: 333,
		Mode:      0o100644,
		Nlink:     2,
		Owner:     fuse.Owner{Uid: 1001, Gid: 1002},
		Rdev:      7,
		Blksize:   65536,
	}

	var st cgofuse.Stat_t
	attrToStat(attr, &st)

	for _, c := range []struct {
		name     string
		got, want int64
	}{
		{"Ino", int64(st.Ino), 42},
		{"Mode", int64(st.Mode), 0o100644},
		{"Nlink", int64(st.Nlink), 2},
		{"Uid", int64(st.Uid), 1001},
		{"Gid", int64(st.Gid), 1002},
		{"Rdev", int64(st.Rdev), 7},
		{"Size", st.Size, 123456},
		{"Blocks", st.Blocks, 241},
		{"Blksize", st.Blksize, 65536},
		{"Atim.Sec", st.Atim.Sec, 1000},
		{"Atim.Nsec", st.Atim.Nsec, 111},
		{"Mtim.Sec", st.Mtim.Sec, 2000},
		{"Mtim.Nsec", st.Mtim.Nsec, 222},
		{"Ctim.Sec", st.Ctim.Sec, 3000},
		{"Ctim.Nsec", st.Ctim.Nsec, 333},
	} {
		if c.got != c.want {
			t.Errorf("attrToStat %s = %d; want %d", c.name, c.got, c.want)
		}
	}
}

// WinFsp surfaces a creation time in every directory listing and Explorer
// property sheet. Linux FUSE attrs carry no birth time, so the fork
// deliberately substitutes ctime. If a merge ever leaves Birthtim zeroed,
// every file on the mount dates to 1601 in Explorer -- cosmetic-looking, but
// it also breaks tools that sort or prune by creation time.
func TestAttrToStatSynthesizesBirthTimeFromCtime(t *testing.T) {
	attr := &fuse.Attr{Ctime: 1700000000, Ctimensec: 500}

	var st cgofuse.Stat_t
	attrToStat(attr, &st)

	if st.Birthtim.Sec != 1700000000 || st.Birthtim.Nsec != 500 {
		t.Fatalf("Birthtim = {%d, %d}; want it synthesized from Ctim {%d, %d}",
			st.Birthtim.Sec, st.Birthtim.Nsec, attr.Ctime, attr.Ctimensec)
	}
	if st.Birthtim != st.Ctim {
		t.Fatalf("Birthtim %v != Ctim %v", st.Birthtim, st.Ctim)
	}
}

// A zero Blksize reaching WinFsp makes it compute a zero-block file; the fork
// substitutes the mount's block size. Entries served straight from the meta
// cache routinely have Blksize unset, so this is the common path, not an edge
// case.
func TestAttrToStatDefaultsZeroBlksize(t *testing.T) {
	var st cgofuse.Stat_t
	attrToStat(&fuse.Attr{Blksize: 0}, &st)
	if st.Blksize != blockSize {
		t.Fatalf("Blksize = %d for a zero-Blksize attr; want the %d default", st.Blksize, blockSize)
	}

	st = cgofuse.Stat_t{}
	attrToStat(&fuse.Attr{Blksize: 4096}, &st)
	if st.Blksize != 4096 {
		t.Fatalf("Blksize = %d; want the attr's own 4096 to be preserved", st.Blksize)
	}
}

// attrToStat must overwrite, not merge: cgofuse reuses Stat_t buffers across
// Readdir fill callbacks, so a field left untouched leaks the previous entry's
// value into the next one.
func TestAttrToStatOverwritesReusedBuffer(t *testing.T) {
	st := cgofuse.Stat_t{
		Ino:   999,
		Size:  888,
		Uid:   777,
		Gid:   666,
		Nlink: 55,
		Mode:  0o777,
	}
	attrToStat(&fuse.Attr{Ino: 1, Size: 2, Owner: fuse.Owner{Uid: 3, Gid: 4}, Nlink: 5, Mode: 0o644}, &st)

	if st.Ino != 1 || st.Size != 2 || st.Uid != 3 || st.Gid != 4 || st.Nlink != 5 || st.Mode != 0o644 {
		t.Fatalf("stale values survived into a reused Stat_t: %+v", st)
	}
}

// fullPath maps a cgofuse path (always '/'-rooted, relative to the mount
// point) onto the filer namespace. The subdirectory-mount case is the one that
// matters operationally: every seaweedfs-csi-driver volume mounts a filer
// subtree, so a regression here sends every operation to the wrong filer path.
func TestWinfspFSFullPath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mountRoot string
		path      string
		want      util.FullPath
	}{
		{"root mount, root path", "/", "/", "/"},
		{"root mount, empty path", "/", "", "/"},
		{"root mount, file", "/", "/a.txt", "/a.txt"},
		{"root mount, nested", "/", "/dir/sub/a.txt", "/dir/sub/a.txt"},

		{"subdir mount, root path", "/buckets/vol1", "/", "/buckets/vol1"},
		{"subdir mount, empty path", "/buckets/vol1", "", "/buckets/vol1"},
		{"subdir mount, file", "/buckets/vol1", "/a.txt", "/buckets/vol1/a.txt"},
		{"subdir mount, nested", "/buckets/vol1", "/dir/sub/a.txt", "/buckets/vol1/dir/sub/a.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &winfspFS{wfs: &WFS{option: &Option{FilerMountRootPath: tc.mountRoot}}}
			if got := a.fullPath(tc.path); got != tc.want {
				t.Fatalf("fullPath(%q) with root %q = %q; want %q", tc.path, tc.mountRoot, got, tc.want)
			}
		})
	}
}

// The root-mount case needs its own guard: naive concatenation would yield
// "//a.txt", which the filer treats as a different path than "/a.txt".
func TestWinfspFSFullPathDoesNotDoubleSlashAtRoot(t *testing.T) {
	a := &winfspFS{wfs: &WFS{option: &Option{FilerMountRootPath: "/"}}}
	for _, p := range []string{"/a.txt", "/dir", "/dir/sub"} {
		if got := a.fullPath(p); string(got) != p {
			t.Fatalf("fullPath(%q) at root mount = %q; want %q", p, got, p)
		}
	}
}

// invalidFh is cgofuse's "no file handle" sentinel. inodeFromFh must reject it
// before it reaches WFS.GetHandle, which would otherwise look up handle
// 0xFFFFFFFFFFFFFFFF.
func TestInodeFromFhRejectsSentinel(t *testing.T) {
	a := &winfspFS{wfs: &WFS{option: &Option{}}}
	if _, ok := a.inodeFromFh(invalidFh); ok {
		t.Fatal("inodeFromFh(invalidFh) reported ok; the sentinel must never be treated as a real handle")
	}
	if invalidFh != ^uint64(0) {
		t.Fatalf("invalidFh = %#x; cgofuse's sentinel is %#x", invalidFh, ^uint64(0))
	}
}
