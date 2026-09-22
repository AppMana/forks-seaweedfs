package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// This validates evidence consistency, not trust in the compiler or source.
func validateWinFspLabManifest(manifest, dll, patch []byte) error {
	fields := map[string]string{}
	for _, line := range strings.Split(string(manifest), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if _, exists := fields[key]; exists {
			return fmt.Errorf("duplicate WinFsp manifest field %q", key)
		}
		fields[key] = value
	}
	if fields["dll_sha256"] != fmt.Sprintf("%x", sha256.Sum256(dll)) || fields["source_patch_sha256"] != fmt.Sprintf("%x", sha256.Sum256(patch)) {
		return fmt.Errorf("WinFsp DLL/source patch does not match build manifest")
	}
	if fields["source_revision"] != "ddca7bd5481857a65ba552f643b8776fd070836f" {
		return fmt.Errorf("unexpected WinFsp source revision")
	}
	switch fields["source_mode"] {
	case "baseline":
		if len(patch) != 0 {
			return fmt.Errorf("WinFsp baseline has source changes")
		}
	case "candidate":
		if len(patch) == 0 {
			return fmt.Errorf("WinFsp candidate has no recorded patch")
		}
	default:
		return fmt.Errorf("missing/invalid WinFsp source mode")
	}
	for _, key := range []string{"source_date_epoch", "build_script_sha256", "build_shim_sha256"} {
		if fields[key] == "" {
			return fmt.Errorf("missing WinFsp build evidence %q", key)
		}
	}
	return nil
}

func TestWinFspLabManifest(t *testing.T) {
	dll := []byte("test DLL bytes")
	baseline := fmt.Sprintf("source_revision=ddca7bd5481857a65ba552f643b8776fd070836f\nsource_mode=baseline\ndll_sha256=%x\nsource_patch_sha256=%x\nsource_date_epoch=1\nbuild_script_sha256=fixture\nbuild_shim_sha256=fixture\n", sha256.Sum256(dll), sha256.Sum256(nil))
	patch := []byte("tracked source patch")
	withPatch := strings.Replace(baseline, fmt.Sprintf("source_patch_sha256=%x", sha256.Sum256(nil)), fmt.Sprintf("source_patch_sha256=%x", sha256.Sum256(patch)), 1)
	for _, tc := range []struct {
		name, manifest string
		dll, patch     []byte
		valid          bool
	}{
		{"baseline", baseline, dll, nil, true},
		{"candidate", strings.Replace(withPatch, "source_mode=baseline", "source_mode=candidate", 1), dll, patch, true},
		{"patched baseline", withPatch, dll, patch, false},
		{"missing", "", dll, nil, false},
		{"wrong DLL", baseline, []byte("other"), nil, false},
		{"wrong patch", baseline, dll, []byte("patch"), false},
		{"duplicate", baseline + "source_mode=baseline\n", dll, nil, false},
		{"empty candidate", strings.Replace(baseline, "source_mode=baseline", "source_mode=candidate", 1), dll, nil, false},
		{"missing script", strings.Replace(baseline, "build_script_sha256=fixture", "build_script_sha256=", 1), dll, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateWinFspLabManifest([]byte(tc.manifest), tc.dll, tc.patch); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
