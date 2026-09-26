package mount

import "testing"

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
