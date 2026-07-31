package command

import (
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/filer"
)

// parseReaderCacheMode backs the fork-added -readerCacheMode weed mount flag.
// Its contract is to fail closed: anything that is not one of the three known
// modes must produce an error rather than silently degrading to "auto". A
// mount that quietly reverts to inferred classification after a StorageClass
// typo is the exact failure this flag exists to prevent, so the fails-closed
// behavior is load-bearing and not merely defensive.
func TestParseReaderCacheModeAcceptsKnownModes(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  filer.ReaderCacheMode
	}{
		{"auto", filer.ReaderCacheModeAuto},
		{"sequential", filer.ReaderCacheModeSequential},
		{"random", filer.ReaderCacheModeRandom},
	} {
		got, err := parseReaderCacheMode(tc.value)
		if err != nil {
			t.Fatalf("parseReaderCacheMode(%q) returned error %v; want %v", tc.value, err, tc.want)
		}
		if got != tc.want {
			t.Fatalf("parseReaderCacheMode(%q) = %q; want %q", tc.value, got, tc.want)
		}
	}
}

func TestParseReaderCacheModeFailsClosed(t *testing.T) {
	// The empty string is called out separately from an ordinary typo: a
	// caller that forgets to set the flag (or a StorageClass parameter that
	// resolves to "") must not be handed ReaderCacheModeAuto, because at the
	// filer layer NewReaderPatternWithMode *does* treat "" as auto. The flag
	// layer is where that ambiguity has to be rejected.
	for _, value := range []string{"", "Auto", "SEQUENTIAL", "seq", "randomm", "true", "0"} {
		got, err := parseReaderCacheMode(value)
		if err == nil {
			t.Fatalf("parseReaderCacheMode(%q) = %q, nil; want an error (must fail closed, not fall back to auto)", value, got)
		}
		if got != "" {
			t.Fatalf("parseReaderCacheMode(%q) returned mode %q alongside an error; want the empty mode", value, got)
		}
		if !strings.Contains(err.Error(), "readerCacheMode") {
			t.Fatalf("parseReaderCacheMode(%q) error %q does not name the flag; the message is the operator's only clue", value, err)
		}
	}
}

// The flag's registered default must itself be a value parseReaderCacheMode
// accepts. If a merge ever changes the default string without changing the
// parser (or vice versa), every mount would fail to start.
func TestReaderCacheModeFlagDefaultParses(t *testing.T) {
	f := cmdMount.Flag.Lookup("readerCacheMode")
	if f == nil {
		t.Fatal("weed mount no longer registers the fork-added -readerCacheMode flag")
	}
	if _, err := parseReaderCacheMode(f.DefValue); err != nil {
		t.Fatalf("registered -readerCacheMode default %q does not parse: %v", f.DefValue, err)
	}
	if f.DefValue != string(filer.ReaderCacheModeAuto) {
		t.Fatalf("-readerCacheMode default = %q; want %q so behavior is unchanged unless explicitly overridden", f.DefValue, filer.ReaderCacheModeAuto)
	}
}
