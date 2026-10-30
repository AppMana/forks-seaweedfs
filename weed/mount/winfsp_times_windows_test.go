//go:build windows

package mount

import (
	"context"
	"testing"
	"time"

	cgofuse "github.com/winfsp/cgofuse/fuse"
)

// WinFsp passes UTIME_OMIT, zero or its own 1601 epoch for a time it is not
// setting. Writing any of them stores a bogus time that every client reads.
func TestWinfspUtimensKeepsTimesWindowsDidNotSet(t *testing.T) {
	wfs := newPagingWFS(t, "/d", []string{"f"}, 0)
	wfs.atimeMap = make(map[uint64]time.Time)
	// The entry belongs to root; open it as root to write it.
	wfs.option.MountUid, wfs.option.MountGid = 0, 0
	a := newWinfspFS(wfs, false)
	status, fh := a.Open("/d/f", cgofuse.O_RDWR)
	if status != 0 {
		t.Fatalf("open: %d", status)
	}
	ino, ok := a.inodeFromFh(fh)
	if !ok {
		t.Fatal("open returned no handle")
	}
	mtime := func() (int64, int32) {
		t.Helper()
		_, _, entry, status := wfs.maybeReadEntry(context.Background(), ino)
		if status != 0 || entry == nil {
			t.Fatalf("read entry: %v", status)
		}
		return entry.Attributes.Mtime, entry.Attributes.MtimeNs
	}
	atimeSet := func() bool {
		wfs.atimeMu.Lock()
		defer wfs.atimeMu.Unlock()
		_, found := wfs.atimeMap[ino]
		return found
	}
	sec, nsec := mtime()

	for _, tc := range []struct {
		name string
		tmsp []cgofuse.Timespec
	}{
		{"zero", []cgofuse.Timespec{{}, {}}},
		{"UTIME_OMIT", []cgofuse.Timespec{{Nsec: cgofuse.UTIME_OMIT}, {Nsec: cgofuse.UTIME_OMIT}}},
		{"Windows epoch", []cgofuse.Timespec{{Sec: -11644473600}, {Sec: -11644473600}}},
	} {
		if code := a.Utimens("/d/f", tc.tmsp); code != 0 {
			t.Fatalf("%s: utimens = %d", tc.name, code)
		}
		if gotSec, gotNsec := mtime(); gotSec != sec || gotNsec != nsec {
			t.Fatalf("%s: mtime became %d.%09d, want it left at %d.%09d", tc.name, gotSec, gotNsec, sec, nsec)
		}
		if atimeSet() {
			t.Fatalf("%s: atime was written", tc.name)
		}
	}

	if code := a.Utimens("/d/f", []cgofuse.Timespec{{Nsec: cgofuse.UTIME_OMIT}, {Sec: 1700000000, Nsec: 5}}); code != 0 {
		t.Fatalf("utimens mtime only: %d", code)
	}
	if gotSec, gotNsec := mtime(); gotSec != 1700000000 || gotNsec != 5 {
		t.Fatalf("mtime = %d.%09d, want 1700000000.000000005", gotSec, gotNsec)
	}
	if atimeSet() {
		t.Fatal("an omitted atime was written")
	}

	if code := a.Utimens("/d/f", []cgofuse.Timespec{{Sec: -86400}, {Sec: -86400}}); code != 0 {
		t.Fatalf("utimens before 1970: %d", code)
	}
	if gotSec, _ := mtime(); gotSec != -86400 {
		t.Fatalf("mtime = %d, want a date before 1970 to be written", gotSec)
	}
	if !atimeSet() {
		t.Fatal("a date before 1970 was not written to atime")
	}
}

func TestWinfspUtimeOmitMatchesCgofuse(t *testing.T) {
	if winfspUtimeOmit != cgofuse.UTIME_OMIT {
		t.Fatalf("winfspUtimeOmit = %d, cgofuse UTIME_OMIT = %d", winfspUtimeOmit, cgofuse.UTIME_OMIT)
	}
}
