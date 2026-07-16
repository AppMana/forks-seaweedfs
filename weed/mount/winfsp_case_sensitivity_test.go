package mount

import "testing"

func TestWinFspCaseSensitivityDefaultsToInsensitive(t *testing.T) {
	if defaultWinFspCaseSensitive {
		t.Fatal("Windows mounts must default to case-insensitive")
	}
	if !winFspCaseInsensitive(defaultWinFspCaseSensitive) {
		t.Fatal("default Windows mount must advertise case-insensitive semantics")
	}
}

func TestWinFspCaseSensitivityCanBeEnabled(t *testing.T) {
	if winFspCaseInsensitive(true) {
		t.Fatal("explicit case-sensitive mode must not advertise case-insensitive semantics")
	}
}
