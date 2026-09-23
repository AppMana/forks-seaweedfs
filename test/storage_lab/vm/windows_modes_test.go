package main

import (
	"fmt"
	"testing"
)

func validateWindowsMountModes(isolate bool, registration, junction, dll string) error {
	if registration != "" && (registration != "1" || !isolate) {
		return fmt.Errorf("SEAWEEDFS_WINDOWS_MOUNT_MANAGER_FROM_FSD=1 requires the isolated mount-manager scenario")
	}
	if junction != "" && ((junction != "1" && junction != "nt-control") || !isolate || registration != "") {
		return fmt.Errorf("junction experiments require isolated default registration and mode 1 or nt-control")
	}
	if dll != "" && junction != "" {
		return fmt.Errorf("candidate DLL qualification cannot use a test-side junction rewrite")
	}
	return nil
}

func TestWindowsMountModeIsolation(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		isolate                     bool
		registration, junction, dll string
		valid                       bool
	}{
		{"default workload", false, "", "", "", true},
		{"candidate workload", false, "", "", "candidate.dll", true},
		{"candidate native", true, "", "", "candidate.dll", true},
		{"candidate FSD native", true, "1", "", "candidate.dll", true},
		{"FSD workload rejected", false, "1", "", "candidate.dll", false},
		{"invalid registration", true, "2", "", "", false},
		{"GUID experiment", true, "", "1", "", true},
		{"NT control", true, "", "nt-control", "", true},
		{"mixed registration experiment", true, "1", "1", "", false},
		{"candidate GUID intervention", true, "", "1", "candidate.dll", false},
		{"candidate NT intervention", true, "", "nt-control", "candidate.dll", false},
		{"workload intervention", false, "", "1", "", false},
		{"unknown intervention", true, "", "unknown", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateWindowsMountModes(tc.isolate, tc.registration, tc.junction, tc.dll); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
