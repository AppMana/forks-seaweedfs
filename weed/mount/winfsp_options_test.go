package mount

import (
	"reflect"
	"testing"
)

func TestWinFspCacheOptions(t *testing.T) {
	want := []string{
		"-o", "FileInfoTimeout=-1",
		"-o", "DirInfoTimeout=2000",
		"-o", "VolumeInfoTimeout=5000",
		"-o", "EaTimeout=1000",
	}
	if got := winFspCacheOptions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("cache options = %v, want %v (without KeepFileCache)", got, want)
	}
	// A caller appending explicit per-volume overrides must not mutate defaults.
	options := winFspCacheOptions()
	options[1] = "FileInfoTimeout=1000"
	if !reflect.DeepEqual(winFspCacheOptions(), want) {
		t.Fatal("per-mount override mutated shared defaults")
	}
}

func TestWinFspBasicOptionsRejectPermissionOverrides(t *testing.T) {
	for _, option := range []string{
		"FileSecurity=D:P(A;;FA;;;WD)", "uid=-1", "gid=-1", "UserName=SYSTEM",
		"GroupName=Users", "uidmap=18:544", "umask=000", "create_umask=000",
		"create_file_umask=000", "create_dir_umask=000", "FILESECURITY=x",
		"FileInfoTimeout=1000,uid=18", "-oumask=000", "  uid=18",
	} {
		t.Run(option, func(t *testing.T) {
			if err := validateWinFspBasicOptions([]string{"-o", option}); err == nil {
				t.Fatal("accepted contradictory permissions")
			}
		})
	}
	for _, options := range [][]string{nil, {"-o", "debug"}, {"-o", "FileInfoTimeout=1000,DirInfoTimeout=2000"}, {"-o", "volname=uid=data"}} {
		if err := validateWinFspBasicOptions(options); err != nil {
			t.Fatal(err)
		}
	}
}
