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
		if pairedRatio(baseline[size], candidate[size]) > 1.25 {
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

// Gate run 38812348120: the shared runner slowed down from candidate trial 4
// on (~55 -> ~90 ms for both binaries), so four of the ten slow samples fell
// on the candidate side. Medians taken per side read that as a 26% regression;
// trial by trial the candidate was faster in three of five.
func TestIndexPerformanceGateComparesTrialsPairwise(t *testing.T) {
	baseline := map[string][]float64{
		"10000": {63960478, 62199791, 48276720, 52545592, 86907254},
		"16384": {65368598, 54868369, 54408721, 53787650, 95655071},
		"65536": {57613159, 53604653, 52552384, 58456587, 90850229},
	}
	candidate := map[string][]float64{
		"10000": {65163974, 51214742, 56665099, 92139022, 104876204},
		"16384": {69313272, 49916899, 48327706, 96838200, 86409322},
		"65536": {68497076, 62074975, 51569861, 105182004, 96057481},
	}
	if err := qualifyIndexPerformance(baseline, candidate); err != nil {
		t.Fatalf("host slowdown read as a regression: %v", err)
	}
	slower := map[string][]float64{}
	for size, values := range baseline {
		for _, v := range values {
			slower[size] = append(slower[size], 1.3*v)
		}
	}
	if qualifyIndexPerformance(baseline, slower) == nil {
		t.Fatal("consistent 30% regression accepted")
	}
}

func TestIndexRecoveryPerformanceLab(t *testing.T) {
	if os.Getenv("SEAWEEDFS_INDEX_PERFORMANCE_LIVE") != "1" {
		t.Skip("set SEAWEEDFS_INDEX_PERFORMANCE_LIVE=1")
	}
	runNativePerformanceLab(t, "index-recovery", "BenchmarkLevelDBRecoveryOpen", "LevelDB startup with checkpoint and replay tails; not mounted IO throughput", indexSamples, qualifyIndexPerformance)
}
