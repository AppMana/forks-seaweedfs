package shell

import (
	"io"
	"testing"
)

func TestMaintenanceCommandsRejectMalformedArguments(t *testing.T) {
	commands := []struct {
		name     string
		do       func([]string, *CommandEnv, io.Writer) error
		badValue string
	}{
		{"volume.check.disk", (&commandVolumeCheckDisk{}).Do, "-volumeId=not-a-number"},
		{"volume.fsck", (&commandVolumeFsck{}).Do, "-cutoffTimeAgo=not-a-duration"},
		{"volume.vacuum", (&commandVacuum{}).Do, "-garbageThreshold=not-a-number"},
		{"volume.deleteEmpty", (&commandVolumeDeleteEmpty{}).Do, "-quietFor=not-a-duration"},
		{"volume.balance", (&commandVolumeBalance{}).Do, "-volumeBy=INVALID"},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			for _, argument := range []string{"-unknown-maintenance-flag", command.badValue} {
				// nil environment also proves malformed input never reaches a
				// master connection, admin lock, or mutating operation.
				if err := command.do([]string{argument}, nil, io.Discard); err == nil {
					t.Errorf("%s reported success for %q", command.name, argument)
				}
			}
			if err := command.do([]string{"-h"}, nil, io.Discard); err != nil {
				t.Errorf("help should remain successful: %v", err)
			}
		})
	}
}
