package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeCompactor stands in for a volume server whose unthrottled copy keeps
// its .cpd for window before committing it, as XFS compaction of the lab's
// 44 MB live set did in about 0.25 s.
const fakeCompactor = `import os, sys, time
d, window = sys.argv[1], float(sys.argv[2])
time.sleep(0.2)
cpd = os.path.join(d, '1.cpd')
with open(cpd, 'w') as f: f.write('copy')
time.sleep(window)
os.rename(cpd, os.path.join(d, '1.dat'))
open(os.path.join(d, 'committed'), 'w').close()
time.sleep(60)
`

func runBarrier(t *testing.T, window time.Duration) (dir, marker string, compactor *exec.Cmd, barrierErr error) {
	t.Helper()
	dir = t.TempDir()
	compactor = exec.Command("python3", "-c", fakeCompactor, dir, strconv.FormatFloat(window.Seconds(), 'f', 3, 64))
	if err := compactor.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		compactor.Process.Kill()
		compactor.Wait()
	})
	pidFile := filepath.Join(t.TempDir(), "volume.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(compactor.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(t.TempDir(), "barrier")
	barrierErr = exec.Command("python3", "-c", vacuumCopyBarrier, dir, pidFile, marker, "5").Run()
	return dir, marker, compactor, barrierErr
}

func processState(t *testing.T, pid int) string {
	t.Helper()
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))[0]
}

// The host samples the victim through a guest-agent exec every ~230 ms, so a
// copy that lives 0.25 s can be missed; the barrier must catch a copy far
// shorter than that and hold the compactor inside it until the power cut.
func TestVacuumCopyBarrierHoldsAShortCopyOpen(t *testing.T) {
	for i := 0; i < 20; i++ {
		dir, marker, compactor, err := runBarrier(t, 20*time.Millisecond)
		if err != nil {
			t.Fatalf("run %d: barrier did not catch a 20 ms copy: %v", i, err)
		}
		frozen, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(frozen)); got != filepath.Join(dir, "1.cpd") {
			t.Fatalf("run %d: marker lists %q", i, got)
		}
		if state := processState(t, compactor.Process.Pid); state != "T" {
			t.Fatalf("run %d: compactor state %q, want stopped", i, state)
		}
		time.Sleep(100 * time.Millisecond)
		if _, err := os.Stat(filepath.Join(dir, "1.cpd")); err != nil {
			t.Fatalf("run %d: copy did not stay in progress: %v", i, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "committed")); err == nil {
			t.Fatalf("run %d: compactor committed while held", i)
		}
	}
}

// A vacuum whose copy never appears must fail the gate, not report a power
// cut during a copy that did not happen.
func TestVacuumCopyBarrierFailsWithoutACopy(t *testing.T) {
	dir := t.TempDir()
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		sleeper.Process.Kill()
		sleeper.Wait()
	}()
	pidFile := filepath.Join(t.TempDir(), "volume.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(sleeper.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "barrier")
	if err := exec.Command("python3", "-c", vacuumCopyBarrier, dir, pidFile, marker, "0.5").Run(); err == nil {
		t.Fatal("barrier succeeded with no copy")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("marker written with no copy")
	}
	if state := processState(t, sleeper.Process.Pid); state == "T" {
		t.Fatal("volume server left stopped")
	}
}
