package storage

import (
	"errors"
	"os"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/backend"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
)

type vacuumSyncFailBackend struct {
	backend.BackendStorageFile
	err error
}

func (b vacuumSyncFailBackend) Sync() error { return b.err }

type vacuumSyncFailIndex struct {
	NeedleMapper
	err error
}

func (m vacuumSyncFailIndex) Sync() error { return m.err }

func TestCompactAbortsOnSourceSyncFailure(t *testing.T) {
	for _, byIndex := range []bool{false, true} {
		for _, target := range []string{"dat", "idx"} {
			name := "data/" + target
			if byIndex {
				name = "index/" + target
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				v, err := NewVolume(dir, dir, "", 1, NeedleMapInMemory, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer v.Close()
				want := errors.New("injected source sync I/O failure")
				originalDat, originalIdx := v.DataBackend, v.nm
				defer func() { v.DataBackend, v.nm = originalDat, originalIdx }()
				if target == "dat" {
					v.DataBackend = vacuumSyncFailBackend{originalDat, want}
				} else {
					v.nm = vacuumSyncFailIndex{originalIdx, want}
				}
				if byIndex {
					err = v.CompactByIndex(nil)
				} else {
					err = v.CompactByVolumeData(nil)
				}
				if !errors.Is(err, want) {
					t.Fatalf("sync failure not propagated: %v", err)
				}
				if err := v.CommitCompact(); err == nil {
					t.Fatal("failed preflight must not be committable")
				}
				for _, ext := range []string{".cpd", ".cpx", ".cpc"} {
					if _, err := os.Stat(v.FileName(ext)); !os.IsNotExist(err) {
						t.Fatalf("failed preflight created %s: %v", ext, err)
					}
				}
			})
		}
	}
}
