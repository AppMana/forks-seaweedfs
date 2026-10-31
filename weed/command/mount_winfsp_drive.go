package command

// winFspDriveMountPoint reports whether -dir names a whole drive, which WinFsp
// mounts as is, and returns it in the bare "S:" form WinFsp expects. Any other
// directory is a mount point WinFsp creates, so it must not exist yet.
func winFspDriveMountPoint(dir string) (string, bool) {
	if len(dir) == 3 && (dir[2] == '\\' || dir[2] == '/') {
		dir = dir[:2]
	}
	if len(dir) != 2 || dir[1] != ':' {
		return "", false
	}
	if letter := dir[0] | 0x20; letter < 'a' || letter > 'z' {
		return "", false
	}
	return dir, true
}
