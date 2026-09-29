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

// cgroupRoot lays out a filesystem root with a private cgroup v2 namespace,
// the given memory.max and MemTotal.
func cgroupRoot(t *testing.T, memoryMax string, memTotalKiB string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range map[string]string{
		"proc/self/cgroup":         "0::/\n",
		"proc/self/mountinfo":      "30 24 0:26 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup2 rw\n",
		"proc/meminfo":             "MemTotal:       " + memTotalKiB + " kB\n",
		"sys/fs/cgroup/memory.max": memoryMax,
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func keepRuntimeLimit(t *testing.T) int64 {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	return prev
}

func TestApplyMemoryLimitsDerivesFromCgroup(t *testing.T) {
	withoutGOMEMLIMIT(t)
	keepRuntimeLimit(t)

	upload, download := memlimit.AutoMB, memlimit.AutoMB
	opts := VolumeServerOptions{concurrentUploadLimitMB: &upload, concurrentDownloadLimitMB: &download}
	opts.applyMemoryLimits(cgroupRoot(t, "5368709120\n", "32000000"))

	// 5 GiB available: Go limit 4 GiB, budget 4 GiB - 1.25 GiB = 2816 MiB, split 3:1.
	if got := debug.SetMemoryLimit(-1); got != 4<<30 {
		t.Fatalf("Go memory limit = %d, want %d", got, int64(4<<30))
	}
	if upload != 2112 || download != 704 {
		t.Fatalf("admission = %d/%d MiB, want 2112/704", upload, download)
	}
}

// The limit on a parent slice of a cgroup v1 hierarchy governs a service whose
// own cgroup is unlimited; the root of the hierarchy reports no limit.
func TestApplyMemoryLimitsV1ParentSlice(t *testing.T) {
	withoutGOMEMLIMIT(t)
	keepRuntimeLimit(t)

	upload, download := memlimit.AutoMB, memlimit.AutoMB
	opts := VolumeServerOptions{concurrentUploadLimitMB: &upload, concurrentDownloadLimitMB: &download}
	opts.applyMemoryLimits("../util/memlimit/testdata/synology")

	if got := debug.SetMemoryLimit(-1); got != 4<<30 {
		t.Fatalf("Go memory limit = %d, want %d", got, int64(4<<30))
	}
	if upload != 2112 || download != 704 {
		t.Fatalf("admission = %d/%d MiB, want 2112/704", upload, download)
	}
}

func TestApplyMemoryLimitsKeepsExplicitSettings(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "3GiB")
	keepRuntimeLimit(t)
	debug.SetMemoryLimit(3 << 30) // what the runtime read from the env at start

	upload, download := 3072, 1024
	opts := VolumeServerOptions{concurrentUploadLimitMB: &upload, concurrentDownloadLimitMB: &download}
	opts.applyMemoryLimits(cgroupRoot(t, "5368709120\n", "32000000"))

	if got := debug.SetMemoryLimit(-1); got != 3<<30 {
		t.Fatalf("Go memory limit = %d, want the explicit %d", got, int64(3<<30))
	}
	if upload != 3072 || download != 1024 {
		t.Fatalf("admission = %d/%d MiB, want the explicit 3072/1024", upload, download)
	}
}

// Without a cgroup limit the physical RAM is what is available.
func TestApplyMemoryLimitsUnlimitedCgroupUsesPhysicalRAM(t *testing.T) {
	withoutGOMEMLIMIT(t)
	keepRuntimeLimit(t)

	upload, download := memlimit.AutoMB, memlimit.AutoMB
	opts := VolumeServerOptions{concurrentUploadLimitMB: &upload, concurrentDownloadLimitMB: &download}
	opts.applyMemoryLimits(cgroupRoot(t, "max\n", "5242880")) // 5 GiB of RAM

	if got := debug.SetMemoryLimit(-1); got != 4<<30 {
		t.Fatalf("Go memory limit = %d, want %d", got, int64(4<<30))
	}
	if upload != 2112 || download != 704 {
		t.Fatalf("admission = %d/%d MiB, want 2112/704", upload, download)
	}
}

// Nothing readable: leave the runtime and admission unlimited.
func TestApplyMemoryLimitsUnknownMemory(t *testing.T) {
	withoutGOMEMLIMIT(t)
	prev := keepRuntimeLimit(t)

	upload, download := memlimit.AutoMB, memlimit.AutoMB
	opts := VolumeServerOptions{concurrentUploadLimitMB: &upload, concurrentDownloadLimitMB: &download}
	opts.applyMemoryLimits(t.TempDir())

	if got := debug.SetMemoryLimit(-1); got != prev {
		t.Fatalf("Go memory limit changed to %d with no memory information", got)
	}
	if upload != 0 || download != 0 {
		t.Fatalf("admission = %d/%d, want unlimited", upload, download)
	}
}
