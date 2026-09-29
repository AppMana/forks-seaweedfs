// Package memlimit derives a process's memory settings from the memory
// available to it, determined the way the JVM does (the cgroup limit that
// applies to the process, capped at physical RAM): set only the container or
// slice memory limit and the Go memory limit and the volume server's transfer
// admission budgets follow from it.
//
// Go does not do this by itself: it reads the cgroup CPU quota for GOMAXPROCS
// but never the memory limit, so without this a container's GOMEMLIMIT and
// admission flags have to be kept consistent with its limit by hand.
package memlimit

import "math"

const (
	// cgroup v1 reports "no limit" as the largest page-aligned int64,
	// 9223372036854771712; anything this large is not a real limit.
	cgroupV1Unlimited = int64(1) << 62

	// The Go runtime gets GoMemLimitNumerator/GoMemLimitDenominator (90%) of
	// the available memory as its soft memory limit. The Go limit already
	// covers the heap, goroutine stacks and runtime metadata; the remaining
	// 10% is for what the cgroup charges outside the Go runtime: kernel memory
	// (slab, page tables; about 65 MiB measured on a volume server) and socket
	// buffers, which grow with thousands of queued connections, plus the
	// collector's overshoot when live data sits near a soft limit. Page cache
	// needs no reserve: the kernel reclaims it before invoking the OOM killer.
	// Integer arithmetic keeps the limit exact.
	GoMemLimitNumerator   = 9
	GoMemLimitDenominator = 10

	// VolumeBaseHeapBytes is the heap a volume server holds with no transfer
	// in flight: leveldb needle-map state, volume and EC metadata, caches.
	// Measured on the DAS volume servers (315-403 volumes each, -index=leveldb)
	// on 2026-09-29 07:30-07:36 UTC: heap in use 0.85-1.21 GiB while admitted
	// uploads were at most 96 MiB, i.e. at most 1.13 GiB without transfers.
	// 1.25 GiB covers that with margin.
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
// available memory, which is the effective cgroup limit capped at physical RAM,
// or physical RAM on an unconstrained host; none only when neither is known.
//
// Admission (for flags left at AutoMB): admitted bytes are request bodies held
// on the Go heap next to the base heap, so together they must fit the Go
// limit. Each admitted body is held once: the needle parsed from the request
// is appended to the .dat file and sent to every replica from that same
// buffer, without a per-write or per-replica copy:
//
//	budget   = max(goLimit - VolumeBaseHeapBytes, goLimit / 4)
//	upload   = budget * 3 / 4
//	download = budget - upload
//
// The 3:1 split keeps the ratio of the previously audited 3072 MB upload /
// 1024 MB download limits. The goLimit/4 floor keeps a small container
// serving when its base heap estimate does not fit. When the available memory
// is unknown the budgets stay 0 (unlimited), which is upstream's default. For
// 5 GiB available this gives a 4.5 GiB Go limit and 2496 MiB upload / 832 MiB
// download.
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
