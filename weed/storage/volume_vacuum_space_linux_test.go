//go:build linux

package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/backend"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
)

func allocatedBytes(t *testing.T, root string) uint64 {
	t.Helper()
	var blocks uint64
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok {
				blocks += uint64(stat.Blocks) * 512
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return blocks
}

func TestVacuumStableLiveDatasetHasBoundedAllocatedBlocks(t *testing.T) {
	for _, kind := range []NeedleMapKind{NeedleMapInMemory, NeedleMapLevelDb} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			dir := t.TempDir()
			v, err := NewVolume(dir, dir, "", 1, kind, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.Version3, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			var baseline uint64
			for cycle := 0; cycle < 16; cycle++ {
				for id := uint64(1); id <= 32; id++ {
					n := newEmptyNeedle(id)
					n.Data = bytes.Repeat([]byte{byte(cycle + int(id))}, 4096)
					n.Checksum = needle.NewCRC(n.Data)
					if _, _, _, err := v.writeNeedle2(n, true, false, false); err != nil {
						t.Fatal(err)
					}
				}
				for id := uint64(17); id <= 32; id++ {
					if _, err := v.deleteNeedle2(newEmptyNeedle(id)); err != nil {
						t.Fatal(err)
					}
				}
				if err := v.CompactByIndex(nil); err != nil {
					t.Fatal(err)
				}
				if err := v.CommitCompact(); err != nil {
					t.Fatal(err)
				}
				allocated := allocatedBytes(t, dir)
				if cycle == 0 {
					baseline = allocated
				}
				// LevelDB may compact its own small files asynchronously. It must
				// not accumulate one generation of volume data per vacuum cycle.
				if allocated > baseline+16*1024*1024 {
					t.Fatalf("cycle %d: allocated bytes grew from %d to %d for fixed live data", cycle, baseline, allocated)
				}
				t.Logf("cycle=%d live=65536 allocated=%d", cycle, allocated)
			}
		})
	}
}

func TestVolumePreallocateENOSPCFailsCreation(t *testing.T) {
	if os.Getenv("SEAWEEDFS_TEST_DISPOSABLE_FS") != "1" {
		t.Skip("requires the disposable filesystem lab")
	}
	dir := t.TempDir()
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	total := uint64(stat.Blocks) * uint64(stat.Bsize)
	if total > 3*1024*1024*1024 {
		t.Fatalf("refusing ENOSPC test on filesystem larger than 3 GiB: %d", total)
	}

	path := filepath.Join(dir, "preallocated.dat")
	file, err := backend.CreateVolumeFile(path, int64(total+1024*1024*1024), 0)
	if file != nil {
		_ = file.Close()
	}
	if err == nil {
		t.Fatal("volume creation succeeded after the requested disk reservation failed")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("failed preallocation left a candidate file behind: %v", statErr)
	}

	existingPath := filepath.Join(dir, "existing.dat")
	existingData := []byte("intact existing volume data")
	if err := os.WriteFile(existingPath, existingData, 0600); err != nil {
		t.Fatal(err)
	}
	file, err = backend.CreateVolumeFile(existingPath, int64(total+1024*1024*1024), 0)
	if file != nil {
		_ = file.Close()
	}
	if err == nil {
		t.Fatal("existing volume reservation unexpectedly succeeded")
	}
	got, readErr := os.ReadFile(existingPath)
	if readErr != nil {
		t.Fatalf("failed reservation removed existing volume: %v", readErr)
	}
	if !bytes.Equal(got, existingData) {
		t.Fatalf("failed reservation changed existing volume: got %q", got)
	}
}

func TestCompactENOSPCPreservesOriginal(t *testing.T) {
	if os.Getenv("SEAWEEDFS_TEST_DISPOSABLE_FS") != "1" {
		t.Skip("requires the disposable filesystem lab")
	}
	dir := t.TempDir()
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	total := uint64(stat.Blocks) * uint64(stat.Bsize)
	if total > 3*1024*1024*1024 {
		t.Fatalf("refusing ENOSPC test on filesystem larger than 3 GiB: %d", total)
	}
	v, err := NewVolume(dir, dir, "", 1, NeedleMapInMemory, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.Version3, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	want := make(map[uint64][]byte)
	for id := uint64(1); id <= 32; id++ {
		data := bytes.Repeat([]byte{byte(id)}, 1024*1024)
		n := newEmptyNeedle(id)
		n.Data = data
		n.Checksum = needle.NewCRC(data)
		if _, _, _, err := v.writeNeedle2(n, true, true, false); err != nil {
			t.Fatal(err)
		}
		want[id] = data
	}
	datBefore, err := os.ReadFile(v.FileName(".dat"))
	if err != nil {
		t.Fatal(err)
	}
	idxBefore, err := os.ReadFile(v.FileName(".idx"))
	if err != nil {
		t.Fatal(err)
	}
	fillerPath := filepath.Join(dir, "enospc-filler")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { filler.Close(); os.Remove(fillerPath) }()
	if err := syscall.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	available := int64(stat.Bavail) * int64(stat.Bsize)
	if available <= 64*1024*1024 {
		t.Fatalf("unexpectedly little initial free space: %d", available)
	}
	// Btrfs' reported available space includes chunk/metadata constraints, so a
	// single near-filesystem-sized fallocate can fail before consuming anything.
	// Allocate progressively, halving on ENOSPC, until either Statfs is below
	// 4 MiB or even a 1 MiB new extent cannot be allocated.
	fillerSize := int64(0)
	saturated := false
	for attempt := 0; attempt < 512; attempt++ {
		if err := syscall.Statfs(dir, &stat); err != nil {
			t.Fatal(err)
		}
		remaining := int64(stat.Bavail) * int64(stat.Bsize)
		if remaining < 4*1024*1024 {
			t.Logf("ENOSPC fixture free bytes=%d", remaining)
			break
		}
		consume := remaining / 2
		if consume > 256*1024*1024 {
			consume = 256 * 1024 * 1024
		}
		for consume >= 1024*1024 {
			err := syscall.Fallocate(int(filler.Fd()), 0, fillerSize, consume)
			if err == nil {
				fillerSize += consume
				break
			}
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("extend filler: %v", err)
			}
			consume /= 2
		}
		if consume < 1024*1024 {
			saturated = true
			t.Log("ENOSPC fixture cannot allocate a 1 MiB extent")
			break
		}
	}
	if err := syscall.Statfs(dir, &stat); err != nil {
		t.Fatal(err)
	}
	remaining := uint64(stat.Bavail) * uint64(stat.Bsize)
	if remaining >= 4*1024*1024 && !saturated {
		t.Fatalf("could not establish ENOSPC fixture, %d bytes remain", remaining)
	}
	if err := v.CompactByIndex(nil); err == nil {
		t.Fatal("compaction unexpectedly succeeded with less free space than live data")
	} else if !errors.Is(err, syscall.ENOSPC) {
		t.Logf("compaction failed safely with wrapped/non-ENOSPC error: %v", err)
	}
	if err := v.CommitCompact(); err == nil {
		t.Fatal("failed ENOSPC copy must not commit")
	}
	for _, ext := range []string{".cpd", ".cpx", ".cpc"} {
		mustNotExist(t, v.FileName(ext))
	}
	if got, err := os.ReadFile(v.FileName(".dat")); err != nil || !bytes.Equal(got, datBefore) {
		t.Fatalf("ENOSPC changed original .dat: %v", err)
	}
	if got, err := os.ReadFile(v.FileName(".idx")); err != nil || !bytes.Equal(got, idxBefore) {
		t.Fatalf("ENOSPC changed original .idx: %v", err)
	}
	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	for id, data := range want {
		n := newEmptyNeedle(id)
		if _, err := v.readNeedle(n, nil, nil); err != nil || !bytes.Equal(n.Data, data) {
			t.Fatalf("read %d after releasing ENOSPC: %v", id, err)
		}
	}
}
