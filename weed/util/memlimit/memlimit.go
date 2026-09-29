// Package memlimit derives a process's memory settings from the memory limit
// of the container it runs in, the way the JVM sizes its heap from the cgroup:
// set only resources.limits.memory and the Go memory limit and the volume
// server's transfer admission budgets follow from it.
//
// Go does not do this by itself: it reads the cgroup CPU quota for GOMAXPROCS
// but never the memory limit, so without this a container's GOMEMLIMIT and
// admission flags have to be kept consistent with its limit by hand.
package memlimit

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// cgroup v2 unified hierarchy, as mounted inside a container.
	cgroupV2MemoryMax = "sys/fs/cgroup/memory.max"
	// cgroup v1 memory controller, as mounted inside a container.
	cgroupV1MemoryLimit = "sys/fs/cgroup/memory/memory.limit_in_bytes"
	// cgroup v1 reports "no limit" as the largest page-aligned int64,
	// 9223372036854771712; anything this large is not a real limit.
	cgroupV1Unlimited = int64(1) << 62

	// GoMemLimitRatio is the share of the container limit given to the Go
	// runtime as its soft memory limit. The remaining 20% is memory the Go
	// limit does not bound but the cgroup charges: goroutine stacks (a
	// saturated volume server queued 2,700 requests), runtime metadata and
	// heap fragmentation, and file pages the kernel must be able to reclaim.
	// A Go limit at the cgroup limit would let the heap alone reach the OOM
	// killer before the collector reacts.
	GoMemLimitRatio = 0.8

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

// CgroupMemoryLimit returns the memory limit of the cgroup visible under
// root ("/" in production). ok is false when no limit applies: cgroup v2
// "max", the cgroup v1 unlimited sentinel, or no cgroup memory files at all.
func CgroupMemoryLimit(root string) (limit int64, ok bool, err error) {
	if raw, readErr := os.ReadFile(filepath.Join(root, cgroupV2MemoryMax)); readErr == nil {
		value := strings.TrimSpace(string(raw))
		if value == "max" {
			return 0, false, nil
		}
		limit, err = strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0, false, fmt.Errorf("parse %s %q: %w", cgroupV2MemoryMax, value, err)
		}
		return limit, true, nil
	} else if !errors.Is(readErr, fs.ErrNotExist) {
		return 0, false, fmt.Errorf("read %s: %w", cgroupV2MemoryMax, readErr)
	}

	raw, readErr := os.ReadFile(filepath.Join(root, cgroupV1MemoryLimit))
	if errors.Is(readErr, fs.ErrNotExist) {
		return 0, false, nil
	}
	if readErr != nil {
		return 0, false, fmt.Errorf("read %s: %w", cgroupV1MemoryLimit, readErr)
	}
	value := strings.TrimSpace(string(raw))
	limit, err = strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse %s %q: %w", cgroupV1MemoryLimit, value, err)
	}
	if limit >= cgroupV1Unlimited {
		return 0, false, nil
	}
	return limit, true, nil
}

// VolumeMemoryInput is everything PlanVolumeMemory depends on.
type VolumeMemoryInput struct {
	CgroupLimit    int64
	HasCgroupLimit bool
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
// Go limit: an explicit GOMEMLIMIT wins; otherwise, when the container has a
// memory limit, GoMemLimitRatio of it; otherwise none.
//
// Admission (for flags left at AutoMB): admitted bytes are request bodies held
// on the Go heap next to the base heap, so together they must fit the Go
// limit:
//
//	budget   = max(goLimit - VolumeBaseHeapBytes, goLimit / 4)
//	upload   = budget * 3 / 4
//	download = budget - upload
//
// The 3:1 split keeps the ratio of the previously audited 3072 MB upload /
// 1024 MB download limits. The goLimit/4 floor keeps a small container
// serving when its base heap estimate does not fit. Without any memory limit
// the budgets stay 0 (unlimited), which is upstream's default. For a 5 GiB
// container this gives a 4 GiB Go limit and 2112 MiB upload / 704 MiB download.
func PlanVolumeMemory(in VolumeMemoryInput) VolumeMemoryPlan {
	plan := VolumeMemoryPlan{GoMemLimit: in.RuntimeLimit}
	if !in.EnvGOMEMLIMIT && in.HasCgroupLimit && in.CgroupLimit > 0 {
		plan.GoMemLimit = int64(float64(in.CgroupLimit) * GoMemLimitRatio)
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
