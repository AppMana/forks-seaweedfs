package framework

import (
	"context"
	"errors"
	"os"
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
	checkMaintenanceCLIExitStatus(t, func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, c.weedBinary, "-config_dir="+c.configDir, "shell", "-master="+c.MasterAddress())
		cmd.Dir = c.baseDir
		return cmd
	})
}

// Opt-in image qualification uses a network-disabled container: master and
// CLI share only its loopback, with no production routes or mounted credentials.
func TestMaintenanceCLIImage(t *testing.T) {
	image := os.Getenv("WEED_MAINTENANCE_IMAGE")
	if testing.Short() || image == "" {
		t.Skip("set WEED_MAINTENANCE_IMAGE to an already built local image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "run", "--pull=never", "--rm", "-d", "--network=none", "--read-only",
		"--tmpfs", "/data", "--tmpfs", "/tmp", image, "master", "-ip=127.0.0.1", "-mdir=/data").CombinedOutput()
	if err != nil {
		t.Fatalf("start isolated image: %v: %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "docker", "stop", "--time=5", id).CombinedOutput(); err != nil {
			t.Errorf("clean isolated image: %v: %s", err, out)
		}
	})
	checkMaintenanceCLIExitStatus(t, func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "docker", "exec", "-i", id, "/usr/bin/weed", "shell", "-master=127.0.0.1:9333")
	})
}

func checkMaintenanceCLIExitStatus(t *testing.T, command func(context.Context) *exec.Cmd) {
	t.Helper()
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
			cmd := command(ctx)
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
