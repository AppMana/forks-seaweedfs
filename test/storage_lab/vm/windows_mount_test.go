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
	trace := os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_TRACE") == "1"
	if value := os.Getenv("SEAWEEDFS_WINDOWS_MOUNT_TRACE"); value != "" && value != "0" && value != "1" {
		t.Fatal("SEAWEEDFS_WINDOWS_MOUNT_TRACE must be 0 or 1")
	}
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
	if trace {
		inputs[`C:\lab\seaweed-fileio.wprp`] = filepath.Join("..", "..", "..", "hack", "appmana", "seaweed-fileio.wprp")
	}
	if diagnosticLFS := os.Getenv("SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC"); diagnosticLFS != "" {
		inputs[`C:\lab\git-lfs-diagnostic.exe`] = diagnosticLFS
		inputs[`C:\lab\install-lab-git-lfs.ps1`] = filepath.Join("..", "..", "..", "hack", "appmana", "install-lab-git-lfs.ps1")
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
	topology := []byte(fmt.Sprintf("name: ignored\ntopology:\n  nodes:\n    vm:\n      kind: generic_vm\n      image: %q\n      network-mode: none\n    peer:\n      kind: linux\n      image: alpine:3.20\n      network-mode: none\n  links:\n    - endpoints: [vm:eth1, peer:eth1]\n", img))
	lab, err := c.Start(ctx, &labv1.LabSpec{Topology: &labv1.TopologySource{Source: &labv1.TopologySource_Yaml{Yaml: topology}}, Nodes: map[string]*labv1.NodeExtension{"vm": {Control: "qga"}}}, 45*time.Minute)
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
		logs, logErr := n.ExecWithTimeout(diagnosticCtx, 50*time.Second, ps, "-NoProfile", "-Command", `Get-ChildItem C:\lab\smoke-*\logs\*,C:\lab\dependency-install.log -File -ErrorAction SilentlyContinue | Where-Object { $_.Extension -in '.log','.txt' } | ForEach-Object { Write-Output ("FILE: " + $_.FullName); Get-Content -LiteralPath $_.FullName -Tail 2000 }`)
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
Start-Transcript -LiteralPath C:\lab\dependency-install.log -Force;
Write-Output "SETUP $(Get-Date -Format o): WinFsp install starting";
$p=Start-Process msiexec.exe -ArgumentList '/i','C:\lab\winfsp.msi','/qn','/norestart','INSTALLLEVEL=1000' -Wait -PassThru;
Write-Output "SETUP $(Get-Date -Format o): WinFsp install exit $($p.ExitCode)";
if($p.ExitCode -notin @(0,3010)){throw "WinFsp installer exit $($p.ExitCode)"};
if(-not(Get-Service WinFsp.Launcher -ErrorAction SilentlyContinue)){throw 'WinFsp service absent'};
Write-Output "SETUP $(Get-Date -Format o): Git install starting";
$p=Start-Process 'C:\lab\git-installer.exe' -ArgumentList '/VERYSILENT','/NORESTART','/SP-','/SUPPRESSMSGBOXES' -Wait -PassThru;
Write-Output "SETUP $(Get-Date -Format o): Git install exit $($p.ExitCode)";
if($p.ExitCode -ne 0){throw "Git installer exit $($p.ExitCode)"};
& 'C:\Program Files\Git\cmd\git.exe' --version; if($LASTEXITCODE -ne 0){exit $LASTEXITCODE}`
	if os.Getenv("SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC") != "" {
		setup += `; & C:\lab\install-lab-git-lfs.ps1 -GitRoot 'C:\Program Files\Git' -Candidate C:\lab\git-lfs-diagnostic.exe; $env:PATH='C:\Program Files\Git\cmd;'+$env:PATH; & git --version; if($LASTEXITCODE -ne 0){exit $LASTEXITCODE}; & git lfs version; if($LASTEXITCODE -ne 0){exit $LASTEXITCODE}`
	}
	setup += `; Write-Output "SETUP $(Get-Date -Format o): complete"; Stop-Transcript`
	r, err := n.ExecWithTimeout(ctx, 5*time.Minute, ps, "-NoProfile", "-NonInteractive", "-Command", setup)
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
			if trace {
				command = strings.Replace(command, " -TraceSummary ", " -TraceSummary -Trace -EtwFileIO ", 1)
			}
			if os.Getenv("SEAWEEDFS_WINDOWS_WINFSP_TEST") != "" {
				command = strings.Replace(command, " -TestCase ", ` -WinFspTestExe C:\lab\winfsp.test.exe -TestCase `, 1)
			}
			r, err := n.ExecWithTimeout(ctx, 8*time.Minute, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command)
			if trace {
				decodeCtx, finishDecode := context.WithTimeout(context.Background(), time.Minute)
				decoded, decodeErr := n.ExecWithTimeout(decodeCtx, 55*time.Second, ps, "-NoProfile", "-Command", `$ErrorActionPreference='Stop'; & tracerpt.exe 'C:\lab\smoke-`+caseName+`\logs\fileio.etl' -of XML -o 'C:\lab\smoke-`+caseName+`\logs\fileio.xml' -y; if($LASTEXITCODE -ne 0){exit $LASTEXITCODE}; Compress-Archive -LiteralPath 'C:\lab\smoke-`+caseName+`\logs\fileio.xml' -DestinationPath 'C:\lab\smoke-`+caseName+`\logs\fileio.xml.zip'`)
				finishDecode()
				if decodeErr != nil || decoded.GetExitCode() != 0 {
					t.Errorf("%s ETW decode: %v exit %d: %s", caseName, decodeErr, decoded.GetExitCode(), decoded.GetStderr())
				}
				for _, artifact := range []string{"fileio.etl", "fileio.xml.zip", "mount1-winfsp-trace.log"} {
					traceCtx, stop := context.WithTimeout(context.Background(), 4*time.Minute)
					data, traceErr := readWindowsArtifact(`C:\lab\smoke-`+caseName+`\logs\`+artifact, func(command string) ([]byte, error) {
						result, execErr := n.ExecWithTimeout(traceCtx, 30*time.Second, ps, "-NoProfile", "-Command", "$ErrorActionPreference='Stop'; "+command)
						if execErr != nil {
							return nil, execErr
						}
						if result.GetExitCode() != 0 {
							return nil, fmt.Errorf("trace download exit %d: %s", result.GetExitCode(), result.GetStderr())
						}
						return result.GetStdout(), nil
					})
					stop()
					if traceErr != nil {
						t.Errorf("%s %s evidence: %v", caseName, artifact, traceErr)
					} else if writeErr := os.WriteFile(filepath.Join(resultDir, caseName+"-"+artifact), data, 0600); writeErr != nil {
						t.Error(writeErr)
					} else {
						t.Logf("trace artifact=%s-%s bytes=%d sha256=%s", caseName, artifact, len(data), sha(data))
					}
				}
			}
			if err != nil {
				// Use a separate bounded context to salvage diagnostics even when the
				// scenario context expired. The RPC error remains a test failure.
				diagnosticCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
				partial, readErr := n.ExecWithTimeout(diagnosticCtx, 25*time.Second, ps, "-NoProfile", "-Command", "Get-Content -LiteralPath '"+guestLog+"' -ErrorAction Stop")
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
