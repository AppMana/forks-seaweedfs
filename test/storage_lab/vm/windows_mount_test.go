package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	labv1 "github.com/appmana/labcontainers/api/v1"
	"github.com/appmana/labcontainers/pkg/client"
)

// Reuse the real WinFsp smoke reproducer unchanged, in a fresh isolated VM.
// Installers are supplied from the host: the guest needs no Internet access.
func TestWindowsMountLab(t *testing.T) {
	if os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_LIVE") != "1" {
		t.Skip("set SEAWEEDFS_WINDOWS_MOUNT_LIVE=1")
	}
	resultDir, err := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "seaweedfs-windows-mount-results-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained results: %s", resultDir)
	scenarios := []string{"GitAtomicRenamePrimed", "GitLfsTempMetadata"}
	if scenario := os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_SCENARIO"); scenario != "" {
		switch scenario {
		case "GitAtomicRenamePrimed", "GitLfsTempMetadata":
			scenarios = []string{scenario}
		default:
			t.Fatal("SEAWEEDFS_WINDOWS_MOUNT_SCENARIO must be GitAtomicRenamePrimed or GitLfsTempMetadata")
		}
	}
	repeats := 1
	if value := os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_REPEATS"); value != "" {
		repeats, err = strconv.Atoi(value)
		if err != nil || repeats < 1 || repeats > 20 {
			t.Fatal("SEAWEEDFS_WINDOWS_MOUNT_REPEATS must be 1..20")
		}
	}
	verbosity := "0"
	if value := os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_VERBOSITY"); value != "" {
		v, parseErr := strconv.Atoi(value)
		if parseErr != nil || v < 0 || v > 4 {
			t.Fatal("SEAWEEDFS_WINDOWS_MOUNT_VERBOSITY must be 0..4")
		}
		verbosity = value
	}
	inputs := map[string]string{
		`C:\lab\weed.exe`:          os.Getenv("SEAWEEDFS_WINDOWS_WEED"),
		`C:\lab\winfsp.msi`:        os.Getenv("SEAWEEDFS_WINFSP_MSI"),
		`C:\lab\git-installer.exe`: os.Getenv("SEAWEEDFS_GIT_INSTALLER"),
		`C:\lab\mount-smoke.ps1`:   filepath.Join("..", "..", "..", "hack", "appmana", "mount-smoke.ps1"),
	}
	if nativeTest := os.Getenv("SEAWEEDFS_WINDOWS_WINFSP_TEST"); nativeTest != "" {
		inputs[`C:\lab\winfsp.test.exe`] = nativeTest
	}
	artifacts := map[string][]byte{}
	for target, path := range inputs {
		if path == "" {
			t.Fatalf("missing input for %s", target)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		artifacts[target] = b
		t.Logf("artifact=%s sha256=%s", target, sha(b))
	}
	img := os.Getenv("LABCONTAINERS_WINDOWS_IMAGE")
	if img == "" {
		t.Fatal("LABCONTAINERS_WINDOWS_IMAGE required")
	}
	// Readiness (10m), installation (5m), and two scenarios (8m each),
	// plus staging overhead. Leave the outer Go timeout room for cleanup.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	c, err := client.Launch(ctx, client.Options{LabdPath: os.Getenv("LABCONTAINERS_LABD")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	}()
	topology, err := windowsTopology(img)
	if err != nil {
		t.Fatal(err)
	}
	lab, err := c.Start(ctx, &labv1.LabSpec{Topology: topology, Nodes: map[string]*labv1.NodeExtension{"vm": {Control: "qga"}}}, 45*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ps := `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	_, err = lab.RunTimeline(ctx, &labv1.TimelineAction{Action: &labv1.TimelineAction_WaitExec{WaitExec: &labv1.WaitExec{Exec: &labv1.ExecRequest{Node: &labv1.NodeRef{Node: "vm"}, Argv: []string{ps, "-NoProfile", "-Command", "Write-Output 'windows-ready'"}, TimeoutMillis: 10000}, TimeoutMillis: 600000, RetryMillis: 2000, StdoutContains: []byte("windows-ready")}}})
	if err != nil {
		t.Fatal(err)
	}
	n := lab.Node("vm")
	t.Log("Windows ready; staging offline inputs")
	defer func() {
		diagnosticCtx, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		logs, logErr := c.RPC().Exec(diagnosticCtx, &labv1.ExecRequest{Node: n.Ref(), TimeoutMillis: 50000, Argv: []string{ps, "-NoProfile", "-Command", `Get-ChildItem C:\lab\smoke-*\logs\* -File -ErrorAction SilentlyContinue | ForEach-Object { Write-Output ("FILE: " + $_.FullName); Get-Content -LiteralPath $_.FullName -Tail 2000 }`}})
		if writeErr := os.WriteFile(filepath.Join(resultDir, "guest-logs.txt"), []byte(fmt.Sprintf("collection error: %v\nexit: %d\n%s\n%s", logErr, logs.GetExitCode(), logs.GetStdout(), logs.GetStderr())), 0600); writeErr != nil {
			t.Error(writeErr)
		}
	}()
	for target, b := range artifacts {
		t.Logf("staging %s (%d bytes)", target, len(b))
		if err := n.Put(ctx, target, 0600, b); err != nil {
			t.Fatal(err)
		}
	}
	setup := `$ErrorActionPreference='Stop';
$p=Start-Process msiexec.exe -ArgumentList '/i','C:\lab\winfsp.msi','/qn','/norestart','INSTALLLEVEL=1000' -Wait -PassThru;
if($p.ExitCode -notin @(0,3010)){throw "WinFsp installer exit $($p.ExitCode)"};
if(-not(Get-Service WinFsp.Launcher -ErrorAction SilentlyContinue)){throw 'WinFsp service absent'};
$p=Start-Process 'C:\lab\git-installer.exe' -ArgumentList '/VERYSILENT','/NORESTART','/SP-','/SUPPRESSMSGBOXES' -Wait -PassThru;
if($p.ExitCode -ne 0){throw "Git installer exit $($p.ExitCode)"};
& 'C:\Program Files\Git\cmd\git.exe' --version; if($LASTEXITCODE -ne 0){exit $LASTEXITCODE}`
	r, err := c.RPC().Exec(ctx, &labv1.ExecRequest{Node: n.Ref(), TimeoutMillis: int64((5 * time.Minute) / time.Millisecond), Argv: []string{ps, "-NoProfile", "-NonInteractive", "-Command", setup}})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(r.GetStdout()), string(r.GetStderr()))
	if r.GetExitCode() != 0 {
		t.Fatalf("dependency installation exit %d", r.GetExitCode())
	}
	for repetition := 1; repetition <= repeats; repetition++ {
		for _, scenario := range scenarios {
			caseName := fmt.Sprintf("%s-%02d", scenario, repetition)
			guestLog := `C:\lab\` + caseName + `.log`
			command := `$env:PATH='C:\Program Files\Git\cmd;'+$env:PATH; & C:\lab\mount-smoke.ps1 -WeedExe C:\lab\weed.exe -WorkRoot C:\lab\smoke-` + caseName + ` -TestCase ` + scenario + ` -GitIterations 20 -TraceSummary -Verbosity ` + verbosity + ` *>&1 | Tee-Object -FilePath ` + guestLog + `; exit $LASTEXITCODE`
			if os.Getenv("SEAWEEDFS_WINDOWS_WINFSP_TEST") != "" {
				command = strings.Replace(command, " -TestCase ", ` -WinFspTestExe C:\lab\winfsp.test.exe -TestCase `, 1)
			}
			r, err := c.RPC().Exec(ctx, &labv1.ExecRequest{Node: n.Ref(), TimeoutMillis: int64((8 * time.Minute) / time.Millisecond), Argv: []string{ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command}})
			if err != nil {
				// Use a separate bounded context to salvage diagnostics even when the
				// scenario context expired. The RPC error remains a test failure.
				diagnosticCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
				partial, readErr := c.RPC().Exec(diagnosticCtx, &labv1.ExecRequest{Node: n.Ref(), TimeoutMillis: 25000, Argv: []string{ps, "-NoProfile", "-Command", "Get-Content -LiteralPath '" + guestLog + "' -ErrorAction Stop"}})
				stop()
				log := fmt.Sprintf("execution error: %v\ndiagnostic error: %v\ndiagnostic exit: %d\n%s\n%s", err, readErr, partial.GetExitCode(), partial.GetStdout(), partial.GetStderr())
				if writeErr := os.WriteFile(filepath.Join(resultDir, caseName+".log"), []byte(log), 0600); writeErr != nil {
					t.Error(writeErr)
				}
				t.Fatal(log)
			}
			output := string(r.GetStdout()) + string(r.GetStderr())
			if err := os.WriteFile(filepath.Join(resultDir, caseName+".log"), []byte(output), 0600); err != nil {
				t.Fatal(err)
			}
			t.Log(output)
			if r.GetExitCode() != 0 || strings.Contains(output, "FAIL:") {
				t.Fatalf("%s failed: exit %d", scenario, r.GetExitCode())
			}
			marker := "PASS: git init iteration 20 leaves no stale config.lock"
			if scenario == "GitLfsTempMetadata" {
				marker = "PASS: Git LFS status iteration 20 reports all 32 modified assets"
				for _, required := range []string{"PASS: Git LFS filter is active", "PASS: Git LFS seed commit succeeds"} {
					if !strings.Contains(output, required) {
						t.Fatalf("%s missing prerequisite evidence: %s", scenario, required)
					}
				}
			}
			if !strings.Contains(output, marker) {
				t.Fatalf("%s did not complete all required iterations", scenario)
			}
			if scenario == "GitLfsTempMetadata" && os.Getenv("SEAWEEDFS_WINDOWS_WINFSP_TEST") != "" && !strings.Contains(output, "PASS: native Git LFS object rename reproducer completes without skips") {
				t.Fatal("native object-rename regression did not complete")
			}
		}
	}
}
