package main

import (
	"os/exec"
	"testing"
)

func TestVacuumPayloadCannotCollapseUnderCompression(t *testing.T) {
	// The copy-phase crash rendezvous needs actual bytes on disk, not a
	// multi-MiB logical payload that compresses to a few KiB before vacuum.
	check := "\nimport zlib\np=payload(0)\nassert len(p)>2*1024*1024\nassert p==payload(0) and p!=payload(1)\nassert len(zlib.compress(p))>len(p)*.95, (len(p),len(zlib.compress(p)))\n"
	if output, err := exec.Command("python3", "-c", workload+check, "payload-contract").CombinedOutput(); err != nil {
		t.Fatalf("vacuum payload contract: %v\n%s", err, output)
	}
}
