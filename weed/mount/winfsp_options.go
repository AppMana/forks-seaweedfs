package mount

import (
	"fmt"
	"strings"
)

// WinFsp enables the Windows data cache only for an infinite file-info
// timeout. KeepFileCache must remain absent: the FUSE layer then flushes and
// purges on cleanup rather than retaining pages across the last close.
// Foreign metadata events additionally invalidate cached pages via Notify.
func winFspCacheOptions() []string {
	return []string{
		"-o", "FileInfoTimeout=-1",
		"-o", "DirInfoTimeout=2000",
		"-o", "VolumeInfoTimeout=5000",
		"-o", "EaTimeout=1000",
	}
}

// Basic mode must not claim enforcement while WinFsp substitutes another
// identity, fixed DACL or mode. Validate once at mount, never on file access.
func validateWinFspBasicOptions(options []string) error {
	for _, option := range options {
		option = strings.TrimSpace(option)
		option = strings.TrimPrefix(option, "-o")
		for _, part := range strings.Split(option, ",") {
			key, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			switch strings.ToLower(key) {
			case "filesecurity", "uid", "gid", "username", "groupname", "uidmap",
				"umask", "create_umask", "create_file_umask", "create_dir_umask":
				return fmt.Errorf("WinFsp option %q conflicts with winfspBasicPermissions", key)
			}
		}
	}
	return nil
}
