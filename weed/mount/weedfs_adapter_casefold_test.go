package mount

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/filer"
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

func TestCaseFoldPrefixes(t *testing.T) {
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"", []string{""}},
		{"1_", []string{"1_"}},
		{"ab", []string{"ab", "aB", "Ab", "AB"}},
		{"k", []string{"k", "K", "K"}},
		{"IMG_0001.JPG", nil},
		{"\xffabc", []string{""}},
		{"a�b", []string{"a", "A"}},
	} {
		got := caseFoldPrefixes(tc.name, caseFoldPrefixMaxVariants)
		if len(got) > caseFoldPrefixMaxVariants {
			t.Fatalf("%q: %d prefixes, over the bound", tc.name, len(got))
		}
		if tc.want != nil {
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("caseFoldPrefixes(%q) = %q, want %q", tc.name, got, want)
			}
		}
	}
	// Free runes extend the prefix: four cased letters, the digits and the
	// underscore, then the dot, before the next letter would pass 16.
	got := caseFoldPrefixes("IMG_0001.JPG", caseFoldPrefixMaxVariants)
	for _, prefix := range got {
		if len(prefix) != len("IMG_0001.J") {
			t.Fatalf("prefix %q, want each to run through %q", prefix, "IMG_0001.J")
		}
		if !strings.EqualFold(prefix, "IMG_0001.J") {
			t.Fatalf("prefix %q does not fold to the name", prefix)
		}
	}
	if len(got) != 16 {
		t.Fatalf("%d prefixes for IMG_0001.JPG, want 16", len(got))
	}
}

func TestAdapterPrefixListingPagesThroughCachedDirectory(t *testing.T) {
	var names []string
	for i := 0; i < 300; i++ {
		names = append(names, fmt.Sprintf("x-%04d", i), fmt.Sprintf("y-%04d", i))
	}
	names = append(names, "x", "x-", "w-0000")
	wfs, _ := newCaseFoldWFS(t, names, 0)
	for _, prefix := range []string{"x-", "x", ""} {
		var got []string
		if err := wfs.listDirectoryPrefixForAdapter(context.Background(), "/", prefix, func(entry *filer.Entry) (bool, error) {
			got = append(got, entry.Name())
			return true, nil
		}); err != nil {
			t.Fatal(err)
		}
		var want []string
		for _, name := range names {
			if strings.HasPrefix(name, prefix) {
				want = append(want, name)
			}
		}
		sort.Strings(want)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("prefix %q listed %d names, want %d", prefix, len(got), len(want))
		}
	}
	if !wfs.metaCache.IsDirectoryCached("/") {
		t.Fatal("the directory was not cached; this test needs the local listing")
	}
}
