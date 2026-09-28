package winfsp

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFilerRootScopesPersistenceOracle(t *testing.T) {
	rootFlag := flag.Lookup("filer-root")
	if rootFlag == nil {
		t.Fatal("native executable has no filer-root configuration")
	}
	old := rootFlag.Value.String()
	t.Cleanup(func() { _ = rootFlag.Value.Set(old) })
	for _, tc := range []struct{ prefix, relative, want string }{
		{"/", "winfsp-persist/small.txt", "/winfsp-persist/small.txt"},
		{"/buckets/pvc/qualification-token/native", "winfsp-persist/small.txt", "/buckets/pvc/qualification-token/native/winfsp-persist/small.txt"},
		{"/buckets/pvc/", "nested/unicode-café-日本 #?.txt", "/buckets/pvc/nested/unicode-café-日本 #?.txt"},
	} {
		t.Run(tc.prefix+tc.relative, func(t *testing.T) {
			if err := rootFlag.Value.Set(tc.prefix); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.want {
					t.Errorf("filer path=%q want=%q", r.URL.Path, tc.want)
				}
				if r.URL.RawQuery != "" {
					t.Errorf("filename became URL query: %s", r.URL.RawQuery)
				}
				_, _ = w.Write([]byte("intact"))
			}))
			defer server.Close()
			got, err := fetchFromFiler(strings.TrimPrefix(server.URL, "http://"), tc.relative)
			if err != nil || string(got) != "intact" {
				t.Fatalf("oracle fetch=%q,%v", got, err)
			}
		})
	}
}

func TestScopedFilerPathPreservesNamespace(t *testing.T) {
	for _, tc := range []struct{ root, relative, want string }{
		{"/", "file", "/file"},
		{"", "file", "/file"},
		{"/buckets/pvc/native", `winfsp-test\legacy.bin`, "/buckets/pvc/native/winfsp-test/legacy.bin"},
		{"/buckets/pvc/native/", ".", "/buckets/pvc/native"},
	} {
		got, err := scopedFilerPath(tc.root, tc.relative)
		if err != nil || got != tc.want {
			t.Errorf("resolve(%q,%q)=%q,%v want %q", tc.root, tc.relative, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ root, relative string }{
		{"relative-root", "file"}, {"/buckets/pvc", "../other/file"},
		{"/buckets/pvc", `..\other\file`}, {"/buckets/pvc", "/other/file"},
		{"/buckets/../other", "file"},
	} {
		if got, err := scopedFilerPath(tc.root, tc.relative); err == nil {
			t.Errorf("accepted namespace escape %q/%q: %s", tc.root, tc.relative, got)
		}
	}
}
