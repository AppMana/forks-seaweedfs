# Advisory WinFsp compatibility sweep: mounts a weed filesystem and runs
# the upstream winfsp-tests suite against it with an exclusion list for
# semantics seaweedfs does not implement (reparse points, streams, EAs).
# Modeled on cgofuse's own CI usage of winfsp-tests.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$WeedExe,
    [Parameter(Mandatory = $true)][string]$TestsExe,
    [string]$WorkRoot = (Join-Path $env:RUNNER_TEMP 'sw-compat')
)

$ErrorActionPreference = 'Stop'

New-Item -ItemType Directory -Force -Path $WorkRoot | Out-Null
$dataDir = Join-Path $WorkRoot 'data'
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
$mnt = Join-Path $WorkRoot 'mnt'

$server = Start-Process -FilePath $WeedExe -PassThru -NoNewWindow -ArgumentList @(
    'server', '-ip=127.0.0.1', "-dir=$dataDir", '-master.volumeSizeLimitMB=64', '-volume.max=5', '-filer'
)
$mount = $null
try {
    $deadline = (Get-Date).AddSeconds(120)
    while ((Get-Date) -lt $deadline) {
        if ((Test-NetConnection -ComputerName 127.0.0.1 -Port 8888 -WarningAction SilentlyContinue).TcpTestSucceeded) { break }
        Start-Sleep -Milliseconds 500
    }
    $mount = Start-Process -FilePath $WeedExe -PassThru -NoNewWindow -ArgumentList @(
        'mount', '-filer=127.0.0.1:8888', "-dir=$mnt", "-cacheDir=$(Join-Path $WorkRoot 'cache')"
    )
    $deadline = (Get-Date).AddSeconds(60)
    while ((Get-Date) -lt $deadline -and -not (Test-Path $mnt)) { Start-Sleep -Milliseconds 250 }
    if (-not (Test-Path $mnt)) { throw 'mount never appeared' }

    Push-Location $mnt
    try {
        # Exclusions follow cgofuse CI plus seaweedfs specifics:
        # no reparse/stream/EA support, no fileattr beyond the basics.
        & $TestsExe --fuse-external --resilient --case-insensitive-cmp `
            +* `
            -reparse* -stream* -ea* `
            -create_fileattr_test -create_readonlydir_test `
            -getfileattr_test -setfileinfo_test -delete_access_test `
            -rename_flipflop_test -rename_mmap_test -exec* -oplock*
        $code = $LASTEXITCODE
        Write-Host "winfsp-tests exit code: $code (advisory)"
        exit $code
    } finally {
        Pop-Location
    }
} finally {
    if ($mount -and -not $mount.HasExited) { & taskkill /PID $mount.Id | Out-Null; Start-Sleep 2 }
    if (-not $server.HasExited) { $server.Kill() }
}
