package shell

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/protobuf/proto"
)

func TestRepairNeedleRejectsInvalidArgumentsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{nil, {"-unknown"}, {"-apply=maybe"}, {"-path", "relative"}, {"-path", "/x", "-source", "a:1", "-target", "a:1", "-sha256", fmt.Sprintf("%064d", 0), "-fid", "41,110000007b"}} {
		if err := (&commandVolumeRepairNeedle{}).Do(args, nil, io.Discard); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func TestRepairReferenceValidationDoesNotMutateSnapshot(t *testing.T) {
	fid := needle.NewFileId(41, 17, 123)
	entry := &filer_pb.Entry{Attributes: &filer_pb.FuseAttributes{FileSize: 9}, Chunks: []*filer_pb.FileChunk{{Size: 9, Fid: &filer_pb.FileId{VolumeId: 41, FileKey: 17, Cookie: 123}}}}
	original := proto.Clone(entry)
	if _, err := validateRepairReference(entry, fid); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(original, entry) {
		t.Fatal("validation mutated metadata snapshot")
	}
	if _, err := validateRepairReference(nil, fid); err == nil {
		t.Fatal("nil reference accepted")
	}
}

func TestRepairReadbackAllowsOnlyLocalAppendTimestamp(t *testing.T) {
	size := types.Size(31)
	original := bytes.Repeat([]byte{42}, int(needle.GetActualSize(size, needle.Version3)))
	stamp := types.NeedleHeaderSize + int(size) + needle.NeedleChecksumSize
	for index := range original {
		changed := bytes.Clone(original)
		changed[index] ^= 1
		want := index >= stamp && index < stamp+types.TimestampSize
		if sameRepairRecord(original, changed, size, needle.Version3) != want {
			t.Fatalf("byte %d: only append timestamp may differ", index)
		}
	}
	if sameRepairRecord(original, original[:len(original)-1], size, needle.Version3) {
		t.Fatal("truncation accepted")
	}
	if !sameRepairRecord(original, original, size, needle.Version3) {
		t.Fatal("identical record rejected")
	}
	v2 := bytes.Repeat([]byte{42}, int(needle.GetActualSize(size, needle.Version2)))
	for index := range v2 {
		changed := bytes.Clone(v2)
		changed[index] ^= 1
		if sameRepairRecord(v2, changed, size, needle.Version2) {
			t.Fatalf("v2 byte %d change accepted", index)
		}
	}
}

func TestRepairNeedleReferenceAndPayloadPolicy(t *testing.T) {
	fid, err := needle.ParseFileIdFromString("41,110000007b")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("intact content addressed payload")
	entry := &filer_pb.Entry{Attributes: &filer_pb.FuseAttributes{FileSize: uint64(len(payload))}, Chunks: []*filer_pb.FileChunk{{FileId: fid.String(), Size: uint64(len(payload))}}}
	for _, change := range []string{"valid", "directory", "inline", "missing", "other-cookie", "manifest", "encrypted", "offset", "size", "overlap"} {
		t.Run(change, func(t *testing.T) {
			e := proto.Clone(entry).(*filer_pb.Entry)
			switch change {
			case "directory":
				e.IsDirectory = true
			case "inline":
				e.Content = []byte("inline")
			case "missing":
				e.Chunks = nil
			case "other-cookie":
				e.Chunks[0].FileId = "41,110000007c"
			case "manifest":
				e.Chunks[0].IsChunkManifest = true
			case "encrypted":
				e.Chunks[0].CipherKey = []byte("key")
			case "offset":
				e.Chunks[0].Offset = 1
			case "size":
				e.Attributes.FileSize++
			case "overlap":
				e.Chunks = append(e.Chunks, e.Chunks[0])
			}
			_, err := validateRepairReference(e, fid)
			if (err == nil) != (change == "valid") {
				t.Fatalf("unexpected validation: %v", err)
			}
		})
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(payload))
	for _, change := range []string{"valid", "compressed", "wrong-cookie", "wrong-id", "checksum", "size", "ttl", "manifest", "bad-compression"} {
		t.Run(change, func(t *testing.T) {
			n := &needle.Needle{Id: fid.Key, Cookie: fid.Cookie, Data: payload}
			size := uint64(len(payload))
			switch change {
			case "compressed":
				n.Data, err = util.GzipData(payload)
				if err != nil {
					t.Fatal(err)
				}
				n.SetIsCompressed()
			case "wrong-cookie":
				n.Cookie++
			case "wrong-id":
				n.Id++
			case "checksum":
				n.Data = []byte("different content")
			case "size":
				size++
			case "ttl":
				n.SetHasTtl()
			case "manifest":
				n.SetIsChunkManifest()
			case "bad-compression":
				n.SetIsCompressed()
			}
			err = validateRepairPayload(n, fid, size, hash)
			if (err == nil) != (change == "valid" || change == "compressed") {
				t.Fatalf("unexpected validation: %v", err)
			}
		})
	}
}
