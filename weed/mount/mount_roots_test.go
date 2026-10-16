package mount

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVolumeTargetFromMountRoots(t *testing.T) {
	data := containerMounts{{Path: `C:\data`}}
	for _, tc := range []struct {
		name   string
		target string
		views  []containerMounts
		want   string
		mapped bool
	}{
		{"under the mount", "C:/data/a.txt", []containerMounts{data}, "/a.txt", true},
		{"case folded", "c:/DATA/dir/a.txt", []containerMounts{data}, "/dir/a.txt", true},
		{"mount root", "C:/data", []containerMounts{data}, "/", true},
		{"trailing separator", "C:/data/dir/", []containerMounts{data}, "/dir", true},
		{"prefix sibling", "C:/database/a.txt", []containerMounts{data}, "", false},
		{"other drive", "D:/data/a.txt", []containerMounts{data}, "", false},
		{"outside", "C:/Windows/win.ini", []containerMounts{data}, "", false},
		{"dotdot escapes", "C:/data/../x", []containerMounts{data}, "", false},
		{"dot component", "C:/data/./x", []containerMounts{data}, "", false},
		{"not a drive target", "/data/a.txt", []containerMounts{data}, "", false},
		{"relative", "a.txt", []containerMounts{data}, "", false},
		{"no publishes", "C:/data/a.txt", nil, "", false},
		{"every container agrees", "C:/data/a.txt",
			[]containerMounts{data, {{Path: `c:\data\`}}}, "/a.txt", true},
		{"containers disagree", "C:/data/a.txt",
			[]containerMounts{data, {{Path: `C:\other`}}}, "", false},
		{"subPath", "C:/data/a.txt",
			[]containerMounts{{{Path: `C:\data`, SubPath: `cache\x`}}}, "/cache/x/a.txt", true},
		{"subPath disagrees", "C:/data/a.txt",
			[]containerMounts{{{Path: `C:\data`, SubPath: "a"}}, {{Path: `C:\data`, SubPath: "b"}}}, "", false},
		{"subPath escapes", "C:/data/a.txt",
			[]containerMounts{{{Path: `C:\data`, SubPath: `..\x`}}}, "", false},
		{"nested mount wins", "C:/data/sub/f",
			[]containerMounts{{{Path: `C:\data`}, {Path: `C:\data\sub`, SubPath: "s"}}}, "/s/f", true},
		{"outer of nested", "C:/data/f",
			[]containerMounts{{{Path: `C:\data`}, {Path: `C:\data\sub`, SubPath: "s"}}}, "/f", true},
		{"nested in one container only", "C:/data/sub/f",
			[]containerMounts{{{Path: `C:\data`}, {Path: `C:\data\sub`, SubPath: "s"}}, data}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, mapped := volumeTargetFromMountRoots(tc.target, tc.views)
			if got != tc.want || mapped != tc.mapped {
				t.Fatalf("volumeTargetFromMountRoots(%q) = %q, %v; want %q, %v", tc.target, got, mapped, tc.want, tc.mapped)
			}
		})
	}
}

func TestMountRootTargetReadsPublishedRoots(t *testing.T) {
	file := filepath.Join(t.TempDir(), "roots.json")
	wfs := &WFS{option: &Option{MountRootsFile: file}}
	if got := wfs.mountRootTarget("C:/data/a.txt"); got != "C:/data/a.txt" {
		t.Fatalf("without a roots file the target is kept as written: %q", got)
	}
	if err := os.WriteFile(file, []byte(`{"publishes":{"c:\\var\\lib\\kubelet\\pods\\1\\mount":[[{"path":"C:\\data"}]]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := wfs.mountRootTarget("C:/data/a.txt"); got != "/a.txt" {
		t.Fatalf("published root must map the target into the volume: %q", got)
	}
	if err := os.WriteFile(file, []byte(`{"publishes":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := wfs.mountRootTarget("C:/data/a.txt"); got != "C:/data/a.txt" {
		t.Fatalf("an unpublished root must no longer map: %q", got)
	}
	if err := os.WriteFile(file, []byte(`not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := wfs.mountRootTarget("C:/data/a.txt"); got != "C:/data/a.txt" {
		t.Fatalf("an unreadable roots file must keep the target as written: %q", got)
	}
}
