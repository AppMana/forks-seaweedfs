package mount

// Windows applications generally require case-insensitive filesystem semantics.
// Keep case-sensitive mounts available for workloads that rely on the filer namespace.
const defaultWinFspCaseSensitive = false

func winFspCaseInsensitive(caseSensitive bool) bool {
	return !caseSensitive
}

// DefaultWinFspCaseSensitive is the default for the weed mount Windows option.
const DefaultWinFspCaseSensitive = defaultWinFspCaseSensitive
