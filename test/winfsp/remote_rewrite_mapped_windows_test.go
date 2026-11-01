package winfsp

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var peerMountPoint = flag.String("peer-mountpoint", "", "a second mount of the same filer as -mountpoint, e.g. T:\\")

// Another client rewrites a file in place, keeping its size and mtime, while
// a process here holds it open and mapped. Windows cannot purge the cache
// section a mapped view pins, so the descriptor and mapping held across the
// rewrite are reported, not required; an open made after the rewrite while
// the mapping is still held must read it, and so must a read after both are
// released.
func TestRemoteRewriteWhileMapped(t *testing.T) {
	if *peerMountPoint == "" {
		t.Skip("no -peer-mountpoint given; this test needs two live mounts of one filer")
	}
	dir := testRoot(t)
	rel, err := filepath.Rel(*mountPoint, dir)
	if err != nil {
		t.Fatal(err)
	}
	peerDir := filepath.Join(*peerMountPoint, rel)
	path, peerPath := filepath.Join(dir, "data"), filepath.Join(peerDir, "data")
	before, after := bytes.Repeat([]byte{0x35}, 8192), bytes.Repeat([]byte{0xca}, 8192)
	stamp := time.Unix(1700000000, 0)
	if err := os.WriteFile(path, before, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	// Coordination waits only: the peer must see the file before rewriting
	// it, and this mount must have applied the rewrite before the reopen.
	awaitPath(t, peerPath)

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, len(before))
	if _, err := f.ReadAt(got, 0); err != nil || !bytes.Equal(got, before) {
		t.Fatalf("initial read: err=%v equal=%t", err, bytes.Equal(got, before))
	}
	mapping, unmap := mapReadOnly(t, f, len(before))
	defer unmap()
	if !bytes.Equal(mapping, before) {
		t.Fatal("initial mapping differs")
	}

	w, err := os.OpenFile(peerPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	n, writeErr := w.WriteAt(after, 0)
	if err := errors.Join(writeErr, w.Sync(), w.Close()); err != nil || n != len(after) {
		t.Fatalf("peer rewrite: n=%d err=%v", n, err)
	}
	if err := os.Chtimes(peerPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	// The mount applies events in order, so a marker created after the
	// rewrite becoming visible here means the rewrite was applied.
	if err := os.WriteFile(filepath.Join(peerDir, "applied"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	awaitPath(t, filepath.Join(dir, "applied"))

	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	t.Logf("held across the rewrite: descriptor current=%t mapping current=%t", bytes.Equal(got, after), bytes.Equal(mapping, after))
	reopened, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reopened, after) {
		t.Errorf("open after the peer rewrite, while the mapping is held, read stale bytes (first byte %#x)", reopened[0])
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(after)) || !info.ModTime().Equal(stamp) {
		t.Fatalf("rewrite did not preserve size and mtime: size=%d mtime=%v", info.Size(), info.ModTime())
	}

	unmap()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	released, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(released, after) {
		t.Errorf("read after releasing the mapping and descriptor read stale bytes (first byte %#x)", released[0])
	}
}

func awaitPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func mapReadOnly(t *testing.T, f *os.File, size int) ([]byte, func()) {
	t.Helper()
	h, err := windows.CreateFileMapping(windows.Handle(f.Fd()), nil, windows.PAGE_READONLY, 0, uint32(size), nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := windows.MapViewOfFile(h, windows.FILE_MAP_READ, 0, 0, uintptr(size))
	if err != nil {
		windows.CloseHandle(h)
		t.Fatal(err)
	}
	unmapped := false
	return unsafe.Slice((*byte)(unsafe.Pointer(view)), size), func() {
		if !unmapped {
			unmapped = true
			windows.UnmapViewOfFile(view)
			windows.CloseHandle(h)
		}
	}
}
