package memlimit

import (
	"math"
	"testing"
)

const gib = int64(1) << 30

func TestPlanVolumeMemoryFromCgroup(t *testing.T) {
	// The production DAS volume container: 5 GiB limit, no GOMEMLIMIT, auto admission.
	plan := PlanVolumeMemory(VolumeMemoryInput{
		AvailableBytes: 5 * gib,
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
		AvailableBytes: 5 * gib,
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
		AvailableBytes: 5 * gib,
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

func TestPlanVolumeMemoryUnknownAvailable(t *testing.T) {
	plan := PlanVolumeMemory(VolumeMemoryInput{
		RuntimeLimit: math.MaxInt64,
		UploadMB:     AutoMB,
		DownloadMB:   AutoMB,
	})
	if plan.SetGoMemLimit {
		t.Fatal("available memory unknown: leave the Go limit alone")
	}
	if plan.UploadLimitBytes != 0 || plan.DownloadLimitBytes != 0 {
		t.Fatalf("available memory unknown: admission must stay unlimited, got %d/%d", plan.UploadLimitBytes, plan.DownloadLimitBytes)
	}
}

func TestPlanVolumeMemorySmallContainerKeepsAQuarter(t *testing.T) {
	plan := PlanVolumeMemory(VolumeMemoryInput{
		AvailableBytes: 1 * gib,
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
