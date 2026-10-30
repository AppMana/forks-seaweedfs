package shell

import (
	"bytes"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle_map"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func writeReplicaIdx(t *testing.T, tempFolder, dataNodeId string, volumeId uint32, keys ...uint64) {
	t.Helper()
	db := needle_map.NewMemDb()
	defer db.Close()
	for _, key := range keys {
		if err := db.Set(types.NeedleId(key), types.ToOffset(int64(key*64)), types.Size(10+key)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SaveToIdx(getVolumeFileIdFile(tempFolder, dataNodeId, volumeId)); err != nil {
		t.Fatal(err)
	}
}

func writeFilerRefs(t *testing.T, tempFolder string, volumeId uint32, refs map[string]uint64) {
	t.Helper()
	paths := make([]string, 0, len(refs))
	for p := range refs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var buf bytes.Buffer
	for _, p := range paths {
		record := make([]byte, readbufferSize)
		util.Uint64toBytes(record, refs[p])
		util.Uint32toBytes(record[8:], 0xabc)
		util.Uint32toBytes(record[12:], uint32(len(p)))
		buf.Write(record)
		buf.WriteString(p)
		buf.Write([]byte{0, 0, 0, 0}) // no hard link id
	}
	if err := writeToFile(buf.Bytes(), getFilerFileIdFile(tempFolder, volumeId)); err != nil {
		t.Fatal(err)
	}
}

// A filer-referenced needle that a replica lacks is a write that replica
// missed: the filer still points at it, and the chunk is older than the
// cutoff, so it is not a delete. -repairMissingFromReplicas copies exactly
// that needle from a replica holding it live, through the absent-only RPC,
// and reports a needle no replica holds instead of inventing it.
func TestFsckRepairsFilerReferencedNeedlesFromAnotherReplica(t *testing.T) {
	tempFolder := t.TempDir()
	const volumeId = uint32(7)
	writeReplicaIdx(t, tempFolder, "dnA", volumeId, 1, 2, 3, 5)
	writeReplicaIdx(t, tempFolder, "dnB", volumeId, 1, 3)
	writeReplicaIdx(t, tempFolder, "dnC", volumeId, 1)
	writeReplicaIdx(t, tempFolder, "dnRO", volumeId, 1)
	writeFilerRefs(t, tempFolder, volumeId, map[string]uint64{
		"/buckets/b/one":     1,
		"/buckets/b/two":     2,
		"/buckets/b/three":   3,
		"/buckets/b/lost":    4,
		"/buckets/b/five":    5,
		"/buckets/b/fiveToo": 5,
	})

	var reads, writes []string
	verbose := false
	var out bytes.Buffer
	c := &commandVolumeFsck{
		writer:     &out,
		tempFolder: tempFolder,
		verbose:    &verbose,
		readNeedleBlob: func(server pb.ServerAddress, vid uint32, n needle_map.NeedleValue) ([]byte, error) {
			reads = append(reads, fmt.Sprintf("%s:%d", server, n.Key))
			return []byte(fmt.Sprintf("blob-%d@%d+%d", n.Key, n.Offset.ToActualOffset(), n.Size)), nil
		},
		writeNeedleBlobIfAbsent: func(server pb.ServerAddress, vid uint32, n needle_map.NeedleValue, blob []byte) error {
			writes = append(writes, fmt.Sprintf("%s:%d:%s", server, n.Key, blob))
			if server == "c:8080" && n.Key == 3 {
				// Another repairer got there first: present is the goal.
				return status.Error(codes.AlreadyExists, "needle already exists")
			}
			return nil
		},
	}
	volumes := map[string]map[uint32]VInfo{
		"dnA":  {volumeId: {server: "a:8080", collection: "b"}},
		"dnB":  {volumeId: {server: "b:8080", collection: "b"}},
		"dnC":  {volumeId: {server: "c:8080", collection: "b"}},
		"dnRO": {volumeId: {server: "ro:8080", collection: "b", isReadOnly: true}},
	}

	repaired, lost, err := c.repairMissingFromReplicas(volumes, getFilerFileIdFile)
	if err != nil {
		t.Fatalf("repair: %v\n%s", err, out.String())
	}

	sort.Strings(writes)
	want := []string{
		"b:8080:2:blob-2@128+12",
		"b:8080:5:blob-5@320+15",
		"c:8080:2:blob-2@128+12",
		"c:8080:3:blob-3@192+13",
		"c:8080:5:blob-5@320+15",
	}
	if strings.Join(writes, " ") != strings.Join(want, " ") {
		t.Fatalf("writes\n got %v\nwant %v", writes, want)
	}
	for _, r := range reads {
		if !strings.HasPrefix(r, "a:8080:") && !strings.HasPrefix(r, "b:8080:") {
			t.Fatalf("read %s from a replica that lacks the needle", r)
		}
	}
	// c:8080 already held needle 3 by the time it was written.
	if repaired != len(want)-1 {
		t.Fatalf("repaired %d, want %d", repaired, len(want)-1)
	}
	if lost != 1 || !strings.Contains(out.String(), "7,4") || !strings.Contains(out.String(), "/buckets/b/lost") {
		t.Fatalf("needle held by no replica must be reported with its path, lost=%d:\n%s", lost, out.String())
	}
	if !strings.Contains(out.String(), "ro:8080") || !strings.Contains(out.String(), "read-only") {
		t.Fatalf("a read-only lagging replica must be reported, not written:\n%s", out.String())
	}
}

// Any write failure other than "already present" leaves the replica short,
// so the run must not report success.
func TestFsckRepairFailsOnWriteError(t *testing.T) {
	tempFolder := t.TempDir()
	const volumeId = uint32(9)
	writeReplicaIdx(t, tempFolder, "dnA", volumeId, 1)
	writeReplicaIdx(t, tempFolder, "dnB", volumeId)
	writeFilerRefs(t, tempFolder, volumeId, map[string]uint64{"/buckets/b/one": 1})

	verbose := false
	c := &commandVolumeFsck{
		writer:     &bytes.Buffer{},
		tempFolder: tempFolder,
		verbose:    &verbose,
		readNeedleBlob: func(pb.ServerAddress, uint32, needle_map.NeedleValue) ([]byte, error) {
			return []byte("blob"), nil
		},
		writeNeedleBlobIfAbsent: func(pb.ServerAddress, uint32, needle_map.NeedleValue, []byte) error {
			return status.Error(codes.FailedPrecondition, "volume 9 is read only")
		},
	}
	volumes := map[string]map[uint32]VInfo{
		"dnA": {volumeId: {server: "a:8080", collection: "b"}},
		"dnB": {volumeId: {server: "b:8080", collection: "b"}},
	}
	if _, _, err := c.repairMissingFromReplicas(volumes, getFilerFileIdFile); err == nil {
		t.Fatal("a failed repair write must fail the run")
	}
}

// Each replica index is an in-memory LevelDB with its own goroutines and
// write buffer. A whole-cluster repair walks ~1300 volumes; holding every
// volume's indexes until the run ends exhausted a 4 GiB job on 2026-10-23.
// A volume's indexes must be closed before the next volume's are loaded.
func TestFsckRepairReleasesEachVolumeIndexesBeforeTheNext(t *testing.T) {
	tempFolder := t.TempDir()
	volumes := map[string]map[uint32]VInfo{"dnA": {}, "dnB": {}}
	const count = 20
	for vid := uint32(1); vid <= count; vid++ {
		writeReplicaIdx(t, tempFolder, "dnA", vid, 1, 2)
		writeReplicaIdx(t, tempFolder, "dnB", vid, 1)
		writeFilerRefs(t, tempFolder, vid, map[string]uint64{"/buckets/b/two": 2})
		volumes["dnA"][vid] = VInfo{server: "a:8080", collection: "b"}
		volumes["dnB"][vid] = VInfo{server: "b:8080", collection: "b"}
	}

	goroutines := map[uint32]int{}
	verbose := false
	c := &commandVolumeFsck{
		writer:     &bytes.Buffer{},
		tempFolder: tempFolder,
		verbose:    &verbose,
		readNeedleBlob: func(_ pb.ServerAddress, vid uint32, _ needle_map.NeedleValue) ([]byte, error) {
			goroutines[vid] = runtime.NumGoroutine()
			return []byte("blob"), nil
		},
		writeNeedleBlobIfAbsent: func(pb.ServerAddress, uint32, needle_map.NeedleValue, []byte) error {
			return nil
		},
	}
	repaired, _, err := c.repairMissingFromReplicas(volumes, getFilerFileIdFile)
	if err != nil || repaired != count {
		t.Fatalf("repaired %d, err %v", repaired, err)
	}
	// An open index runs six goroutines. A closed one keeps only its pool
	// drainer, for up to a second after Close, so each finished volume may
	// leave at most one per replica.
	if growth := goroutines[count] - goroutines[1]; growth > 2*(count-1) {
		t.Fatalf("goroutines while repairing volume 1: %d, volume %d: %d; earlier volumes' indexes are still open",
			goroutines[1], count, goroutines[count])
	}
}
