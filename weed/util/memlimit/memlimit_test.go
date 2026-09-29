package memlimit

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

const gib = int64(1) << 30

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCgroupMemoryLimit(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		wantLimit int64
		wantOK    bool
		wantErr   bool
	}{
		{name: "v2 numeric", files: map[string]string{cgroupV2MemoryMax: "5368709120\n"}, wantLimit: 5 * gib, wantOK: true},
		{name: "v2 max", files: map[string]string{cgroupV2MemoryMax: "max\n"}, wantOK: false},
		{name: "v2 takes precedence over v1", files: map[string]string{cgroupV2MemoryMax: "4294967296\n", cgroupV1MemoryLimit: "8589934592\n"}, wantLimit: 4 * gib, wantOK: true},
		{name: "v1 numeric", files: map[string]string{cgroupV1MemoryLimit: "5368709120\n"}, wantLimit: 5 * gib, wantOK: true},
		{name: "v1 unlimited sentinel", files: map[string]string{cgroupV1MemoryLimit: "9223372036854771712\n"}, wantOK: false},
		{name: "missing", files: nil, wantOK: false},
		{name: "v2 garbage", files: map[string]string{cgroupV2MemoryMax: "lots\n"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for rel, content := range tc.files {
				writeFile(t, root, rel, content)
			}
			limit, ok, err := CgroupMemoryLimit(root)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got limit=%d ok=%v", limit, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK || (ok && limit != tc.wantLimit) {
				t.Fatalf("got limit=%d ok=%v, want limit=%d ok=%v", limit, ok, tc.wantLimit, tc.wantOK)
			}
		})
	}
}

func TestPlanVolumeMemoryFromCgroup(t *testing.T) {
	// The production DAS volume container: 5 GiB limit, no GOMEMLIMIT, auto admission.
	plan := PlanVolumeMemory(VolumeMemoryInput{
		CgroupLimit:    5 * gib,
		HasCgroupLimit: true,
		RuntimeLimit:   math.MaxInt64,
		UploadMB:       AutoMB,
		DownloadMB:     AutoMB,
	})
	limit := 5 * gib
	wantGo := int64(float64(limit) * GoMemLimitRatio)
	if !plan.SetGoMemLimit || plan.GoMemLimit != wantGo {
		t.Fatalf("GoMemLimit = %d set=%v, want %d set", plan.GoMemLimit, plan.SetGoMemLimit, wantGo)
	}
	budget := wantGo - VolumeBaseHeapBytes
	if got := plan.UploadLimitBytes + plan.DownloadLimitBytes; got > budget {
		t.Fatalf("admission %d exceeds Go limit minus base heap %d", got, budget)
	}
	if plan.UploadLimitBytes != budget*3/4 || plan.DownloadLimitBytes != budget-budget*3/4 {
		t.Fatalf("upload/download = %d/%d, want 3:1 split of %d", plan.UploadLimitBytes, plan.DownloadLimitBytes, budget)
	}
	if plan.UploadLimitBytes+plan.DownloadLimitBytes+VolumeBaseHeapBytes > 5*gib {
		t.Fatal("admitted bytes plus base heap exceed the container limit")
	}
}

func TestPlanVolumeMemoryRespectsGOMEMLIMITEnv(t *testing.T) {
	plan := PlanVolumeMemory(VolumeMemoryInput{
		CgroupLimit:    5 * gib,
		HasCgroupLimit: true,
		EnvGOMEMLIMIT:  true,
		RuntimeLimit:   3 * gib,
		UploadMB:       AutoMB,
		DownloadMB:     AutoMB,
	})
	if plan.SetGoMemLimit {
		t.Fatal("must not override an explicit GOMEMLIMIT")
	}
	if plan.GoMemLimit != 3*gib {
		t.Fatalf("GoMemLimit = %d, want the runtime's %d", plan.GoMemLimit, 3*gib)
	}
	budget := 3*gib - VolumeBaseHeapBytes
	if plan.UploadLimitBytes != budget*3/4 || plan.DownloadLimitBytes != budget-budget*3/4 {
		t.Fatalf("upload/download = %d/%d, want 3:1 split of %d", plan.UploadLimitBytes, plan.DownloadLimitBytes, budget)
	}
}

func TestPlanVolumeMemoryExplicitFlagsOverride(t *testing.T) {
	plan := PlanVolumeMemory(VolumeMemoryInput{
		CgroupLimit:    5 * gib,
		HasCgroupLimit: true,
		RuntimeLimit:   math.MaxInt64,
		UploadMB:       3072,
		DownloadMB:     0,
	})
	if plan.UploadLimitBytes != 3072<<20 {
		t.Fatalf("upload = %d, want the explicit 3072 MB", plan.UploadLimitBytes)
	}
	if plan.DownloadLimitBytes != 0 {
		t.Fatalf("download = %d, want explicit 0 (unlimited)", plan.DownloadLimitBytes)
	}
	if !plan.SetGoMemLimit {
		t.Fatal("explicit admission flags do not stop deriving the Go limit")
	}
}

func TestPlanVolumeMemoryWithoutAnyLimit(t *testing.T) {
	plan := PlanVolumeMemory(VolumeMemoryInput{
		RuntimeLimit: math.MaxInt64,
		UploadMB:     AutoMB,
		DownloadMB:   AutoMB,
	})
	if plan.SetGoMemLimit {
		t.Fatal("no cgroup limit: leave the Go limit alone")
	}
	if plan.UploadLimitBytes != 0 || plan.DownloadLimitBytes != 0 {
		t.Fatalf("no memory limit anywhere: admission must stay unlimited, got %d/%d", plan.UploadLimitBytes, plan.DownloadLimitBytes)
	}
}

func TestPlanVolumeMemorySmallContainerKeepsAQuarter(t *testing.T) {
	plan := PlanVolumeMemory(VolumeMemoryInput{
		CgroupLimit:    1 * gib,
		HasCgroupLimit: true,
		RuntimeLimit:   math.MaxInt64,
		UploadMB:       AutoMB,
		DownloadMB:     AutoMB,
	})
	limit := gib
	goLimit := int64(float64(limit) * GoMemLimitRatio)
	budget := goLimit / 4
	if plan.UploadLimitBytes+plan.DownloadLimitBytes != budget {
		t.Fatalf("admission = %d, want the %d floor when the base heap does not fit", plan.UploadLimitBytes+plan.DownloadLimitBytes, budget)
	}
}
