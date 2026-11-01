package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Keep the descriptor and mapping open across a peer's same-size rewrite.
// Rendezvous is explicit; data mismatches are never retried into a pass.
func cacheCoherence(root, owner, peer, action string) error {
	path := filepath.Join(root, ".sync", action+"-data-"+owner)
	peerPath := filepath.Join(root, ".sync", action+"-data-"+peer)
	before, after := bytes.Repeat([]byte{0x35}, 8192), bytes.Repeat([]byte{0xca}, 8192)
	stamp := time.Unix(1700000000, 0)
	if err := write(path, before); err != nil {
		return err
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		return err
	}
	if err := barrier(root, owner, action+"-seeded"); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	closeFile := sync.OnceValue(f.Close)
	defer closeFile()
	readFD := func(want []byte) error {
		got := make([]byte, len(want))
		n, err := f.ReadAt(got, 0)
		if err != nil || n != len(want) || !bytes.Equal(got, want) {
			return fmt.Errorf("open descriptor: n=%d err=%v equal=%t", n, err, bytes.Equal(got, want))
		}
		return nil
	}
	if err := readFD(before); err != nil {
		return err
	}
	// Ordinary file coherence is mandatory. The strict probe
	// (cache-coherence) maps the file on Windows too, and holds Windows to
	// what it can guarantee while the mapping exists; see heldViews.
	var mapped []byte
	var immutableMapping []byte
	unmapData := func() {}
	if runtime.GOOS != "windows" || action == "cache-coherence" {
		var unmap func()
		mapped, unmap, err = mapReadOnly(f, len(before))
		if err != nil {
			return err
		}
		unmapData = sync.OnceFunc(unmap)
		defer unmapData()
		if !bytes.Equal(mapped, before) {
			return fmt.Errorf("initial mapping differs")
		}
	} else {
		// Deployment contract: Windows mapped files are immutable while in
		// use. Keep a separate immutable view open across unrelated peer IO.
		immutablePath := filepath.Join(root, ".sync", action+"-immutable-"+owner)
		if err := write(immutablePath, before); err != nil {
			return err
		}
		immutableFile, err := os.Open(immutablePath)
		if err != nil {
			return err
		}
		defer immutableFile.Close()
		var unmap func()
		immutableMapping, unmap, err = mapReadOnly(immutableFile, len(before))
		if err != nil {
			return err
		}
		defer unmap()
		if !bytes.Equal(immutableMapping, before) {
			return fmt.Errorf("initial immutable mapping differs")
		}
	}
	if err := barrier(root, owner, action+"-primed"); err != nil {
		return err
	}
	// No truncate or rename: neither a size transition nor a new inode can
	// accidentally invalidate the cache and conceal a same-mtime failure.
	w, err := os.OpenFile(peerPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	n, writeErr := w.WriteAt(after, 0)
	err = errors.Join(writeErr, w.Sync(), w.Close())
	if err != nil || n != len(after) {
		return fmt.Errorf("peer rewrite: n=%d err=%v", n, err)
	}
	if err := os.Chtimes(peerPath, stamp, stamp); err != nil {
		return err
	}
	if err := barrier(root, owner, action+"-rewritten"); err != nil {
		return err
	}
	// Report all three paths, not just whichever happened to fail first.
	fdErr := readFD(after)
	var mapErr error
	if mapped != nil && !bytes.Equal(mapped, after) {
		mapErr = fmt.Errorf("existing mmap has stale bytes")
	}
	if immutableMapping != nil && !bytes.Equal(immutableMapping, before) {
		mapErr = fmt.Errorf("immutable mapping changed during unrelated peer writes")
	}
	got, reopenErr := os.ReadFile(path)
	if reopenErr == nil && !bytes.Equal(got, after) {
		reopenErr = fmt.Errorf("reopened file has stale bytes")
	}
	info, statErr := f.Stat()
	if statErr == nil && (info.Size() != int64(len(after)) || !info.ModTime().Equal(stamp)) {
		statErr = fmt.Errorf("rewrite did not preserve size/mtime: %v", info)
	}
	if heldViews(runtime.GOOS, action) {
		return errors.Join(fdErr, mapErr, reopenErr, statErr)
	}
	// Windows shares one cache section among every open of a name and its
	// mappings, and cannot purge it while a view is mapped, so the
	// descriptor and mapping opened before the rewrite are not guaranteed
	// to see it. Report them, then require what is guaranteed: an open
	// after the rewrite reads it, and so does a read once both are gone.
	fmt.Printf("windows views held across the peer rewrite: descriptor=%s mapping=%s\n", viewState(fdErr), viewState(mapErr))
	unmapData()
	closeErr := closeFile()
	released, releasedErr := os.ReadFile(path)
	if releasedErr == nil && !bytes.Equal(released, after) {
		releasedErr = fmt.Errorf("file read after releasing the mapping has stale bytes")
	}
	return errors.Join(reopenErr, statErr, closeErr, releasedErr)
}

// heldViews reports whether a descriptor and mapping held across a peer's
// rewrite are required to see it.
func heldViews(goos, action string) bool {
	return goos != "windows" || action != "cache-coherence"
}

func viewState(err error) string {
	if err != nil {
		return "stale"
	}
	return "current"
}
