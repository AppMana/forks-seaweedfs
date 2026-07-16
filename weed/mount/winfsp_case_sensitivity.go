package mount

import "strings"

// Windows applications generally require case-insensitive filesystem semantics.
// Keep case-sensitive mounts available for workloads that rely on the filer namespace.
const defaultWinFspCaseSensitive = false

func winFspCaseInsensitive(caseSensitive bool) bool {
	return !caseSensitive
}

func winFspNameEqual(requested, candidate string, caseSensitive bool) bool {
	return requested == candidate || (!caseSensitive && strings.EqualFold(requested, candidate))
}

func findWinFspName(requested string, names []string, caseSensitive bool) (string, bool) {
	for _, name := range names {
		if winFspNameEqual(requested, name, true) {
			return name, true
		}
	}
	if caseSensitive {
		return "", false
	}
	for _, name := range names {
		if winFspNameEqual(requested, name, false) {
			return name, true
		}
	}
	return "", false
}

func winFspDirectoryListingPath(requested, canonical string, caseSensitive bool) string {
	if !caseSensitive && canonical != "" {
		return canonical
	}
	return requested
}

// DefaultWinFspCaseSensitive is the default for the weed mount Windows option.
const DefaultWinFspCaseSensitive = defaultWinFspCaseSensitive
