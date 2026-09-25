package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"testing"
	"time"

	labv1 "github.com/appmana/labcontainers/api/v1"
	"github.com/appmana/labcontainers/pkg/client"
	clab "github.com/appmana/labcontainers/pkg/containerlab"
)

var intervalBenchmarkLine = regexp.MustCompile(`(?m)^BenchmarkIntervalAppendAndCompletion/(4096|16384)(?:-\d+)?\s+\d+\s+([0-9.]+) ns/op`)

func intervalSamples(output string) (map[string][]float64, error) {
	result := map[string][]float64{}
	for _, match := range intervalBenchmarkLine.FindAllStringSubmatch(output, -1) {
		n, err := strconv.ParseFloat(match[2], 64)
		if err != nil || n <= 0 || math.IsInf(n, 0) || math.IsNaN(n) {
			return nil, fmt.Errorf("invalid benchmark sample %q", match[2])
		}
		result[match[1]] = append(result[match[1]], n)
	}
	for _, size := range []string{"4096", "16384"} {
		if len(result[size]) != 1 {
			return nil, fmt.Errorf("expected one %s sample, got %d", size, len(result[size]))
		}
	}
	return result, nil
}

func median(samples []float64) float64 {
	copyOf := append([]float64(nil), samples...)
	sort.Float64s(copyOf)
	return copyOf[len(copyOf)/2]
}

func qualifyIntervalPerformance(baseline, candidate map[string][]float64) error {
	for _, size := range []string{"4096", "16384"} {
		if len(baseline[size]) != 5 || len(candidate[size]) != 5 {
			return fmt.Errorf("five independent samples required for %s", size)
		}
		if median(candidate[size]) > 1.25*median(baseline[size]) {
			return fmt.Errorf("%s candidate regressed more than 25%%", size)
		}
	}
	if median(candidate["16384"])/median(candidate["4096"]) >= 8 {
		return fmt.Errorf("4x writes must take less than 8x time; quadratic interval tracking detected")
	}
	return nil
}

func TestIntervalPerformanceGateRejectsQuadraticAndMissingSamples(t *testing.T) {
	old := map[string][]float64{"4096": {21, 22, 21, 20, 21}, "16384": {318, 312, 321, 319, 318}}
	if qualifyIntervalPerformance(old, old) == nil {
		t.Fatal("quadratic implementation accepted")
	}
	if qualifyIntervalPerformance(old, map[string][]float64{}) == nil {
		t.Fatal("missing results accepted")
	}
	fast := map[string][]float64{"4096": {1, 1, 1, 1, 1}, "16384": {4, 4, 4, 4, 4}}
	if err := qualifyIntervalPerformance(old, fast); err != nil {
		t.Fatal(err)
	}
	if _, err := intervalSamples("PASS"); err == nil {
		t.Fatal("empty benchmark accepted")
	}
}

// Opt-in native VM gate: identical benchmark instrumentation in two Go test
// binaries, five alternating trials, one CPU, no external network or disk.
// Source commits and binary hashes are retained alongside raw guest output.
func TestIntervalPerformanceLab(t *testing.T) {
	if os.Getenv("SEAWEEDFS_INTERVAL_PERFORMANCE_LIVE") != "1" {
		t.Skip("set SEAWEEDFS_INTERVAL_PERFORMANCE_LIVE=1")
	}
	runNativePerformanceLab(t, "interval", "BenchmarkIntervalAppendAndCompletion", "page-writer interval CPU scaling; not mounted IO throughput", intervalSamples, qualifyIntervalPerformance)
}

func runNativePerformanceLab(t *testing.T, name, benchmark, scope string, parse func(string) (map[string][]float64, error), qualify func(map[string][]float64, map[string][]float64) error) {
	t.Helper()
	results, err := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "seaweedfs-"+name+"-results-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained results: %s", results)
	manifest := map[string]any{"status": "failed", "baseline_commit": os.Getenv("SEAWEEDFS_PERFORMANCE_BASELINE_COMMIT"), "candidate_commit": os.Getenv("SEAWEEDFS_PERFORMANCE_CANDIDATE_COMMIT"), "scope": scope, "benchmark": benchmark, "max_baseline_ratio": 1.25}
	if name == "interval" {
		manifest["max_scaling_ratio"] = 8
	}
	defer func() {
		data, err := json.MarshalIndent(manifest, "", "  ")
		if err == nil {
			err = os.WriteFile(filepath.Join(results, "manifest.json"), data, 0600)
		}
		if err != nil {
			t.Error(err)
		}
	}()
	binaries := map[string][]byte{}
	for _, name := range []string{"BASELINE", "CANDIDATE"} {
		path := os.Getenv("SEAWEEDFS_PERFORMANCE_" + name)
		if !filepath.IsAbs(path) || os.Getenv("SEAWEEDFS_PERFORMANCE_"+name+"_COMMIT") == "" {
			t.Fatalf("absolute %s binary and source commit required", name)
		}
		binaries[name], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		manifest[name+"_sha256"] = sha(binaries[name])
	}
	if sha(binaries["BASELINE"]) == sha(binaries["CANDIDATE"]) {
		t.Fatal("baseline and candidate binaries must differ")
	}
	image := os.Getenv("LABCONTAINERS_VM_IMAGE")
	if image == "" {
		t.Fatal("LABCONTAINERS_VM_IMAGE required")
	}
	manifest["image"] = image
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	config := windowsTopologyConfig(image)
	config.Name = "seaweedfs-" + name + "-performance"
	source, err := clab.Source(config)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Launch(ctx, client.Options{LabdPath: os.Getenv("LABCONTAINERS_LABD")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	lab, err := c.Start(ctx, &labv1.LabSpec{Topology: source, Nodes: map[string]*labv1.NodeExtension{"vm": {Control: "qga"}}, ArtifactDirectory: results}, 18*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lab.RunTimeline(ctx, &labv1.TimelineAction{Action: &labv1.TimelineAction_WaitExec{WaitExec: &labv1.WaitExec{Exec: &labv1.ExecRequest{Node: &labv1.NodeRef{Node: "vm"}, Argv: []string{"sh", "-ec", "test -z \"$(ip route show default)\"; echo ready"}, TimeoutMillis: 10000}, TimeoutMillis: 600000, RetryMillis: 2000, StdoutContains: []byte("ready")}}})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range binaries {
		if err := lab.Node("vm").Put(ctx, "/tmp/"+name+".test", 0755, data); err != nil {
			t.Fatal(err)
		}
	}
	samples := map[string]map[string][]float64{"BASELINE": {}, "CANDIDATE": {}}
	for trial := 0; trial < 6; trial++ {
		order := []string{"BASELINE", "CANDIDATE"}
		if trial%2 != 0 {
			order[0], order[1] = order[1], order[0]
		}
		for _, name := range order {
			r, err := lab.Node("vm").ExecWithTimeout(ctx, time.Minute, "env", "GOMAXPROCS=1", "/tmp/"+name+".test", "-test.run=^$", "-test.bench=^"+benchmark+"$", "-test.benchtime=1s", "-test.count=1")
			if writeErr := os.WriteFile(filepath.Join(results, fmt.Sprintf("%d-%s.log", trial, name)), []byte(fmt.Sprintf("error=%v\n%s\n%s", err, r.GetStdout(), r.GetStderr())), 0600); writeErr != nil {
				t.Fatal(writeErr)
			}
			if err != nil || r.GetExitCode() != 0 {
				t.Fatalf("%s trial %d failed: %v exit=%d", name, trial, err, r.GetExitCode())
			}
			parsed, err := parse(string(r.GetStdout()))
			if err != nil {
				t.Fatal(err)
			}
			if trial > 0 {
				for size, values := range parsed {
					samples[name][size] = append(samples[name][size], values...)
				}
			}
		}
	}
	manifest["samples_ns"] = samples
	if err := qualify(samples["BASELINE"], samples["CANDIDATE"]); err != nil {
		t.Fatal(err)
	}
	manifest["status"] = "passed"
}
