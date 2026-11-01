package mount

const (
	// winfspUtimeOmit is UTIME_OMIT: leave this timestamp as it is.
	winfspUtimeOmit = (1 << 30) - 2

	// winfspWindowsEpochCutoff is 1601-01-02 in Unix seconds. A time at or
	// before it is Windows' own epoch leaking through, not a real time.
	winfspWindowsEpochCutoff = -11644387200
)

// winfspTimeApplies reports whether a timestamp WinFsp passes to Utimens is
// one to write. WinFsp passes UTIME_OMIT, zero or its 1601 epoch for a field
// the caller is not setting; storing zero put 1970 in the atime overlay, and
// 1601 would be stored for every other client to read. A real date before
// 1970 still applies: only the epoch itself is a sentinel.
func winfspTimeApplies(sec, nsec int64) bool {
	if nsec == winfspUtimeOmit {
		return false
	}
	if sec == 0 && nsec == 0 {
		return false
	}
	return sec > winfspWindowsEpochCutoff
}
