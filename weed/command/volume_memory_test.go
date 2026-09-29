package command

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/util/memlimit"
)

func withoutGOMEMLIMIT(t *testing.T) {
	t.Helper()
	prev, had := os.LookupEnv("GOMEMLIMIT")
	os.Unsetenv("GOMEMLIMIT")
	t.Cleanup(func() {
		if had {
			os.Setenv("GOMEMLIMIT", prev)
		}
	})
}

func cgroupRoot(t *testing.T, memoryMax string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "sys", "fs", "cgroup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(memoryMax), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestApplyMemoryLimitsDerivesFromCgroup(t *testing.T) {
	withoutGOMEMLIMIT(t)
	prevLimit := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prevLimit) })

	upload, download := memlimit.AutoMB, memlimit.AutoMB
	opts := VolumeServerOptions{concurrentUploadLimitMB: &upload, concurrentDownloadLimitMB: &download}
	opts.applyMemoryLimits(cgroupRoot(t, "5368709120\n"))

	// 5 GiB container: Go limit 4 GiB, budget 4 GiB - 1.25 GiB = 2816 MiB, split 3:1.
	if got := debug.SetMemoryLimit(-1); got != 4<<30 {
		t.Fatalf("Go memory limit = %d, want %d", got, int64(4<<30))
	}
	if upload != 2112 || download != 704 {
		t.Fatalf("admission = %d/%d MiB, want 2112/704", upload, download)
	}
}

func TestApplyMemoryLimitsKeepsExplicitSettings(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "3GiB")
	prevLimit := debug.SetMemoryLimit(3 << 30) // what the runtime read from the env at start
	t.Cleanup(func() { debug.SetMemoryLimit(prevLimit) })

	upload, download := 3072, 1024
	opts := VolumeServerOptions{concurrentUploadLimitMB: &upload, concurrentDownloadLimitMB: &download}
	opts.applyMemoryLimits(cgroupRoot(t, "5368709120\n"))

	if got := debug.SetMemoryLimit(-1); got != 3<<30 {
		t.Fatalf("Go memory limit = %d, want the explicit %d", got, int64(3<<30))
	}
	if upload != 3072 || download != 1024 {
		t.Fatalf("admission = %d/%d MiB, want the explicit 3072/1024", upload, download)
	}
}

func TestApplyMemoryLimitsUnlimitedContainer(t *testing.T) {
	withoutGOMEMLIMIT(t)
	prevLimit := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prevLimit) })

	upload, download := memlimit.AutoMB, memlimit.AutoMB
	opts := VolumeServerOptions{concurrentUploadLimitMB: &upload, concurrentDownloadLimitMB: &download}
	opts.applyMemoryLimits(cgroupRoot(t, "max\n"))

	if got := debug.SetMemoryLimit(-1); got != prevLimit {
		t.Fatalf("Go memory limit changed to %d without a container limit", got)
	}
	if upload != 0 || download != 0 {
		t.Fatalf("admission = %d/%d, want unlimited", upload, download)
	}
}
