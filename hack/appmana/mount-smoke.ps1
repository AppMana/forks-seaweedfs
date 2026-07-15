# End-to-end smoke test for the WinFsp weed mount on a Windows host.
# Starts a single-node weed server (master+volume+filer), mounts it via
# WinFsp, exercises the read/write/rename/delete/persistence paths, and
# fails loudly on the first broken assertion.
#
# The mount processes are started in their own console so that
# Stop-Mount can deliver a real CTRL_C console event (the same graceful
# shutdown the CSI mount supervisor uses) without killing this script.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$WeedExe,
    [string]$WorkRoot = (Join-Path $env:RUNNER_TEMP 'sw-smoke'),
    [int]$LargeFileMB = 100
)

$ErrorActionPreference = 'Stop'
$failures = 0

function Assert([bool]$cond, [string]$what) {
    if ($cond) {
        Write-Host "PASS: $what"
    } else {
        Write-Host "FAIL: $what" -ForegroundColor Red
        $script:failures++
    }
}

function Wait-Tcp([int]$port, [int]$timeoutSec = 120) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        if ((Test-NetConnection -ComputerName 127.0.0.1 -Port $port -WarningAction SilentlyContinue).TcpTestSucceeded) {
            return $true
        }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

function Wait-PathExists([string]$p, [int]$timeoutSec = 60) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        if (Test-Path $p) { return $true }
        Start-Sleep -Milliseconds 250
    }
    return $false
}

function Start-Mount([string]$mnt, [string]$cacheDir, [string]$logDir, [string]$name) {
    New-Item -ItemType Directory -Force -Path (Join-Path $logDir $name) | Out-Null
    # New console (no -NoNewWindow): required so Stop-Mount's CTRL_C
    # event reaches only the mount process. Logs go to -logdir.
    $proc = Start-Process -FilePath $WeedExe -PassThru -WindowStyle Hidden -ArgumentList @(
        "-logdir=$(Join-Path $logDir $name)", 'mount',
        '-filer=127.0.0.1:8888',
        "-dir=$mnt",
        "-cacheDir=$cacheDir",
        '-cacheCapacityMB=512',
        '-volumeLabel=SmokeTest'
    )
    if (-not (Wait-PathExists $mnt 60)) {
        throw "mount point $mnt did not appear (see $logDir\$name)"
    }
    return $proc
}

# Sends CTRL_C to the target's console from a throwaway helper process
# (the helper detaches from our console first, so only the mount sees
# the event). Returns $true when the process exited gracefully.
function Stop-MountGracefully($proc, [int]$timeoutSec = 15) {
    $helperScript = @'
param([int]$TargetPid)
Add-Type -Namespace Win32 -Name Console -MemberDefinition @"
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool FreeConsole();
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool AttachConsole(uint pid);
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool SetConsoleCtrlHandler(IntPtr handler, bool add);
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool GenerateConsoleCtrlEvent(uint ctrlEvent, uint pgid);
"@
[Win32.Console]::FreeConsole() | Out-Null
if (-not [Win32.Console]::AttachConsole([uint32]$TargetPid)) { exit 2 }
[Win32.Console]::SetConsoleCtrlHandler([IntPtr]::Zero, $true) | Out-Null
[Win32.Console]::GenerateConsoleCtrlEvent(0, 0) | Out-Null
exit 0
'@
    $helperPath = Join-Path $env:TEMP "send-ctrlc-$($proc.Id).ps1"
    Set-Content -Path $helperPath -Value $helperScript
    Start-Process -FilePath 'powershell.exe' -Wait -WindowStyle Hidden -ArgumentList @(
        '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $helperPath, '-TargetPid', $proc.Id
    )
    Remove-Item $helperPath -ErrorAction SilentlyContinue
    return $proc.WaitForExit($timeoutSec * 1000)
}

function Stop-Mount($proc, [string]$mnt) {
    $graceful = Stop-MountGracefully $proc
    Assert $graceful 'mount exits gracefully on console ctrl event'
    if (-not $graceful) {
        $proc.Kill()
        $proc.WaitForExit(5000) | Out-Null
    }
    $deadline = (Get-Date).AddSeconds(15)
    while ((Get-Date) -lt $deadline -and (Test-Path $mnt)) { Start-Sleep -Milliseconds 250 }
    if (Test-Path $mnt) {
        # dangling reparse point after a hard kill: must be removable
        Remove-Item $mnt -Force -Recurse:$false
        Assert (-not (Test-Path $mnt)) 'dangling mount point removable after kill'
    }
}

New-Item -ItemType Directory -Force -Path $WorkRoot | Out-Null
$logDir = Join-Path $env:RUNNER_TEMP 'sw-logs'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
$dataDir = Join-Path $WorkRoot 'data'
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
$cacheDir = Join-Path $WorkRoot 'cache'
New-Item -ItemType Directory -Force -Path $cacheDir | Out-Null
$mnt = Join-Path $WorkRoot 'mnt'   # must NOT pre-exist; WinFsp creates it

Write-Host '== starting weed server'
New-Item -ItemType Directory -Force -Path (Join-Path $logDir 'server') | Out-Null
$server = Start-Process -FilePath $WeedExe -PassThru -WindowStyle Hidden -ArgumentList @(
    "-logdir=$(Join-Path $logDir 'server')", 'server', '-ip=127.0.0.1',
    "-dir=$dataDir",
    '-master.volumeSizeLimitMB=64',
    '-volume.max=5',
    '-filer'
)
try {
    foreach ($port in 9333, 8080, 8888) {
        if (-not (Wait-Tcp $port)) { throw "weed server port $port never came up" }
    }
    Write-Host '== server up; mounting'
    $mount = Start-Mount $mnt $cacheDir $logDir 'mount1'

    # --- basic write/read
    Set-Content -Path "$mnt\hello.txt" -Value 'seaweedfs on windows' -NoNewline
    Assert ((Get-Content "$mnt\hello.txt" -Raw) -eq 'seaweedfs on windows') 'write/read round trip'

    # --- stat
    $item = Get-Item "$mnt\hello.txt"
    Assert ($item.Length -eq 20) "stat size ($($item.Length))"
    Assert ($item.LastWriteTime -gt (Get-Date).AddMinutes(-10)) 'stat mtime sane'

    # --- mkdir/list
    New-Item -ItemType Directory -Path "$mnt\subdir" | Out-Null
    Set-Content -Path "$mnt\subdir\nested.txt" -Value 'nested'
    $names = (Get-ChildItem $mnt | Select-Object -ExpandProperty Name) -join ','
    Assert ($names -match 'hello.txt' -and $names -match 'subdir') "directory listing ($names)"
    Assert ((Get-ChildItem "$mnt\subdir").Count -eq 1) 'nested directory listing'

    # --- rename (including over an existing file)
    Rename-Item "$mnt\hello.txt" 'renamed.txt'
    Assert (-not (Test-Path "$mnt\hello.txt")) 'rename removes old name'
    Assert ((Get-Content "$mnt\renamed.txt" -Raw) -eq 'seaweedfs on windows') 'rename keeps content'
    Set-Content -Path "$mnt\victim.txt" -Value 'overwrite me'
    Move-Item "$mnt\renamed.txt" "$mnt\victim.txt" -Force
    Assert ((Get-Content "$mnt\victim.txt" -Raw) -eq 'seaweedfs on windows') 'rename over existing file'

    # --- Git performs several config.lock -> config atomic replacements during
    # one init. A stale source name after the first rename makes the second
    # exclusive config.lock create fail with ERROR_FILE_EXISTS.
    $gitRepo = Join-Path $mnt 'git-atomic-rename'
    New-Item -ItemType Directory -Path $gitRepo | Out-Null
    $gitOutput = (& git -C $gitRepo init 2>&1 | Out-String)
    $gitExitCode = $LASTEXITCODE
    if ($gitExitCode -ne 0) { Write-Host $gitOutput -ForegroundColor Red }
    Assert ($gitExitCode -eq 0) 'git init supports repeated config.lock atomic replacements'
    Assert ((Test-Path (Join-Path $gitRepo '.git\config'))) 'git init publishes config'
    Assert (-not (Test-Path (Join-Path $gitRepo '.git\config.lock'))) 'git init leaves no stale config.lock'

    # --- append
    Add-Content -Path "$mnt\append.txt" -Value 'line1'
    Add-Content -Path "$mnt\append.txt" -Value 'line2'
    Assert ((Get-Content "$mnt\append.txt").Count -eq 2) 'append twice yields two lines'

    # --- large file (multi-chunk) hash round trip
    $rand = New-Object byte[] ($LargeFileMB * 1MB)
    (New-Object System.Random 42).NextBytes($rand)
    $src = Join-Path $WorkRoot 'large.bin'
    [IO.File]::WriteAllBytes($src, $rand)
    Copy-Item $src "$mnt\large.bin"
    $h1 = (Get-FileHash $src -Algorithm SHA256).Hash
    $h2 = (Get-FileHash "$mnt\large.bin" -Algorithm SHA256).Hash
    Assert ($h1 -eq $h2) "large file ($LargeFileMB MB) hash equality"

    # --- truncate
    $fs = [IO.File]::Open("$mnt\large.bin", 'Open', 'ReadWrite')
    $fs.SetLength(1MB); $fs.Close()
    Assert ((Get-Item "$mnt\large.bin").Length -eq 1MB) 'truncate to 1MB'

    # --- interleaved writers on two open handles
    $w1 = [IO.StreamWriter]::new("$mnt\writer1.txt")
    $w2 = [IO.StreamWriter]::new("$mnt\writer2.txt")
    1..200 | ForEach-Object {
        $w1.WriteLine("row $_")
        $w2.WriteLine("row $_")
    }
    $w1.Close(); $w2.Close()
    Assert ((Get-Content "$mnt\writer1.txt").Count -eq 200) 'writer1 line count'
    Assert ((Get-Content "$mnt\writer2.txt").Count -eq 200) 'writer2 line count'

    # --- delete
    Remove-Item "$mnt\victim.txt"
    Assert (-not (Test-Path "$mnt\victim.txt")) 'delete file'
    Remove-Item "$mnt\subdir" -Recurse
    Assert (-not (Test-Path "$mnt\subdir")) 'delete directory recursively'

    Write-Host '== unmounting'
    Stop-Mount $mount $mnt
    Assert (-not (Test-Path $mnt)) 'mount point gone after unmount'

    # --- persistence across remount proves data reached the filer
    Write-Host '== remounting for persistence check'
    $cache2 = Join-Path $WorkRoot 'cache2'  # fresh cache: no local masking
    New-Item -ItemType Directory -Force -Path $cache2 | Out-Null
    $mount2 = Start-Mount $mnt $cache2 $logDir 'mount2'
    Assert ((Get-Content "$mnt\append.txt").Count -eq 2) 'append.txt survives remount'
    Assert ((Get-Item "$mnt\large.bin").Length -eq 1MB) 'large.bin truncation survives remount'
    Assert (-not (Test-Path "$mnt\victim.txt")) 'deleted file stays deleted after remount'

    # --- close-to-open consistency across two concurrent mounts:
    # with FileInfoTimeout=-1 the kernel caches data, so a change made
    # through a second mount must become visible to a fresh open here
    # (filer-event Notify invalidation + close-to-open purge).
    Write-Host '== cross-mount close-to-open check'
    $mntB = Join-Path $WorkRoot 'mntB'
    $cacheB = Join-Path $WorkRoot 'cacheB'
    New-Item -ItemType Directory -Force -Path $cacheB | Out-Null
    $mountB = Start-Mount $mntB $cacheB $logDir 'mountB'
    Set-Content -Path "$mnt\c2o.txt" -Value 'version-one' -NoNewline
    $deadline = (Get-Date).AddSeconds(10); $seen = $false
    while ((Get-Date) -lt $deadline) {
        if ((Test-Path "$mntB\c2o.txt") -and ((Get-Content "$mntB\c2o.txt" -Raw -ErrorAction SilentlyContinue) -eq 'version-one')) { $seen = $true; break }
        Start-Sleep -Milliseconds 500
    }
    Assert $seen 'mount-A write visible on mount-B within 10s'
    Set-Content -Path "$mntB\c2o.txt" -Value 'version-two!' -NoNewline
    $deadline = (Get-Date).AddSeconds(10); $seen = $false
    while ((Get-Date) -lt $deadline) {
        if ((Get-Content "$mnt\c2o.txt" -Raw -ErrorAction SilentlyContinue) -eq 'version-two!') { $seen = $true; break }
        Start-Sleep -Milliseconds 500
    }
    Assert $seen 'mount-B overwrite visible on mount-A within 10s (cache invalidation)'
    Stop-Mount $mountB $mntB

    Stop-Mount $mount2 $mnt
} finally {
    if (-not $server.HasExited) { $server.Kill() }
}

if ($failures -gt 0) {
    Write-Host "$failures assertion(s) failed" -ForegroundColor Red
    exit 1
}
Write-Host 'ALL MOUNT SMOKE TESTS PASSED'
