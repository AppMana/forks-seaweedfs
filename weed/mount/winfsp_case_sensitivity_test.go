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

func TestWinFspDirectoryListingUsesCanonicalPathWhenCaseInsensitive(t *testing.T) {
	requested := "/REPOSITORY/BUILDSRC/appmana-gradle-plugins/src/main/groovy"
	canonical := "/repository/buildSrc/appmana-gradle-plugins/src/main/groovy"

	if got := winFspDirectoryListingPath(requested, canonical, false); got != canonical {
		t.Fatalf("case-insensitive listing path = %q; want %q", got, canonical)
	}
	if got := winFspDirectoryListingPath(requested, canonical, true); got != requested {
		t.Fatalf("case-sensitive listing path = %q; want %q", got, requested)
	}
}

func TestWinFspMutationUsesExistingCanonicalName(t *testing.T) {
	if got, exists := winFspMutationName("caseprobe.tmp", "CaseProbe.tmp", true, false); got != "CaseProbe.tmp" || !exists {
		t.Fatalf("case-insensitive mutation = %q, %v; want CaseProbe.tmp, true", got, exists)
	}
	if got, exists := winFspMutationName("caseprobe.tmp", "CaseProbe.tmp", true, true); got != "caseprobe.tmp" || exists {
		t.Fatalf("case-sensitive mutation = %q, %v; want caseprobe.tmp, false", got, exists)
	}
	if got, exists := winFspMutationName("new.tmp", "", false, false); got != "new.tmp" || exists {
		t.Fatalf("new case-insensitive mutation = %q, %v; want new.tmp, false", got, exists)
	}
}
