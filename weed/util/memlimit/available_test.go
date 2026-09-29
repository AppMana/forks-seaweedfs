package memlimit

import (
	"os"
	"path/filepath"
	"testing"
)

const kib = int64(1024)

// fsTree writes files (path relative to the fixture root -> content).
func fsTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
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

func checkAvailable(t *testing.T, root string, wantBytes, wantLimit, wantPhysical int64, wantDir string) {
	t.Helper()
	got, err := AvailableMemory(root)
	if err != nil {
		t.Fatalf("AvailableMemory: %v", err)
	}
	if got.Bytes != wantBytes || got.CgroupLimit != wantLimit || got.PhysicalBytes != wantPhysical || got.CgroupDir != wantDir {
		t.Fatalf("got %+v, want Bytes=%d CgroupLimit=%d PhysicalBytes=%d CgroupDir=%q", got, wantBytes, wantLimit, wantPhysical, wantDir)
	}
}

const v2Mountinfo = "30 24 0:26 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:4 - cgroup2 cgroup2 rw,nsdelegate\n"

// A NAS package under systemd on a cgroup v1-only Linux 4.4 kernel: the limit
// is on the parent slice, the hierarchy root reports the unlimited sentinel.
func TestAvailableMemoryV1SliceOnOldKernel(t *testing.T) {
	checkAvailable(t, "testdata/synology", 5*gib, 5*gib, 65822536*kib, "/sys/fs/cgroup/memory/seaweedfs.slice")
}

// A Kubernetes container with a private cgroup v2 namespace ("0::/").
func TestAvailableMemoryV2PrivateNamespace(t *testing.T) {
	checkAvailable(t, "testdata/k8s-volume", 7*gib, 7*gib, 15479652*kib, "/sys/fs/cgroup")
}

// cgroup v2 in the host namespace: the process's path is the full path under
// a mount rooted at "/", and a parent (the pod) may carry the tighter limit.
func TestAvailableMemoryV2HostNamespace(t *testing.T) {
	const pod = "/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1.slice"
	root := fsTree(t, map[string]string{
		"proc/self/cgroup":                           "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1.slice/cri-containerd-abc.scope\n",
		"proc/self/mountinfo":                        v2Mountinfo,
		"proc/meminfo":                               "MemTotal:       32000000 kB\n",
		pod + "/memory.max":                          "6442450944\n",
		pod + "/cri-containerd-abc.scope/memory.max": "max\n",
		"sys/fs/cgroup/kubepods.slice/memory.max":    "30000000000\n",
	})
	checkAvailable(t, root, 6*gib, 6*gib, 32000000*kib, pod+"/cri-containerd-abc.scope")
}

// cgroup v1 with the container's own subtree bind-mounted: the mount root is
// stripped from the process's path before joining the mountpoint.
func TestAvailableMemoryV1MountRootStripped(t *testing.T) {
	root := fsTree(t, map[string]string{
		"proc/self/cgroup": "11:memory:/docker/abc/sub\n10:cpu,cpuacct:/docker/abc\n",
		"proc/self/mountinfo": "40 30 0:40 /docker/abc /sys/fs/cgroup/memory ro,nosuid,nodev,noexec,relatime - cgroup cgroup rw,memory\n" +
			"41 30 0:41 /docker/abc /sys/fs/cgroup/cpu,cpuacct ro,nosuid,nodev,noexec,relatime - cgroup cgroup rw,cpu,cpuacct\n",
		"proc/meminfo": "MemTotal:       16000000 kB\n",
		"sys/fs/cgroup/memory/memory.limit_in_bytes":     "3221225472\n",
		"sys/fs/cgroup/memory/sub/memory.limit_in_bytes": "9223372036854771712\n",
	})
	checkAvailable(t, root, 3*gib, 3*gib, 16000000*kib, "/sys/fs/cgroup/memory/sub")
}

// A combined v1 controller list ("memory,cpu") still names the memory cgroup.
func TestAvailableMemoryV1CombinedControllers(t *testing.T) {
	root := fsTree(t, map[string]string{
		"proc/self/cgroup":    "4:cpu,memory:/svc\n",
		"proc/self/mountinfo": "40 30 0:40 / /sys/fs/cgroup/cpu,memory rw - cgroup cgroup rw,cpu,memory\n",
		"proc/meminfo":        "MemTotal:       16000000 kB\n",
		"sys/fs/cgroup/cpu,memory/svc/memory.limit_in_bytes": "2147483648\n",
	})
	checkAvailable(t, root, 2*gib, 2*gib, 16000000*kib, "/sys/fs/cgroup/cpu,memory/svc")
}

// A limit above physical RAM is not memory the process can have.
func TestAvailableMemoryLimitAbovePhysical(t *testing.T) {
	root := fsTree(t, map[string]string{
		"proc/self/cgroup":                     "0::/\n",
		"proc/self/mountinfo":                  v2Mountinfo,
		"proc/meminfo":                         "MemTotal:        4000000 kB\n",
		"sys/fs/cgroup/memory.max":             "17179869184\n",
		"sys/fs/cgroup/memory.current":         "0\n",
		"sys/fs/cgroup/cgroup.controllers":     "memory\n",
		"sys/fs/cgroup/cgroup.subtree_control": "",
	})
	checkAvailable(t, root, 4000000*kib, 16*gib, 4000000*kib, "/sys/fs/cgroup")
}

// No limit anywhere in the hierarchy: physical RAM.
func TestAvailableMemoryNoLimitIsPhysical(t *testing.T) {
	root := fsTree(t, map[string]string{
		"proc/self/cgroup":    "0::/system.slice/weed.service\n",
		"proc/self/mountinfo": v2Mountinfo,
		"proc/meminfo":        "MemTotal:       64000000 kB\n",
		"sys/fs/cgroup/system.slice/weed.service/memory.max": "max\n",
		"sys/fs/cgroup/system.slice/memory.max":              "max\n",
	})
	checkAvailable(t, root, 64000000*kib, 0, 64000000*kib, "/sys/fs/cgroup/system.slice/weed.service")
}

// A hybrid host mounts v1 controllers and a v2 unified tree; the v1 memory
// controller holds the limit.
func TestAvailableMemoryHybridPrefersV1Memory(t *testing.T) {
	root := fsTree(t, map[string]string{
		"proc/self/cgroup": "5:memory:/system.slice/weed.service\n0::/system.slice/weed.service\n",
		"proc/self/mountinfo": "40 30 0:40 / /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory\n" +
			"41 30 0:41 / /sys/fs/cgroup/unified rw - cgroup2 cgroup2 rw,nsdelegate\n",
		"proc/meminfo": "MemTotal:       64000000 kB\n",
		"sys/fs/cgroup/memory/system.slice/weed.service/memory.limit_in_bytes": "4294967296\n",
		"sys/fs/cgroup/unified/system.slice/weed.service/memory.max":           "max\n",
	})
	checkAvailable(t, root, 4*gib, 4*gib, 64000000*kib, "/sys/fs/cgroup/memory/system.slice/weed.service")
}

// Without /proc/self/cgroup (not Linux, or a sandbox) only MemTotal applies,
// and without /proc/meminfo nothing is known.
func TestAvailableMemoryWithoutCgroupFiles(t *testing.T) {
	root := fsTree(t, map[string]string{"proc/meminfo": "MemTotal:        8000000 kB\n"})
	checkAvailable(t, root, 8000000*kib, 0, 8000000*kib, "")
	checkAvailable(t, t.TempDir(), 0, 0, 0, "")
}

func TestAvailableMemoryRejectsGarbageLimit(t *testing.T) {
	root := fsTree(t, map[string]string{
		"proc/self/cgroup":         "0::/\n",
		"proc/self/mountinfo":      v2Mountinfo,
		"proc/meminfo":             "MemTotal:        8000000 kB\n",
		"sys/fs/cgroup/memory.max": "lots\n",
	})
	if _, err := AvailableMemory(root); err == nil {
		t.Fatal("want a parse error")
	}
}

func TestUnescapeMountPath(t *testing.T) {
	if got := unescapeMountPath(`/mnt/with\040space`); got != "/mnt/with space" {
		t.Fatalf("got %q", got)
	}
}
