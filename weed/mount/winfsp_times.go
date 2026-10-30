package mount

// winfspTimeApplies reports whether a timestamp WinFsp passes to Utimens is
// one to write.
func winfspTimeApplies(sec, nsec int64) bool {
	return true
}
