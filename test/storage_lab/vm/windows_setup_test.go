package main

import (
	"fmt"
	"strings"
	"testing"
)

// A successful transport response is not evidence that this command ran.
// Require its run-specific terminal marker, not output from an earlier probe.
func validateWindowsCommandOutput(exitCode int32, stdout, marker string) error {
	if exitCode != 0 {
		return fmt.Errorf("Windows command exit %d", exitCode)
	}
	if marker != "" {
		for _, line := range strings.Split(stdout, "\n") {
			if strings.TrimSpace(line) == marker {
				return nil
			}
		}
	}
	return fmt.Errorf("Windows command missing its completion marker %q; command/result correlation is unverified", marker)
}

func TestWindowsSetupCompletion(t *testing.T) {
	const marker = "SETUP_COMPLETE:fixture-123"
	for _, tc := range []struct {
		name, output string
		code         int32
		valid        bool
	}{
		{"complete", "transcript\r\n" + marker + "\r\n", 0, true},
		{"stale readiness", "windows-ready\r\n", 0, false},
		{"other run", "SETUP_COMPLETE:fixture-122\r\n", 0, false},
		{"echoed command", "Write-Output '" + marker + "'", 0, false},
		{"nonzero", marker, 1, false},
		{"empty", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateWindowsCommandOutput(tc.code, tc.output, marker); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
