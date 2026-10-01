package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

type absentBlobWriter interface {
	WriteNeedleBlobIfAbsent(types.NeedleId, []byte, types.Size) error
}

func TestMissingNeedleRepairPreservesExistingRecords(t *testing.T) {
	newVolume := func() *Volume {
		t.Helper()
		dir := t.TempDir()
		v, err := NewVolume(dir, dir, "", 7, NeedleMapInMemory, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(v.Close)
		return v
	}
	source := newVolume()
	n := newRandomNeedle(17)
	offset, _, _, err := source.writeNeedle2(n, true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := source.ReadNeedleBlob(int64(offset), n.Size)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"absent", "live", "deleted", "wrong-id", "corrupt", "truncated", "closed", "read-only"} {
		t.Run(state, func(t *testing.T) {
			v := newVolume()
			writer, ok := any(v).(absentBlobWriter)
			if !ok {
				t.Fatal("missing atomic absent-only repair capability")
			}
			if state == "live" || state == "deleted" {
				existing := newRandomNeedle(17)
				if _, _, _, err := v.writeNeedle2(existing, true, true, false); err != nil {
					t.Fatal(err)
				}
				if state == "deleted" {
					if _, err := v.deleteNeedle2(existing); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := os.ReadFile(v.DataFileName() + ".dat")
			if err != nil {
				t.Fatal(err)
			}
			indexBefore, err := os.ReadFile(v.IndexFileName() + ".idx")
			if err != nil {
				t.Fatal(err)
			}
			input, id := bytes.Clone(blob), n.Id
			switch state {
			case "wrong-id":
				id++
			case "corrupt":
				input[types.NeedleHeaderSize+4] ^= 1
			case "truncated":
				input = input[:len(input)-1]
			case "closed":
				v.Close()
			case "read-only":
				if err := v.PersistReadOnly(true, false); err != nil {
					t.Fatal(err)
				}
				// PersistReadOnly writes the mode for the next open; it does
				// not change the current Store's runtime write flags.
				dir := filepath.Dir(v.DataFileName())
				v.Close()
				v, err = NewVolume(dir, dir, "", 7, NeedleMapInMemory, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(v.Close)
				writer = v
				if !v.IsReadOnly() {
					t.Fatal("fixture did not reopen read-only")
				}
			}
			err = writer.WriteNeedleBlobIfAbsent(id, input, n.Size)
			if state == "absent" {
				if err != nil {
					t.Fatal(err)
				}
				nv, ok := v.nm.Get(n.Id)
				if !ok {
					t.Fatal("repaired needle not indexed")
				}
				var got needle.Needle
				if err := got.ReadData(v.DataBackend, nv.Offset.ToActualOffset(), nv.Size, v.Version()); err != nil {
					t.Fatal(err)
				}
				if got.Cookie != n.Cookie || !bytes.Equal(got.Data, n.Data) {
					t.Fatal("repair changed original payload or cookie")
				}
				dir := filepath.Dir(v.DataFileName())
				v.Close()
				reopened, err := NewVolume(dir, dir, "", 7, NeedleMapInMemory, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.GetCurrentVersion(), 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				nv, ok = reopened.nm.Get(n.Id)
				if !ok {
					t.Fatal("repair lost on reopen")
				}
				if err := got.ReadData(reopened.DataBackend, nv.Offset.ToActualOffset(), nv.Size, reopened.Version()); err != nil {
					t.Fatal(err)
				}
				if got.Cookie != n.Cookie || !bytes.Equal(got.Data, n.Data) {
					t.Fatal("reopened repair differs")
				}
				return
			}
			if err == nil {
				t.Fatal("unsafe repair accepted")
			}
			after, _ := os.ReadFile(v.DataFileName() + ".dat")
			indexAfter, _ := os.ReadFile(v.IndexFileName() + ".idx")
			if !bytes.Equal(before, after) || !bytes.Equal(indexBefore, indexAfter) {
				t.Fatal("rejected repair changed original data/index")
			}
		})
	}
	// One atomic winner even when many repair clients race on the same ID.
	v := newVolume()
	w, ok := any(v).(absentBlobWriter)
	if !ok {
		t.Fatal("missing atomic absent-only repair capability")
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w.WriteNeedleBlobIfAbsent(n.Id, bytes.Clone(blob), n.Size) == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("repair winners=%d, want exactly1", winners.Load())
	}
	for _, stage := range []string{"data", "index"} {
		t.Run("sync-failure-"+stage, func(t *testing.T) {
			v := newVolume()
			failure := errors.New("injected repair sync failure")
			if stage == "data" {
				v.DataBackend = &countingBackend{BackendStorageFile: v.DataBackend, syncErr: failure}
			} else {
				v.nm = &countingNeedleMapper{NeedleMapper: v.nm, syncErr: failure}
			}
			if err := v.WriteNeedleBlobIfAbsent(n.Id, bytes.Clone(blob), n.Size); !errors.Is(err, failure) {
				t.Fatalf("sync failure acknowledged: %v", err)
			}
			if _, ok := v.nm.Get(n.Id); !ok {
				t.Fatal("uncertain write lost mapping")
			}
			if err := v.WriteNeedleBlobIfAbsent(n.Id, bytes.Clone(blob), n.Size); !errors.Is(err, ErrNeedleAlreadyExists) {
				t.Fatalf("uncertain write allowed blind retry: %v", err)
			}
		})
	}
}
