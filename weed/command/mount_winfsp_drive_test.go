package command

import "testing"

func TestWinFspDriveMountPoint(t *testing.T) {
	for _, tc := range []struct {
		dir   string
		drive string
		ok    bool
	}{
		{`S:`, `S:`, true},
		{`S:\`, `S:`, true},
		{`S:/`, `S:`, true},
		{`s:\`, `s:`, true},
		{`S:\mnt`, "", false},
		{`C:\var\lib\kubelet\pods\x\volumes\mount`, "", false},
		{`\var\lib\kubelet\mount`, "", false},
		{`\\server\share`, "", false},
		{`1:\`, "", false},
		{`:\`, "", false},
		{``, "", false},
	} {
		drive, ok := winFspDriveMountPoint(tc.dir)
		if drive != tc.drive || ok != tc.ok {
			t.Errorf("winFspDriveMountPoint(%q) = %q, %v; want %q, %v", tc.dir, drive, ok, tc.drive, tc.ok)
		}
	}
}
