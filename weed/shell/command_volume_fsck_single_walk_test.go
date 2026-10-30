package shell

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/volume_server_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/needle_map"
	"github.com/seaweedfs/seaweedfs/weed/storage/types"
)

type fakeFilerRef struct {
	path       string
	key        uint64
	modifiedNs int64
}

// singleWalkFsck is a volume.fsck over volume 7 on replicas a and b whose
// filer walk serves refs and counts how often it runs. Every needle was
// appended at 1 ns, older than any cutoff.
func singleWalkFsck(t *testing.T, refs []fakeFilerRef, out *bytes.Buffer) (c *commandVolumeFsck, walks *int, writes, deletes *[]string) {
	t.Helper()
	walks, writes, deletes = new(int), new([]string), new([]string)
	var lock sync.Mutex
	verbose, force, skipEc, verify, findMissing := false, true, false, false, false
	collection := ""
	c = &commandVolumeFsck{
		writer:                   out,
		tempFolder:               t.TempDir(),
		verbose:                  &verbose,
		forcePurging:             &force,
		skipEcVolumes:            &skipEc,
		verifyNeedle:             &verify,
		findMissingChunksInFiler: &findMissing,
		collection:               &collection,
		walkFiler: func(path string, genFn func(context.Context, *filer_pb.FullEntry, chan interface{}) error, saveFn func(chan interface{}) error) error {
			*walks++
			ch := make(chan interface{}, len(refs))
			saved := make(chan error, 1)
			go func() { saved <- saveFn(ch) }()
			for _, r := range refs {
				dir, name := r.path[:strings.LastIndex(r.path, "/")], r.path[strings.LastIndex(r.path, "/")+1:]
				entry := &filer_pb.FullEntry{Dir: dir, Entry: &filer_pb.Entry{Name: name, Chunks: []*filer_pb.FileChunk{{
					Fid:          &filer_pb.FileId{VolumeId: 7, FileKey: r.key, Cookie: 0},
					Size:         10,
					ModifiedTsNs: r.modifiedNs,
				}}}}
				if err := genFn(context.Background(), entry, ch); err != nil {
					t.Fatal(err)
				}
			}
			close(ch)
			return <-saved
		},
		readNeedleMeta: func(pb.ServerAddress, uint32, needle_map.NeedleValue) (uint64, error) {
			return 1, nil
		},
		readNeedleBlob: func(server pb.ServerAddress, vid uint32, n needle_map.NeedleValue) ([]byte, error) {
			return []byte(fmt.Sprintf("blob-%d", n.Key)), nil
		},
		writeNeedleBlobIfAbsent: func(server pb.ServerAddress, vid uint32, n needle_map.NeedleValue, blob []byte) error {
			lock.Lock()
			defer lock.Unlock()
			*writes = append(*writes, fmt.Sprintf("%s:%d", server, n.Key))
			return nil
		},
		volumeServers: func(uint32) ([]pb.ServerAddress, bool) {
			return []pb.ServerAddress{"a:8080", "b:8080"}, true
		},
		deleteFileIds: func(server pb.ServerAddress, fileIds []string) []*volume_server_pb.DeleteResult {
			lock.Lock()
			defer lock.Unlock()
			var results []*volume_server_pb.DeleteResult
			for _, fid := range fileIds {
				*deletes = append(*deletes, fmt.Sprintf("%s:%s", server, fid))
				results = append(results, &volume_server_pb.DeleteResult{FileId: fid, Status: http.StatusAccepted})
			}
			return results
		},
	}
	return
}

func singleWalkVolumes() map[string]map[uint32]VInfo {
	return map[string]map[uint32]VInfo{
		"dnA": {7: {server: "a:8080", collection: "b"}},
		"dnB": {7: {server: "b:8080", collection: "b"}},
	}
}

// Restoring missed writes, reporting orphans and purging them each need the
// filer's references, and each used to be its own volume.fsck run, so a
// weekly repair walked a 4M-entry namespace three times. One run must walk it
// once and do all three. A reference newer than the cutoff still protects
// its needle from the purge but proves no missed write, so it is not copied.
func TestFsckRepairsReportsAndPurgesInOneFilerWalk(t *testing.T) {
	cutoff := time.Now().Add(-5 * time.Hour).UnixNano()
	var out bytes.Buffer
	c, walks, writes, deletes := singleWalkFsck(t, []fakeFilerRef{
		{"/buckets/b/one", 1, cutoff - 1},
		{"/buckets/b/two", 2, cutoff - 1},
		{"/buckets/b/fresh", 4, cutoff + 1},
	}, &out)
	// 2 is a write b missed; 3 and 9 are referenced by nothing.
	writeReplicaIdx(t, c.tempFolder, "dnA", 7, 1, 2, 3, 4, 9)
	writeReplicaIdx(t, c.tempFolder, "dnB", 7, 1, 9)

	err := c.checkVolumes(singleWalkVolumes(), fsckChecks{repair: true, applyPurging: true, cutoffFromAtNs: cutoff, maxOrphanPercent: 100})
	if err != nil {
		t.Fatalf("fsck: %v\n%s", err, out.String())
	}
	if *walks != 1 {
		t.Fatalf("filer walked %d times, want 1", *walks)
	}
	if got := strings.Join(*writes, " "); got != "b:8080:2" {
		t.Fatalf("repair writes %q, want the one missed write b:8080:2\n%s", got, out.String())
	}
	sort.Strings(*deletes)
	var want []string
	for _, server := range []string{"a:8080", "b:8080"} {
		for _, key := range []uint64{3, 9} {
			want = append(want, server+":"+types.NeedleId(key).FileId(7))
		}
	}
	if got := strings.Join(*deletes, " "); got != strings.Join(want, " ") {
		t.Fatalf("purged %q, want %q\n%s", got, strings.Join(want, " "), out.String())
	}
	if !strings.Contains(out.String(), "repaired 1 missing replica needle(s); 0 referenced needle(s) on no replica") {
		t.Fatalf("repair summary missing:\n%s", out.String())
	}
}

// A walk that missed part of the namespace reports live chunks as orphans,
// so above -maxOrphanPercent the run reports, deletes nothing and fails. The
// restore still runs: it only adds needles the filer references.
func TestFsckPurgesNothingAboveTheOrphanLimit(t *testing.T) {
	var out bytes.Buffer
	c, walks, writes, deletes := singleWalkFsck(t, []fakeFilerRef{
		{"/buckets/b/one", 1, 0},
		{"/buckets/b/two", 2, 0},
	}, &out)
	writeReplicaIdx(t, c.tempFolder, "dnA", 7, 1, 2, 3, 9)
	writeReplicaIdx(t, c.tempFolder, "dnB", 7, 1, 9)

	// orphans: 3 and 9 on a, 9 on b; entries count per replica, 3 of 7
	err := c.checkVolumes(singleWalkVolumes(), fsckChecks{repair: true, applyPurging: true, maxOrphanPercent: 40})
	if err == nil {
		t.Fatalf("orphans above the limit must fail the run\n%s", out.String())
	}
	if *walks != 1 || len(*deletes) != 0 {
		t.Fatalf("walks %d, deletes %v; want one walk and no deletes\n%s", *walks, *deletes, out.String())
	}
	if strings.Join(*writes, " ") != "b:8080:2" {
		t.Fatalf("repair writes %v\n%s", *writes, out.String())
	}
	if !strings.Contains(out.String(), "Total\t\tentries:7\torphan:3\t42.86%") {
		t.Fatalf("the report must still be printed:\n%s", out.String())
	}

	out.Reset()
	*walks, *deletes = 0, nil
	if err := c.checkVolumes(singleWalkVolumes(), fsckChecks{applyPurging: true, maxOrphanPercent: 50}); err != nil {
		t.Fatalf("at the limit: %v\n%s", err, out.String())
	}
	if len(*deletes) == 0 {
		t.Fatalf("orphans at the limit must be purged\n%s", out.String())
	}
}
