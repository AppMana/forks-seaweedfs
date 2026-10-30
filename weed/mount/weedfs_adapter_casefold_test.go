package mount

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// caseFoldFiler lists a fixed directory the way the filer does, honouring the
// prefix, start and limit of each request, and counts what it streams.
type caseFoldFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	names    []string
	streamed atomic.Int64
}

func (s *caseFoldFiler) ListEntries(req *filer_pb.ListEntriesRequest, stream filer_pb.SeaweedFiler_ListEntriesServer) error {
	var sent uint32
	for _, name := range s.names {
		if !strings.HasPrefix(name, req.Prefix) {
			continue
		}
		if name < req.StartFromFileName || (name == req.StartFromFileName && !req.InclusiveStartFrom) {
			continue
		}
		if req.Limit > 0 && sent >= req.Limit {
			break
		}
		if err := stream.Send(&filer_pb.ListEntriesResponse{Entry: &filer_pb.Entry{
			Name:       name,
			Attributes: &filer_pb.FuseAttributes{FileMode: 0o100644, FileSize: 1},
		}}); err != nil {
			return err
		}
		sent++
		s.streamed.Add(1)
	}
	return nil
}

func newCaseFoldWFS(t *testing.T, names []string, maxCachedEntries int) (*WFS, *caseFoldFiler) {
	t.Helper()
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	wfs := newLookupCacheTestWFS(t, 60)
	wfs.option.CacheDirMaxEntries = maxCachedEntries
	fake := &caseFoldFiler{names: sorted}
	startFakeFiler(t, wfs, fake)
	return wfs, fake
}

// A Windows lookup that misses by exact name is retried case-folded. In a
// directory too large to cache that used to stream the whole listing from the
// filer, on every miss.
func TestCaseFoldedLookupDoesNotListTheWholeDirectory(t *testing.T) {
	const total = 2000
	var names []string
	for i := 0; i < total; i++ {
		names = append(names, fmt.Sprintf("file-%04d.txt", i))
	}
	wfs, fake := newCaseFoldWFS(t, names, 100)
	ctx := context.Background()
	if _, err := wfs.findCaseFoldedChild(ctx, "/", "warm-up"); err != nil {
		t.Fatal(err)
	}
	if !wfs.inodeToPath.ShouldReadDirectoryDirect("/") {
		t.Fatal("the directory was cached; this test needs one read through from the filer")
	}
	for _, tc := range []struct{ requested, want string }{
		{"FILE-1234.TXT", "file-1234.txt"},
		{"file-1999.TXT", "file-1999.txt"},
		{"desktop.ini", ""},
		{"FILE-9999.TXT", ""},
	} {
		fake.streamed.Store(0)
		got, err := wfs.findCaseFoldedChild(ctx, "/", tc.requested)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("%q resolved to %q, want %q", tc.requested, got, tc.want)
		}
		if streamed := fake.streamed.Load(); streamed > 16 {
			t.Fatalf("resolving %q streamed %d of %d entries from the filer", tc.requested, streamed, total)
		}
	}
}

func TestCaseFoldedLookupMatchesEqualFold(t *testing.T) {
	names := []string{
		"README", "Readme", "readme.txt", "Kelvin", "straſe", "x", "X-ray",
		"École", "été", "dataÿ", "IMG_0001.JPG", "img_0002.jpg",
	}
	for _, maxCached := range []int{0, 2} {
		t.Run(fmt.Sprintf("maxCached=%d", maxCached), func(t *testing.T) {
			wfs, _ := newCaseFoldWFS(t, names, maxCached)
			for _, tc := range []struct{ requested, want string }{
				// Several names fold together: the first in listing order wins.
				{"readme", "README"},
				{"ReadMe.TXT", "readme.txt"},
				{"KELVIN", "Kelvin"},
				{"kelvin", "Kelvin"},
				{"STRASE", "straſe"},
				{"strase", "straſe"},
				{"X", "x"},
				{"x-RAY", "X-ray"},
				{"éCOLE", "École"},
				{"ÉTÉ", "été"},
				{"DATAŸ", "dataÿ"},
				{"img_0001.jpg", "IMG_0001.JPG"},
				{"IMG_0002.JPG", "img_0002.jpg"},
				{"readm", ""},
				{"readme.txt.bak", ""},
				{"missing", ""},
				{"\xff", ""},
			} {
				got, err := wfs.findCaseFoldedChild(context.Background(), util.FullPath("/"), tc.requested)
				if err != nil {
					t.Fatal(err)
				}
				if got != tc.want {
					t.Errorf("%q resolved to %q, want %q", tc.requested, got, tc.want)
				}
			}
		})
	}
}
