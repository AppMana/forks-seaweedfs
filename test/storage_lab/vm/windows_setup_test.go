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

func validateWindowsScenarioEvidence(scenario, output string) error {
	var required []string
	switch scenario {
	case "GitAtomicRenamePrimed":
		required = []string{"PASS: git init iteration 20 leaves no stale config.lock"}
	case "NativeMetadata":
		required = []string{"PASS: native metadata regressions completes without skips"}
	case "AccessPerformance":
		required = []string{"PASS: native access performance completes without skips", "--- PASS: TestWindowsAccessPerformance "}
	case "Conformance":
		required = []string{"PASS: upstream conformance including known failures"}
	case "All":
		required = []string{"PASS: git init iteration 20 leaves no stale config.lock", "PASS: native mounted suite completes without skips", "PASS: native persistence write completes without skips", "PASS: native persistence verify completes without skips"}
	case "GitLfsTempMetadata":
		required = []string{"PASS: Git LFS status iteration 20 reports all 32 modified assets", "PASS: Git LFS filter is active", "PASS: Git LFS seed commit succeeds"}
	default:
		return fmt.Errorf("unknown Windows workload scenario %q", scenario)
	}
	for _, marker := range required {
		if !strings.Contains(output, marker) {
			return fmt.Errorf("%s missing evidence: %s", scenario, marker)
		}
	}
	return nil
}

func TestWindowsScenarioEvidence(t *testing.T) {
	const perf = "--- PASS: TestWindowsAccessPerformance (5.33s)\r\nPASS: native access performance completes without skips\r\n"
	for _, tc := range []struct {
		name, scenario, output string
		valid                  bool
	}{
		{"real performance marker", "AccessPerformance", perf, true},
		{"empty performance", "AccessPerformance", "", false},
		{"wrong Git marker", "AccessPerformance", "PASS: git init iteration 20 leaves no stale config.lock", false},
		{"missing native PASS", "AccessPerformance", "PASS: native access performance completes without skips", false},
		{"missing harness marker", "AccessPerformance", "--- PASS: TestWindowsAccessPerformance (5.33s)", false},
		{"Git cannot use perf marker", "GitAtomicRenamePrimed", perf, false},
		{"LFS missing prerequisites", "GitLfsTempMetadata", "PASS: Git LFS status iteration 20 reports all 32 modified assets", false},
		{"All missing remount", "All", "PASS: git init iteration 20 leaves no stale config.lock\nPASS: native mounted suite completes without skips", false},
		{"unknown scenario", "NewScenario", perf, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateWindowsScenarioEvidence(tc.scenario, tc.output); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
