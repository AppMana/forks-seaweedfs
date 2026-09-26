package mount

import (
	"fmt"
	"strings"
)

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
