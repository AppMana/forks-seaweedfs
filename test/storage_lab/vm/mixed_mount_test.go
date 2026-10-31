package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	labv1 "github.com/appmana/labcontainers/api/v1"
	"github.com/appmana/labcontainers/pkg/client"
)

// Both native mounted clients share a Linux filer, without host networking,
// production endpoints, credentials, or disk attachments. No API-only substitute.
func TestMixedOSMountLab(t *testing.T) {
	if os.Getenv("SEAWEEDFS_MIXED_LIVE") != "1" {
		t.Skip("set SEAWEEDFS_MIXED_LIVE=1")
	}
	results, err := os.MkdirTemp(os.Getenv("RUNNER_TEMP"), "seaweedfs-mixed-results-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained results: %s", results)
	completed := 0
	var phaseResults []string
	required := 8
	if os.Getenv("SEAWEEDFS_MIXED_CACHE_COHERENCE") == "1" {
		required++
	}
	t.Cleanup(func() {
		status := "passed"
		if t.Failed() || completed != required {
			status = "failed"
		}
		data, err := json.MarshalIndent(map[string]any{"status": status, "completed_paired_phases": completed, "required_paired_phases": required, "phase_results": phaseResults, "scope": "Linux filer with native Linux FUSE and Windows WinFsp mounts; not CSI, shared-file locking, or power loss"}, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(results, "manifest.json"), data, 0600); err != nil {
			t.Error(err)
		}
	})
	inputs := map[string]string{
		"linux-weed":       os.Getenv("SEAWEEDFS_LINUX_WEED"),
		"windows-weed":     os.Getenv("SEAWEEDFS_WINDOWS_WEED"),
		"linux-workload":   os.Getenv("SEAWEEDFS_MIXED_LINUX_WORKLOAD"),
		"windows-workload": os.Getenv("SEAWEEDFS_MIXED_WINDOWS_WORKLOAD"),
		"winfsp.msi":       os.Getenv("SEAWEEDFS_WINFSP_MSI"),
	}
	nativePackage := os.Getenv("SEAWEEDFS_WINDOWS_WINFSP_NATIVE_PACKAGE")
	winfsp, err := selectMixedWinFsp(os.Getenv("SEAWEEDFS_WINDOWS_WINFSP_DLL"), nativePackage,
		os.Getenv("SEAWEEDFS_WINDOWS_WINFSP_RELEASE_DLL"), os.Getenv("SEAWEEDFS_WINDOWS_WINFSP_RELEASE_DLL_SHA256"))
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range winfsp.inputs {
		inputs[name] = path
	}
	artifacts := map[string][]byte{}
	var provenance strings.Builder
	if nativePackage != "" {
		native, err := nativeWinFspInputs(nativePackage, os.Getenv("SEAWEEDFS_WINDOWS_WINFSP_FORK"))
		if err != nil {
			t.Fatal(err)
		}
		for target, data := range native {
			name := strings.TrimPrefix(target, `C:\lab\`)
			if name == target {
				t.Fatalf("unexpected native artifact target %s", target)
			}
			artifacts[name] = data
			fmt.Fprintf(&provenance, "native_artifact=%s sha256=%s\n", target, sha(data))
		}
	}
	traceLinux := os.Getenv("SEAWEEDFS_MIXED_TRACE") == "1"
	fmt.Fprintf(&provenance, "linux_fuse_trace=%t\n", traceLinux)
	for name, path := range inputs {
		if !filepath.IsAbs(path) {
			t.Fatalf("absolute input required for %s", name)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		artifacts[name] = b
		fmt.Fprintf(&provenance, "%s %s sha256=%s\n", name, path, sha(b))
	}
	winfspProvenance, err := winfsp.verify(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	provenance.WriteString(winfspProvenance)
	linuxImage, windowsImage := os.Getenv("LABCONTAINERS_VM_IMAGE"), os.Getenv("LABCONTAINERS_WINDOWS_IMAGE")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	for _, image := range []string{linuxImage, windowsImage} {
		if image == "" {
			t.Fatal("both LABCONTAINERS_VM_IMAGE and LABCONTAINERS_WINDOWS_IMAGE required")
		}
		id, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image).Output()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&provenance, "image=%s id=%s\n", image, strings.TrimSpace(string(id)))
	}
	if err := os.WriteFile(filepath.Join(results, "provenance.txt"), []byte(provenance.String()), 0600); err != nil {
		t.Fatal(err)
	}
	network := t.TempDir()
	if err := writeNetworkConfigs(network); err != nil {
		t.Fatal(err)
	}
	topology, err := mixedTopology(linuxImage, windowsImage, network)
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
	lab, err := c.Start(ctx, &labv1.LabSpec{Topology: topology, Nodes: map[string]*labv1.NodeExtension{"linux": {Control: "qga"}, "windows": {Control: "qga"}}, ArtifactDirectory: results}, 28*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ps := `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	run := func(node, label string, argv ...string) error {
		marker := "COMMAND_COMPLETE:" + filepath.Base(results) + ":" + node + ":" + label
		if !strings.HasPrefix(label, "phase-") {
			if node == "windows" {
				argv[len(argv)-1] += "; Write-Output '" + marker + "'"
			} else {
				argv = append([]string{"sh", "-c", `"$@"; rc=$?; if [ "$rc" = 0 ]; then echo '` + marker + `'; fi; exit "$rc"`, "mixed-command"}, argv...)
			}
		}
		r, err := lab.Node(node).ExecWithTimeout(ctx, 4*time.Minute, argv...)
		output := fmt.Sprintf("execution error: %v\n%s\n%s", err, r.GetStdout(), r.GetStderr())
		if writeErr := os.WriteFile(filepath.Join(results, node+"-"+label+".log"), []byte(output), 0600); writeErr != nil {
			return writeErr
		}
		if err != nil {
			return err
		}
		if r.GetExitCode() != 0 {
			return fmt.Errorf("%s %s exit %d: %s", node, label, r.GetExitCode(), output)
		}
		if strings.HasPrefix(label, "phase-") {
			marker = "MIXED_COMPLETE:" + filepath.Base(results) + ":" + node + ":" + strings.TrimPrefix(label, "phase-")
		}
		return validateWindowsCommandOutput(r.GetExitCode(), string(r.GetStdout()), marker)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, node := range []string{"linux", "windows"} {
		argv := []string{"sh", "-ec", "cloud-init status --wait >/dev/null; ip -4 addr show | grep -q 192.0.2.10; test -z \"$(ip route show default)\"; echo ready"}
		if node == "windows" {
			argv = []string{ps, "-NoProfile", "-Command", "Write-Output ready"}
		}
		_, err := lab.RunTimeline(ctx, &labv1.TimelineAction{Action: &labv1.TimelineAction_WaitExec{WaitExec: &labv1.WaitExec{Exec: &labv1.ExecRequest{Node: &labv1.NodeRef{Node: node}, Argv: argv, TimeoutMillis: 10000}, TimeoutMillis: 600000, RetryMillis: 2000, StdoutContains: []byte("ready")}}})
		must(err)
	}
	defer func() {
		diagnosticCtx, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, node := range []string{"linux", "windows"} {
			argv := []string{"sh", "-c", "tail -n 200 /var/log/mixed-*.log"}
			if traceLinux && node == "linux" {
				// Bounded diagnostic output, never a replacement for the strict
				// workload oracle. Record file sizes so truncation is visible.
				argv = []string{"sh", "-c", "echo TRACE_TAIL_LIMIT_BYTES=3000000; wc -c /var/log/mixed-mount-*.log; tail -c 3000000 /var/log/mixed-mount-*.log"}
			}
			if node == "windows" {
				argv = []string{ps, "-NoProfile", "-Command", `Get-ChildItem C:\lab\logs-* -Directory | ForEach-Object { Get-ChildItem -LiteralPath $_.FullName -File } | ForEach-Object { $_.Name; Get-Content -LiteralPath $_.FullName -Tail 200 }`}
			}
			r, err := lab.Node(node).ExecWithTimeout(diagnosticCtx, 25*time.Second, argv...)
			if writeErr := os.WriteFile(filepath.Join(results, node+"-guest.log"), []byte(fmt.Sprintf("collection error=%v\n%s\n%s", err, r.GetStdout(), r.GetStderr())), 0600); writeErr != nil {
				t.Error(writeErr)
			}
		}
	}()
	must(lab.Node("linux").Put(ctx, "/opt/weed", 0755, artifacts["linux-weed"]))
	must(lab.Node("linux").Put(ctx, "/opt/mixed", 0755, artifacts["linux-workload"]))
	for name, b := range artifacts {
		if strings.HasPrefix(name, "linux-") {
			continue
		}
		target := `C:\lab\` + name
		if name == "windows-weed" {
			target = `C:\lab\weed.exe`
		}
		if name == "windows-workload" {
			target = `C:\lab\mixed.exe`
		}
		must(lab.Node("windows").Put(ctx, target, 0600, b))
	}
	t.Log("both guests ready; installing offline WinFsp and starting shared Linux filer")
	must(run("windows", "setup", ps, "-NoProfile", "-Command", `$ErrorActionPreference='Stop';
$nic=@(Get-NetAdapter | Where-Object Status -eq Up); if($nic.Count -ne 1){throw "expected one isolated NIC: $($nic | Out-String)"};
if(Get-NetRoute -DestinationPrefix '0.0.0.0/0' -ErrorAction SilentlyContinue){throw 'unexpected default route'};
New-NetIPAddress -InterfaceIndex $nic[0].ifIndex -IPAddress 192.0.2.11 -PrefixLength 24 | Out-Null;
$p=Start-Process msiexec.exe -ArgumentList '/i','C:\lab\winfsp.msi','/qn','/norestart','INSTALLLEVEL=1000' -Wait -PassThru;
if($p.ExitCode -notin @(0,3010)){throw "installer exit $($p.ExitCode)"}; if(-not(Get-Service WinFsp.Launcher -ErrorAction SilentlyContinue)){throw 'WinFsp service absent'};
Write-Output SETUP_COMPLETE`))
	if nativePackage != "" {
		installNativeWinFsp(t, ctx, lab, lab.Node("windows"), results)
	}
	// Offline Windows images can interpret the virtual RTC as local time.
	// Mount subscriptions start at the client's time.Now(), so a future clock
	// silently excludes current filer events. Set only this fresh guest's clock,
	// then fail closed unless both guests agree with the controller within 5s.
	must(run("windows", "clock-set", ps, "-NoProfile", "-Command", fmt.Sprintf(`$ErrorActionPreference='Stop'; Set-Date -Date ([DateTimeOffset]::FromUnixTimeMilliseconds(%d).LocalDateTime) | Out-Null`, time.Now().UnixMilli())))
	for _, node := range []string{"linux", "windows"} {
		before := time.Now()
		argv := []string{"sh", "-ec", `printf 'GUEST_UNIX_SECONDS:'; date +%s`}
		if node == "windows" {
			argv = []string{ps, "-NoProfile", "-Command", `Write-Output ('GUEST_UNIX_SECONDS:'+[DateTimeOffset]::UtcNow.ToUnixTimeSeconds())`}
		}
		must(run(node, "clock-check", argv...))
		output, err := os.ReadFile(filepath.Join(results, node+"-clock-check.log"))
		must(err)
		must(validateMixedGuestClock(string(output), before, time.Now()))
	}
	must(run("linux", "server", "sh", "-ec", `test -c /dev/fuse; mkdir -p /var/lib/mixed /mnt/shared; cd /var/lib/mixed; nohup /opt/weed server -ip=192.0.2.10 -ip.bind=0.0.0.0 -dir=/var/lib/mixed -master.volumeSizeLimitMB=64 -volume.max=5 -filer >/var/log/mixed-server.log 2>&1 </dev/null &`))
	_, err = lab.RunTimeline(ctx, &labv1.TimelineAction{Action: &labv1.TimelineAction_WaitExec{WaitExec: &labv1.WaitExec{Exec: &labv1.ExecRequest{Node: &labv1.NodeRef{Node: "linux"}, Argv: []string{"python3", "-c", "import urllib.request; urllib.request.urlopen('http://192.0.2.10:8888/',timeout=2).read(); print('filer-ready')"}, TimeoutMillis: 5000}, TimeoutMillis: 120000, RetryMillis: 1000, StdoutContains: []byte("filer-ready")}}})
	must(err)
	startMounts := func(round string) {
		linuxMount := "/opt/weed mount"
		if traceLinux {
			linuxMount = "/opt/weed -v=4 mount -debug.fuse=true"
		}
		must(run("linux", "mount-"+round, "sh", "-ec", `mkdir -p /var/cache/mixed-`+round+`; nohup `+linuxMount+` -filer=192.0.2.10:8888 -dir=/mnt/shared -cacheDir=/var/cache/mixed-`+round+` >/var/log/mixed-mount-`+round+`.log 2>&1 </dev/null &`))
		_, err := lab.RunTimeline(ctx, &labv1.TimelineAction{Action: &labv1.TimelineAction_WaitExec{WaitExec: &labv1.WaitExec{Exec: &labv1.ExecRequest{Node: &labv1.NodeRef{Node: "linux"}, Argv: []string{"sh", "-ec", "mountpoint -q /mnt/shared; echo mounted"}, TimeoutMillis: 5000}, TimeoutMillis: 60000, RetryMillis: 500, StdoutContains: []byte("mounted")}}})
		must(err)
		// Match the non-traced mount-smoke.ps1 launch: no PowerShell output
		// redirection on the detached child. In live tests redirected launches
		// mounted successfully but left QGA waiting after the parent exited.
		// Use weed's own log directory and retain the strict completion marker.
		must(run("windows", "mount-"+round, ps, "-NoProfile", "-Command", `$ErrorActionPreference='Stop';
New-Item -ItemType Directory C:\lab\cache-`+round+` | Out-Null;
New-Item -ItemType Directory C:\lab\logs-`+round+` | Out-Null;
$p=Start-Process C:\lab\weed.exe -WindowStyle Hidden -PassThru -ArgumentList '-logdir=C:\lab\logs-`+round+`','mount','-filer=192.0.2.10:8888','-dir=C:\lab\mnt','-cacheDir=C:\lab\cache-`+round+`';
$p.Id | Set-Content C:\lab\mount.pid;
$deadline=(Get-Date).AddSeconds(60); while(-not(Test-Path C:\lab\mnt)){if($p.HasExited -or (Get-Date) -gt $deadline){throw 'mount failed'}; Start-Sleep -Milliseconds 100};
$modules=@((Get-Process -Id $p.Id).Modules | Where-Object ModuleName -ieq 'winfsp-x64.dll');
if($modules.Count -ne 1 -or $modules[0].FileName -ine 'C:\lab\winfsp-x64.dll'){throw 'candidate DLL not loaded'};
Write-Output 'verified loaded candidate DLL';`))
	}
	startMounts("first")
	must(run("linux", "barrier-directory", "mkdir", "/mnt/shared/.sync"))
	pair := func(action string) {
		t.Log("mixed phase: " + action)
		errs := make(chan error, 2)
		for _, node := range []string{"linux", "windows"} {
			go func(node string) {
				binary, root := "/opt/mixed", "/mnt/shared"
				if node == "windows" {
					binary, root = `C:\lab\mixed.exe`, `C:\lab\mnt`
				}
				errs <- run(node, "phase-"+action, binary, root, node, action, filepath.Base(results))
			}(node)
		}
		first, second := <-errs, <-errs
		if first != nil {
			t.Error(first)
		}
		if second != nil {
			t.Error(second)
		}
		if first != nil || second != nil {
			// Diagnostics never change the failed verdict. Compare mounted reads
			// with filer bytes, then observe whether the default 1s Linux attr
			// cache converges. Keep this separate from the no-retry oracle.
			// The cache phases use their own files under .sync, so inspect
			// those rather than the payload files.
			names := []string{"linux-00.bin", "windows-00.bin"}
			if strings.HasPrefix(action, "cache-coherence") {
				names = []string{".sync/" + action + "-data-linux", ".sync/" + action + "-data-windows"}
			}
			diagnostic := `import hashlib,os,sys,time,urllib.request
def inspect(label):
 for name in sys.argv[1:]:
  for source in ('mount','filer'):
   try:
    if source=='mount':
     p='/mnt/shared/'+name
     size=os.stat(p).st_size
     with open(p,'rb') as f: b=f.read()
    else:
     with urllib.request.urlopen('http://192.0.2.10:8888/'+name,timeout=3) as r: b=r.read()
     size=len(b)
    print(label,source,name,'stat',size,'read',len(b),'first',b[:1].hex(),'sha256',hashlib.sha256(b).hexdigest(),flush=True)
   except Exception as e: print(label,source,name,repr(e),flush=True)
inspect('immediate')
time.sleep(3)
inspect('after-3s')`
			if err := run("linux", "failed-"+action+"-diagnostic", append([]string{"python3", "-c", diagnostic}, names...)...); err != nil {
				t.Logf("auxiliary diagnostic: %v", err)
			}
			// The Windows view of the same files after the workload has
			// exited and released its descriptor and mapping.
			var windowsNames []string
			for _, name := range names {
				windowsNames = append(windowsNames, `'C:\lab\mnt\`+strings.ReplaceAll(name, "/", `\`)+`'`)
			}
			windowsDiagnostic := `$ErrorActionPreference='Continue'; foreach($label in 'immediate','after-3s'){ if($label -eq 'after-3s'){Start-Sleep -Seconds 3}; foreach($p in @(` + strings.Join(windowsNames, ",") + `)){ try { $b=[IO.File]::ReadAllBytes($p); $i=Get-Item -LiteralPath $p; $h=[BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash($b)).Replace('-','').ToLowerInvariant(); $f=''; if($b.Length -gt 0){$f='{0:x2}' -f $b[0]}; Write-Output "$label mount $p stat $($i.Length) read $($b.Length) first $f mtime $($i.LastWriteTimeUtc.ToString('o')) sha256 $h" } catch { Write-Output "$label mount $p $($_.Exception.Message)" } } }`
			if err := run("windows", "failed-"+action+"-diagnostic", ps, "-NoProfile", "-Command", windowsDiagnostic); err != nil {
				t.Logf("auxiliary diagnostic: %v", err)
			}
			phaseResults = append(phaseResults, action+"=failed")
			// A cache phase reads and writes only its own files, so the
			// lifecycle phases after it still run and report; the verdict
			// stays failed. A lifecycle failure leaves no state to continue on.
			if !strings.HasPrefix(action, "cache-coherence") {
				t.FailNow()
			}
			return
		}
		phaseResults = append(phaseResults, action+"=passed")
		completed++
	}
	for _, action := range []string{"seed", "verify-seed", "rewrite", "verify-rewrite", "rename-delete", "verify-final"} {
		pair(action)
		if action == "verify-seed" {
			pair("cache-coherence-files")
			if required == 9 {
				pair("cache-coherence")
			}
		}
	}
	// All payload handles were Sync'ed and closed; Windows restart is abrupt,
	// not a claim of graceful unmount or VM power-loss qualification.
	must(run("linux", "unmount", "umount", "/mnt/shared"))
	must(run("windows", "stop-mount", ps, "-NoProfile", "-Command", `$ErrorActionPreference='Stop'; $p=Get-Process -Id ([int](Get-Content C:\lab\mount.pid)); if($p.Path -ine 'C:\lab\weed.exe'){throw 'unexpected mount process'}; Stop-Process -Id $p.Id -Force; $p.WaitForExit(); Start-Sleep -Seconds 2; if(Test-Path C:\lab\mnt){throw 'owned junction survived mount termination'}`))
	startMounts("second")
	pair("verify-remount")
}

// mixedWinFsp is the WinFsp user-mode source for the Windows mount: a lab
// built DLL with its build manifest and source patch, a released DLL pinned by
// SHA256 that runs against the stock driver from the MSI, or a native package
// staged by nativeWinFspInputs. inputs maps C:\lab names to host paths.
type mixedWinFsp struct {
	inputs        map[string]string
	nativePackage string
	releaseSHA    string
}

func selectMixedWinFsp(labDLL, nativePackage, releaseDLL, releasePin string) (mixedWinFsp, error) {
	releaseDLL, releasePin, err := windowsReleaseDLL(releaseDLL, releasePin, labDLL, nativePackage)
	if err != nil {
		return mixedWinFsp{}, err
	}
	if releaseDLL != "" {
		return mixedWinFsp{inputs: map[string]string{"winfsp-x64.dll": releaseDLL}, releaseSHA: releasePin}, nil
	}
	if nativePackage != "" {
		if labDLL != "" {
			return mixedWinFsp{}, fmt.Errorf("native package selects a matched DLL; unset SEAWEEDFS_WINDOWS_WINFSP_DLL")
		}
		return mixedWinFsp{inputs: map[string]string{}, nativePackage: nativePackage}, nil
	}
	if labDLL == "" {
		return mixedWinFsp{}, fmt.Errorf("WinFsp DLL required: set SEAWEEDFS_WINDOWS_WINFSP_DLL, SEAWEEDFS_WINDOWS_WINFSP_RELEASE_DLL or SEAWEEDFS_WINDOWS_WINFSP_NATIVE_PACKAGE")
	}
	return mixedWinFsp{inputs: map[string]string{
		"winfsp-x64.dll":              labDLL,
		"winfsp-x64.dll.manifest.txt": labDLL + ".manifest.txt",
		"winfsp-x64.dll.source.patch": labDLL + ".source.patch",
	}}, nil
}

// verify checks the staged bytes against their evidence and returns the
// provenance lines for the selected source.
func (m mixedWinFsp) verify(artifacts map[string][]byte) (string, error) {
	if m.nativePackage != "" {
		return "", nil
	}
	if m.releaseSHA != "" {
		// Beside weed.exe, so the mount loads it before the installed DLL;
		// the stock MSI keeps providing the signed driver.
		dll, ok := artifacts["winfsp-x64.dll"]
		if !ok {
			return "", fmt.Errorf("release WinFsp DLL not staged")
		}
		if got := sha(dll); got != m.releaseSHA {
			return "", fmt.Errorf("release WinFsp DLL sha256 %s, pinned %s", got, m.releaseSHA)
		}
		return "release_winfsp_dll_sha256=" + m.releaseSHA + "\n", nil
	}
	if err := validateWinFspLabManifest(artifacts["winfsp-x64.dll.manifest.txt"], artifacts["winfsp-x64.dll"], artifacts["winfsp-x64.dll.source.patch"]); err != nil {
		return "", err
	}
	return "", nil
}

func TestMixedWinFspSelection(t *testing.T) {
	dll := []byte("released user-mode DLL")
	pin := sha(dll)
	release, err := selectMixedWinFsp("", "", "/in/winfsp-x64.dll", pin)
	if err != nil {
		t.Fatal(err)
	}
	if len(release.inputs) != 1 || release.inputs["winfsp-x64.dll"] != "/in/winfsp-x64.dll" {
		t.Fatalf("release DLL inputs %v", release.inputs)
	}
	provenance, err := release.verify(map[string][]byte{"winfsp-x64.dll": dll})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(provenance, "release_winfsp_dll_sha256="+pin+"\n") {
		t.Fatalf("release pin missing from provenance %q", provenance)
	}
	if _, err := release.verify(map[string][]byte{"winfsp-x64.dll": []byte("other DLL")}); err == nil {
		t.Fatal("accepted a release DLL that does not match its pin")
	}
	if _, err := release.verify(map[string][]byte{}); err == nil {
		t.Fatal("accepted a missing release DLL")
	}

	lab, err := selectMixedWinFsp("/lab/winfsp-x64.dll", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(lab.inputs) != 3 || lab.inputs["winfsp-x64.dll"] != "/lab/winfsp-x64.dll" ||
		lab.inputs["winfsp-x64.dll.manifest.txt"] != "/lab/winfsp-x64.dll.manifest.txt" ||
		lab.inputs["winfsp-x64.dll.source.patch"] != "/lab/winfsp-x64.dll.source.patch" {
		t.Fatalf("lab DLL inputs %v", lab.inputs)
	}
	manifest := fmt.Sprintf("source_revision=ddca7bd5481857a65ba552f643b8776fd070836f\nsource_mode=baseline\ndll_sha256=%s\nsource_patch_sha256=%s\nsource_date_epoch=1\nbuild_script_sha256=fixture\nbuild_shim_sha256=fixture\n", pin, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	if _, err := lab.verify(map[string][]byte{"winfsp-x64.dll": dll, "winfsp-x64.dll.manifest.txt": []byte(manifest), "winfsp-x64.dll.source.patch": nil}); err != nil {
		t.Fatalf("lab DLL with matching manifest rejected: %v", err)
	}
	if _, err := lab.verify(map[string][]byte{"winfsp-x64.dll": dll}); err == nil {
		t.Fatal("accepted a lab DLL without its manifest")
	}

	native, err := selectMixedWinFsp("", "/native", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(native.inputs) != 0 || native.nativePackage != "/native" {
		t.Fatalf("native package selection %+v", native)
	}

	for _, c := range []struct{ lab, native, release, pin string }{
		{"", "", "", ""},
		{"/lab/winfsp-x64.dll", "/native", "", ""},
		{"", "", "/in/winfsp-x64.dll", ""},
		{"", "", "/in/winfsp-x64.dll", "E1403"},
		{"", "", "", pin},
		{"/lab/winfsp-x64.dll", "", "", pin},
		{"/lab/winfsp-x64.dll", "", "/in/winfsp-x64.dll", pin},
		{"", "/native", "/in/winfsp-x64.dll", pin},
	} {
		if _, err := selectMixedWinFsp(c.lab, c.native, c.release, c.pin); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
}

func validateMixedGuestClock(output string, before, after time.Time) error {
	count := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "GUEST_UNIX_SECONDS:") {
			continue
		}
		count++
		seconds, err := strconv.ParseInt(strings.TrimPrefix(line, "GUEST_UNIX_SECONDS:"), 10, 64)
		if err != nil {
			return err
		}
		if seconds < before.Unix()-5 || seconds > after.Unix()+5 {
			return fmt.Errorf("guest clock skew: guest=%d controller=[%d,%d]", seconds, before.Unix(), after.Unix())
		}
	}
	if count != 1 {
		return fmt.Errorf("expected one guest clock marker, got %d", count)
	}
	return nil
}

func TestMixedGuestClock(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		output string
		valid  bool
	}{
		{"GUEST_UNIX_SECONDS:1700000000\r\n", true},
		{"GUEST_UNIX_SECONDS:1700025200\n", false},
		{"GUEST_UNIX_SECONDS:1699974800\n", false},
		{"", false},
		{"GUEST_UNIX_SECONDS:invalid", false},
		{"GUEST_UNIX_SECONDS:1700000000\nGUEST_UNIX_SECONDS:1700000000", false},
	} {
		if err := validateMixedGuestClock(tc.output, now, now.Add(time.Second)); (err == nil) != tc.valid {
			t.Fatalf("%q valid=%v error=%v", tc.output, tc.valid, err)
		}
	}
}
