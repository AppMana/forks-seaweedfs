# Benchmarks a weed.exe WinFsp mount in network-FS mode (MUP in the
# path, mirroring CSI production use). Run once per binary/option-set;
# emits one markdown table row per run via -Label.
#
# Measures: seq write, warm seq read (4MB buffers), 4KB buffered read,
# cold + repeat directory listing over 200 files, small-file create rate.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$WeedExe,
    [Parameter(Mandatory = $true)][string]$Label,
    [string]$WinfspOptions = '',
    [string]$WorkRoot = (Join-Path $env:RUNNER_TEMP "sw-bench-$([guid]::NewGuid().ToString('N').Substring(0,8))"),
    [int]$SeqMB = 256,
    [int]$SmallFiles = 200,
    [string]$SummaryFile = $env:GITHUB_STEP_SUMMARY
)

$ErrorActionPreference = 'Stop'

function Wait-Tcp([int]$port, [int]$timeoutSec = 120) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        if ((Test-NetConnection -ComputerName 127.0.0.1 -Port $port -WarningAction SilentlyContinue).TcpTestSucceeded) { return $true }
        Start-Sleep -Milliseconds 500
    }
    return $false
}

New-Item -ItemType Directory -Force -Path $WorkRoot | Out-Null
$logDir = Join-Path $WorkRoot 'logs'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

$dataDir = Join-Path $WorkRoot 'data'
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
$server = Start-Process -FilePath $WeedExe -PassThru -WindowStyle Hidden -ArgumentList @(
    "-logdir=$logDir", 'server', '-ip=127.0.0.1', "-dir=$dataDir",
    '-master.volumeSizeLimitMB=64', '-volume.max=100', '-filer'
)
foreach ($port in 9333, 8080, 8888) {
    if (-not (Wait-Tcp $port)) { throw "weed server port $port never came up" }
}

try {
    $mnt = Join-Path $WorkRoot 'mnt'
    $cache = Join-Path $WorkRoot 'cache'
    New-Item -ItemType Directory -Force -Path $cache | Out-Null
    $mountArgs = @(
        "-logdir=$logDir", 'mount', '-filer=127.0.0.1:8888', "-dir=$mnt",
        "-cacheDir=$cache", '-cacheCapacityMB=1024',
        "-filer.path=/bench-$Label"
    )
    if ($WinfspOptions) { $mountArgs += "-winfspOptions=$WinfspOptions" }
    # network-FS mode: UNC + MUP in the IO path like production
    $env:WEED_WINFSP_VOLUME_PREFIX = '\seaweedfs-bench'
    $mount = Start-Process -FilePath $WeedExe -PassThru -WindowStyle Hidden -ArgumentList $mountArgs
    $deadline = (Get-Date).AddSeconds(60)
    while ((Get-Date) -lt $deadline -and -not (Test-Path $mnt)) { Start-Sleep -Milliseconds 250 }
    if (-not (Test-Path $mnt)) { throw "mount $mnt never appeared (see $logDir)" }

    # --- seq write
    $chunk = New-Object byte[] 4194304; (New-Object System.Random 1).NextBytes($chunk)
    $tW = Measure-Command {
        $fs = [IO.File]::Create("$mnt\seq.bin")
        1..($SeqMB / 4) | ForEach-Object { $fs.Write($chunk, 0, $chunk.Length) }
        $fs.Flush(); $fs.Close()
    }
    $seqW = [math]::Round($SeqMB / $tW.TotalSeconds, 1)

    # --- warm seq read, 4MB buffers (read twice, report 2nd: kernel-cache fed)
    $buf = New-Object byte[] 4194304
    $read = { $fs = [IO.File]::OpenRead("$mnt\seq.bin"); while (($fs.Read($buf, 0, $buf.Length)) -gt 0) {}; $fs.Close() }
    & $read
    $tR = Measure-Command { & $read }
    $seqR = [math]::Round($SeqMB / $tR.TotalSeconds, 1)

    # --- 4KB buffered read over the first 64MB (warm)
    $small = New-Object byte[] 4096
    $tK = Measure-Command {
        $fs = [IO.File]::OpenRead("$mnt\seq.bin")
        $total = 0
        while ($total -lt 64MB -and ($n = $fs.Read($small, 0, $small.Length)) -gt 0) { $total += $n }
        $fs.Close()
    }
    $kbR = [math]::Round(64 / $tK.TotalSeconds, 1)

    # --- small files: create rate + cold/repeat listing
    New-Item -ItemType Directory -Path "$mnt\small" | Out-Null
    $payload = New-Object byte[] 4096; (New-Object System.Random 2).NextBytes($payload)
    $tC = Measure-Command {
        1..$SmallFiles | ForEach-Object { [IO.File]::WriteAllBytes(("$mnt\small\f{0:D4}.bin" -f $_), $payload) }
    }
    $createPs = [math]::Round($SmallFiles / $tC.TotalSeconds, 1)
    $tL1 = Measure-Command { [void](Get-ChildItem "$mnt\small") }
    $tL2 = Measure-Command { [void](Get-ChildItem "$mnt\small") }
    $listCold = [math]::Round($tL1.TotalMilliseconds, 0)
    $listWarm = [math]::Round($tL2.TotalMilliseconds, 0)

    $row = "| $Label | $seqW | $seqR | $kbR | $createPs | $listCold | $listWarm |"
    Write-Host "RESULT $row"
    if ($SummaryFile) {
        if (-not (Select-String -Path $SummaryFile -Pattern 'seq write MB/s' -Quiet -ErrorAction SilentlyContinue)) {
            Add-Content $SummaryFile "| run | seq write MB/s | warm read MB/s | 4KB read MB/s | create files/s | list cold ms | list warm ms |"
            Add-Content $SummaryFile "|---|---|---|---|---|---|---|"
        }
        Add-Content $SummaryFile $row
    }

    # graceful stop (console close via taskkill, fall back to kill)
    & taskkill /PID $mount.Id 2>$null | Out-Null
    if (-not $mount.WaitForExit(10000)) { $mount.Kill(); $mount.WaitForExit(5000) | Out-Null }
    $deadline = (Get-Date).AddSeconds(15)
    while ((Get-Date) -lt $deadline -and (Test-Path $mnt)) { Start-Sleep -Milliseconds 250 }
    if (Test-Path $mnt) { Remove-Item $mnt -Force -Recurse:$false }
} finally {
    Remove-Item Env:\WEED_WINFSP_VOLUME_PREFIX -ErrorAction SilentlyContinue
    if ($server -and -not $server.HasExited) { $server.Kill() }
}
