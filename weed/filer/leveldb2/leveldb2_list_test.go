package leveldb

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

func newListTestStore(tb testing.TB) *LevelDB2Store {
	store := &LevelDB2Store{}
	if err := store.initialize(tb.TempDir(), 2); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(store.Shutdown)
	return store
}

func listStoreNames(t *testing.T, store *LevelDB2Store, dir, start string, inclusive bool, prefix string) []string {
	t.Helper()
	var names []string
	if _, err := store.ListDirectoryPrefixedEntries(context.Background(), util.FullPath(dir), start, inclusive, 100, prefix, func(e *filer.Entry) (bool, error) {
		names = append(names, e.Name())
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	return names
}

func TestListStaysInsideTheDirectory(t *testing.T) {
	store := newListTestStore(t)
	ctx := context.Background()
	for _, p := range []string{"/d/a", "/d/b1", "/d/b2", "/d/c", "/d/\U0010FFFF", "/d/sub/x", "/e/a", "/d2/a"} {
		if err := store.InsertEntry(ctx, &filer.Entry{FullPath: util.FullPath(p)}); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		start     string
		inclusive bool
		prefix    string
		want      []string
	}{
		{want: []string{"a", "b1", "b2", "c", "\U0010FFFF"}},
		{start: "b1", want: []string{"b2", "c", "\U0010FFFF"}},
		{start: "b1", inclusive: true, want: []string{"b1", "b2", "c", "\U0010FFFF"}},
		{prefix: "b", want: []string{"b1", "b2"}},
		{prefix: "\U0010FFFF", want: []string{"\U0010FFFF"}},
		{prefix: "z"},
	}
	for _, tc := range cases {
		got := listStoreNames(t, store, "/d", tc.start, tc.inclusive, tc.prefix)
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("list /d start %q inclusive %v prefix %q = %q, want %q", tc.start, tc.inclusive, tc.prefix, got, tc.want)
		}
	}
	if got := listStoreNames(t, store, "/empty", "", false, ""); len(got) != 0 {
		t.Errorf("list /empty = %q", got)
	}
}

// BenchmarkListEmptiedDirectory lists a directory whose entries were all
// deleted, among many other deleted entries, as the filer does when it checks
// whether a folder or a shared chunk group is empty.
func BenchmarkListEmptiedDirectory(b *testing.B) {
	store := newListTestStore(b)
	ctx := context.Background()
	const dirs = 20000
	for i := 0; i < dirs; i++ {
		p := util.FullPath(fmt.Sprintf("/dir%d/entry", i))
		if err := store.InsertEntry(ctx, &filer.Entry{FullPath: p}); err != nil {
			b.Fatal(err)
		}
		if err := store.DeleteEntry(ctx, p); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.ListDirectoryEntries(ctx, util.FullPath(fmt.Sprintf("/dir%d", i%dirs)), "", false, 1, func(*filer.Entry) (bool, error) {
			b.Fatal("an emptied directory listed an entry")
			return false, nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}
