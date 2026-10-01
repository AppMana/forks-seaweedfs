package framework

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/test/volume_server/matrix"
)

// Exercise the actual binary, stdin EOF, shell loop and process exit status.
// The existing harness starts private loopback servers and cleans them up.
func TestMaintenanceCLIExitStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("starts isolated master and volume binaries")
	}
	c := StartSingleVolumeCluster(t, matrix.P1())
	for _, tc := range []struct {
		name, input, wantOutput string
		wantExit                int
	}{
		{"check-invalid", "volume.check.disk -not-a-real-flag\n", "flag provided but not defined", 1},
		{"vacuum-invalid", "volume.vacuum -garbageThreshold=invalid\n", "invalid", 1},
		{"delete-empty-invalid", "volume.deleteEmpty -quietFor=invalid\n", "invalid", 1},
		{"balance-invalid", "volume.balance -not-a-real-flag\n", "flag provided but not defined", 1},
		{"failure-survives-success", "volume.check.disk -not-a-real-flag; help\n", "flag provided but not defined", 1},
		{"help", "volume.check.disk -h\n", "Usage", 0},
		{"readonly-check-eof", "lock\nvolume.check.disk -slow -v\nunlock\n", "Pass #1", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, c.weedBinary, "-config_dir="+c.configDir, "shell", "-master="+c.MasterAddress())
			cmd.Dir = c.baseDir
			cmd.Stdin = strings.NewReader(tc.input)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("CLI did not finish at stdin EOF: %v\n%s", ctx.Err(), out)
			}
			exit := 0
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatal(err)
				}
				exit = ee.ExitCode()
			}
			if exit != tc.wantExit || !strings.Contains(string(out), tc.wantOutput) {
				t.Fatalf("exit=%d want=%d output must contain %q:\n%s", exit, tc.wantExit, tc.wantOutput, out)
			}
		})
	}
}
