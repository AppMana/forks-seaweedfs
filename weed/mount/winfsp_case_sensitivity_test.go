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

func TestFindWinFspName(t *testing.T) {
	names := []string{"CaseProbe.txt", "Library", "ProjectSettings"}

	if got, found := findWinFspName("caseprobe.TXT", names, false); !found || got != "CaseProbe.txt" {
		t.Fatalf("case-insensitive lookup = %q, %v; want CaseProbe.txt, true", got, found)
	}
	if _, found := findWinFspName("caseprobe.TXT", names, true); found {
		t.Fatal("case-sensitive lookup unexpectedly matched different casing")
	}
	if got, found := findWinFspName("Library", names, true); !found || got != "Library" {
		t.Fatalf("exact lookup = %q, %v; want Library, true", got, found)
	}
}
