// Package memlimit derives a process's Go soft memory limit and volume-server
// transfer admission budgets from the effective cgroup memory limit (capped at
// physical RAM).
package memlimit

import "math"

const (
	// cgroup v1 reports "no limit" as the largest page-aligned int64,
	// 9223372036854771712; anything this large is not a real limit.
	cgroupV1Unlimited = int64(1) << 62

	// The Go runtime gets 90% of available memory as its soft memory limit,
	// leaving 10% headroom for non-Go cgroup charges (kernel slab, page tables,
	// socket buffers) and GC overshoot.
	GoMemLimitNumerator   = 9
	GoMemLimitDenominator = 10

	// VolumeBaseHeapBytes is the baseline heap reserved for a volume server
	// with no transfers in flight (needle maps, volume/EC metadata, caches).
	VolumeBaseHeapBytes = int64(5) << 28 // 1.25 GiB

	// AutoMB is the admission flag value meaning "derive from the memory
	// limit". 0 keeps its existing meaning, unlimited.
	AutoMB = -1
)

// GoMemLimitFor returns the Go soft memory limit for the given available
// memory: GoMemLimitNumerator/GoMemLimitDenominator of it, computed exactly.
func GoMemLimitFor(availableBytes int64) int64 {
	return availableBytes/GoMemLimitDenominator*GoMemLimitNumerator +
		availableBytes%GoMemLimitDenominator*GoMemLimitNumerator/GoMemLimitDenominator
}

// VolumeMemoryInput is everything PlanVolumeMemory depends on.
type VolumeMemoryInput struct {
	// AvailableBytes is the memory the process may use (see AvailableMemory):
	// the effective cgroup limit capped at physical RAM, or physical RAM.
	// 0 means unknown.
	AvailableBytes int64
	// EnvGOMEMLIMIT reports that GOMEMLIMIT is set in the environment; an
	// operator's explicit value is never overridden.
	EnvGOMEMLIMIT bool
	// RuntimeLimit is the Go runtime's current memory limit,
	// debug.SetMemoryLimit(-1); math.MaxInt64 means none.
	RuntimeLimit int64
	// UploadMB and DownloadMB are the -concurrentUploadLimitMB and
	// -concurrentDownloadLimitMB flags: AutoMB derives, 0 is unlimited, and
	// any positive value is used as given.
	UploadMB   int
	DownloadMB int
}

// VolumeMemoryPlan is the memory configuration a volume server applies.
type VolumeMemoryPlan struct {
	// GoMemLimit is the Go soft memory limit in effect after the plan is
	// applied; math.MaxInt64 means none.
	GoMemLimit int64
	// SetGoMemLimit reports that GoMemLimit must be applied with
	// debug.SetMemoryLimit.
	SetGoMemLimit bool
	// UploadLimitBytes and DownloadLimitBytes are the transfer admission
	// budgets; 0 is unlimited.
	UploadLimitBytes   int64
	DownloadLimitBytes int64
	// AutoUpload and AutoDownload report which budgets were derived.
	AutoUpload   bool
	AutoDownload bool
}

// PlanVolumeMemory derives the Go memory limit and the transfer admission
// budgets.
//
// Go limit: an explicit GOMEMLIMIT wins; otherwise GoMemLimitFor of the
// available memory (or none when unknown).
//
// Admission (for flags left at AutoMB):
//
//	budget   = max(goLimit - VolumeBaseHeapBytes, goLimit / 4)
//	upload   = budget * 3 / 4
//	download = budget - upload
func PlanVolumeMemory(in VolumeMemoryInput) VolumeMemoryPlan {
	plan := VolumeMemoryPlan{GoMemLimit: in.RuntimeLimit}
	if !in.EnvGOMEMLIMIT && in.AvailableBytes > 0 {
		plan.GoMemLimit = GoMemLimitFor(in.AvailableBytes)
		plan.SetGoMemLimit = true
	}

	var upload, download int64
	if plan.GoMemLimit > 0 && plan.GoMemLimit != math.MaxInt64 {
		budget := plan.GoMemLimit - VolumeBaseHeapBytes
		if floor := plan.GoMemLimit / 4; budget < floor {
			budget = floor
		}
		upload = budget * 3 / 4
		download = budget - upload
	}

	if in.UploadMB == AutoMB {
		plan.UploadLimitBytes = upload
		plan.AutoUpload = true
	} else {
		plan.UploadLimitBytes = int64(in.UploadMB) << 20
	}
	if in.DownloadMB == AutoMB {
		plan.DownloadLimitBytes = download
		plan.AutoDownload = true
	} else {
		plan.DownloadLimitBytes = int64(in.DownloadMB) << 20
	}
	return plan
}
