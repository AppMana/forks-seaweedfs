package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Keep the descriptor and mapping open across a peer's same-size rewrite.
// Rendezvous is explicit; data mismatches are never retried into a pass.
func cacheCoherence(root, owner, peer, action string) error {
	path := filepath.Join(root, ".sync", action+"-"+owner)
	peerPath := filepath.Join(root, ".sync", action+"-"+peer)
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
	defer f.Close()
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
	// Ordinary file coherence is mandatory. Keep the original strict probe
	// (cache-coherence) requiring Windows mmap too; do not turn its known
	// platform limitation into a silently passing test.
	var mapped []byte
	if runtime.GOOS != "windows" || action == "cache-coherence" {
		var unmap func()
		mapped, unmap, err = mapReadOnly(f, len(before))
		if err != nil {
			return err
		}
		defer unmap()
		if !bytes.Equal(mapped, before) {
			return fmt.Errorf("initial mapping differs")
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
	got, reopenErr := os.ReadFile(path)
	if reopenErr == nil && !bytes.Equal(got, after) {
		reopenErr = fmt.Errorf("reopened file has stale bytes")
	}
	info, statErr := f.Stat()
	if statErr == nil && (info.Size() != int64(len(after)) || !info.ModTime().Equal(stamp)) {
		statErr = fmt.Errorf("rewrite did not preserve size/mtime: %v", info)
	}
	return errors.Join(fdErr, mapErr, reopenErr, statErr)
}
