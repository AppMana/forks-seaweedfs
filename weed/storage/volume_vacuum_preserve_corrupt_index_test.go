package storage

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
	. "github.com/seaweedfs/seaweedfs/weed/storage/types"
)

func TestVacuumPreservesIntactDataWithBadIndex(t *testing.T) {
	for _, kind := range []NeedleMapKind{NeedleMapInMemory, NeedleMapLevelDb} {
		for _, mode := range []string{"past_eof", "wrong_identity", "bad_crc"} {
			for _, phase := range []string{"copy", "replay"} {
				t.Run(phase+"/"+mode+map[NeedleMapKind]string{NeedleMapInMemory: "/memory", NeedleMapLevelDb: "/leveldb"}[kind], func(t *testing.T) {
					dir := t.TempDir()
					v, err := NewVolume(dir, dir, "", 1, kind, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.Version3, 0, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer v.Close()
					var originals []*needle.Needle
					var offsets []int64
					for id := uint64(1); id <= 2; id++ {
						n := &needle.Needle{Id: Uint64ToNeedleId(id), Data: bytes.Repeat([]byte{byte(id)}, 32)}
						n.Checksum = needle.NewCRC(n.Data)
						offset, _, _, err := v.writeNeedle2(n, true, true, false)
						if err != nil {
							t.Fatal(err)
						}
						originals = append(originals, n)
						offsets = append(offsets, int64(offset))
					}
					if phase == "replay" {
						if err := v.CompactByIndex(nil); err != nil {
							t.Fatal(err)
						}
					}
					badOffset := int64(1 << 20)
					if mode == "wrong_identity" {
						badOffset = offsets[1]
					}
					if mode == "bad_crc" {
						badOffset = offsets[0]
						// Corrupt the first payload but leave its checksum and the
						// second intact payload unchanged. The index Put below
						// forces this entry through replay in that phase.
						if _, err := v.DataBackend.WriteAt([]byte{0xff}, offsets[0]+NeedleHeaderSize+4); err != nil {
							t.Fatal(err)
						}
					}
					if err := v.nm.Put(originals[0].Id, ToOffset(badOffset), originals[0].Size); err != nil {
						t.Fatal(err)
					}
					if err := v.nm.Sync(); err != nil {
						t.Fatal(err)
					}
					readFile := func(ext string) []byte {
						t.Helper()
						data, err := os.ReadFile(v.FileName(ext))
						if err != nil {
							t.Fatal(err)
						}
						return data
					}
					dat, index := readFile(".dat"), readFile(".idx")
					if phase == "copy" {
						if err := v.CompactByIndex(nil); err == nil {
							t.Fatal("vacuum must reject an invalid live index, not discard an intact body")
						}
					}
					if err := v.CommitCompact(); err == nil {
						t.Fatal("failed copy must not be committable")
					}
					if !bytes.Equal(dat, readFile(".dat")) || !bytes.Equal(index, readFile(".idx")) {
						t.Fatal("failed vacuum changed original files")
					}
					if _, err := os.Stat(v.FileName(".cpc")); !os.IsNotExist(err) {
						t.Fatalf("failed copy published commit marker: %v", err)
					}
					for i, original := range originals {
						got := new(needle.Needle)
						err := got.ReadData(v.DataBackend, offsets[i], original.Size, v.Version())
						if mode == "bad_crc" && i == 0 {
							if !errors.Is(err, needle.ErrorCorrupted) {
								t.Fatalf("expected preserved damaged record, got %v", err)
							}
							continue
						}
						if err != nil {
							t.Fatal(err)
						}
						if got.Id != original.Id || !bytes.Equal(got.Data, original.Data) {
							t.Fatal("original physical body was not preserved")
						}
					}
				})
			}
		}
	}
}

func TestCompactByVolumeDataFailureCannotCommitCandidate(t *testing.T) {
	dir := t.TempDir()
	v, err := NewVolume(dir, dir, "", 1, NeedleMapInMemory, &super_block.ReplicaPlacement{}, &needle.TTL{}, 0, needle.Version3, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	n := newEmptyNeedle(1)
	n.Data = bytes.Repeat([]byte{1}, 4096)
	n.Checksum = needle.NewCRC(n.Data)
	offset, _, _, err := v.writeNeedle2(n, true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.DataBackend.WriteAt([]byte{0xff}, int64(offset)+NeedleHeaderSize+4); err != nil {
		t.Fatal(err)
	}
	datBefore, err := os.ReadFile(v.FileName(".dat"))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.CompactByVolumeData(nil); err == nil {
		t.Fatal("data scan must reject corrupt payload")
	}
	if err := v.CommitCompact(); err == nil {
		t.Fatal("failed data scan must not be committable")
	}
	for _, ext := range []string{".cpd", ".cpx", ".cpc"} {
		mustNotExist(t, v.FileName(ext))
	}
	datAfter, err := os.ReadFile(v.FileName(".dat"))
	if err != nil || !bytes.Equal(datBefore, datAfter) {
		t.Fatalf("failed data scan changed original: read=%v", err)
	}
}
