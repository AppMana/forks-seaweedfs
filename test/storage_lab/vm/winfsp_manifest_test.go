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
		line = strings.TrimSuffix(line, "\r") // Set-Content emits CRLF on Windows.
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
	required := []string{"source_date_epoch", "build_script_sha256"}
	switch fields["build_toolchain"] {
	case "", "mingw": // Preserve manifests from the original recorded MinGW runs.
		required = append(required, "build_shim_sha256")
	case "msvc":
		if _, exists := fields["build_shim_sha256"]; exists {
			return fmt.Errorf("MSVC build must not claim a MinGW shim")
		}
		required = append(required, "platform_toolset", "vc_tools_version", "windows_sdk_version", "msbuild_version", "version_build_number", "version_copyright_year")
	default:
		return fmt.Errorf("unknown WinFsp build toolchain %q", fields["build_toolchain"])
	}
	for _, key := range required {
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
	msvc := strings.Replace(baseline, "build_shim_sha256=fixture\n", "", 1) + "build_toolchain=msvc\nplatform_toolset=v142\nvc_tools_version=14.29.30133\nwindows_sdk_version=10.0.19041.0\nmsbuild_version=16.11.2\nversion_build_number=25156\nversion_copyright_year=2025\n"
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
		{"explicit mingw", baseline + "build_toolchain=mingw\n", dll, nil, true},
		{"unknown toolchain", baseline + "build_toolchain=unknown\n", dll, nil, false},
		{"msvc baseline", msvc, dll, nil, true},
		{"msvc Windows CRLF", strings.ReplaceAll(msvc, "\n", "\r\n"), dll, nil, true},
		{"msvc Windows CRLF duplicate", strings.ReplaceAll(msvc+"source_mode=baseline\n", "\n", "\r\n"), dll, nil, false},
		{"msvc candidate", strings.Replace(strings.Replace(msvc, "source_mode=baseline", "source_mode=candidate", 1), fmt.Sprintf("source_patch_sha256=%x", sha256.Sum256(nil)), fmt.Sprintf("source_patch_sha256=%x", sha256.Sum256(patch)), 1), dll, patch, true},
		{"msvc missing toolset", strings.Replace(msvc, "platform_toolset=v142", "platform_toolset=", 1), dll, nil, false},
		{"msvc missing MSBuild", strings.Replace(msvc, "msbuild_version=16.11.2", "msbuild_version=", 1), dll, nil, false},
		{"msvc missing SDK", strings.Replace(msvc, "windows_sdk_version=10.0.19041.0", "windows_sdk_version=", 1), dll, nil, false},
		{"msvc missing compiler", strings.Replace(msvc, "vc_tools_version=14.29.30133", "vc_tools_version=", 1), dll, nil, false},
		{"msvc missing build number", strings.Replace(msvc, "version_build_number=25156", "version_build_number=", 1), dll, nil, false},
		{"msvc missing copyright year", strings.Replace(msvc, "version_copyright_year=2025", "version_copyright_year=", 1), dll, nil, false},
		{"msvc unexpected shim", msvc + "build_shim_sha256=fixture\n", dll, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateWinFspLabManifest([]byte(tc.manifest), tc.dll, tc.patch); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
