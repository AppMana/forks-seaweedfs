package main

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"testing"
)

var indexBenchmarkLine = regexp.MustCompile(`(?m)^BenchmarkLevelDBRecoveryOpen/(10000|16384|65536)(?:-\d+)?\s+\d+\s+([0-9.]+) ns/op`)
var indexSizes = []string{"10000", "16384", "65536"}

func indexSamples(output string) (map[string][]float64, error) {
	result := map[string][]float64{}
	for _, match := range indexBenchmarkLine.FindAllStringSubmatch(output, -1) {
		n, err := strconv.ParseFloat(match[2], 64)
		if err != nil || n <= 0 || math.IsInf(n, 0) || math.IsNaN(n) {
			return nil, fmt.Errorf("invalid sample %q", match[2])
		}
		result[match[1]] = append(result[match[1]], n)
	}
	for _, size := range indexSizes {
		if len(result[size]) != 1 {
			return nil, fmt.Errorf("expected one %s sample", size)
		}
	}
	return result, nil
}

func qualifyIndexPerformance(baseline, candidate map[string][]float64) error {
	for _, size := range indexSizes {
		if len(baseline[size]) != 5 || len(candidate[size]) != 5 {
			return fmt.Errorf("five samples required for %s", size)
		}
		if median(candidate[size]) > 1.25*median(baseline[size]) {
			return fmt.Errorf("%s recovery startup regressed more than 25%%", size)
		}
	}
	return nil
}

func TestIndexPerformanceGate(t *testing.T) {
	baseline, slow := map[string][]float64{}, map[string][]float64{}
	for _, size := range indexSizes {
		baseline[size] = []float64{10, 10, 10, 10, 10}
		slow[size] = []float64{30, 30, 30, 30, 30}
	}
	if err := qualifyIndexPerformance(baseline, baseline); err != nil {
		t.Fatal(err)
	}
	if qualifyIndexPerformance(baseline, slow) == nil {
		t.Fatal("3x startup regression accepted")
	}
	if qualifyIndexPerformance(baseline, map[string][]float64{}) == nil {
		t.Fatal("missing samples accepted")
	}
	if _, err := indexSamples("PASS"); err == nil {
		t.Fatal("empty benchmark accepted")
	}
}

func TestIndexRecoveryPerformanceLab(t *testing.T) {
	if os.Getenv("SEAWEEDFS_INDEX_PERFORMANCE_LIVE") != "1" {
		t.Skip("set SEAWEEDFS_INDEX_PERFORMANCE_LIVE=1")
	}
	runNativePerformanceLab(t, "index-recovery", "BenchmarkLevelDBRecoveryOpen", "LevelDB startup with checkpoint and replay tails; not mounted IO throughput", indexSamples, qualifyIndexPerformance)
}
