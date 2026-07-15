# Runs the WinFsp mount regression under Application Verifier's Cuzz layer.
# Cuzz perturbs Win32 synchronization calls and a fixed RandomSeed makes a
# failing schedule substantially easier to reproduce.
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$WeedExe,
    [ValidateRange(1, 4)][int]$FuzzingLevel = 4,
    [ValidateRange(0, [int]::MaxValue)][int]$RandomSeed = 1,
    [ValidateSet('All', 'NamespaceCoherence', 'GitAtomicRename', 'GitAtomicRenamePrimed')][string]$TestCase = 'GitAtomicRenamePrimed',
    [switch]$Trace,
    [switch]$TraceSummary,
    [string]$WorkRoot
)

$ErrorActionPreference = 'Stop'
$tempRoot = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { $env:TEMP }
if (-not $WorkRoot) { $WorkRoot = Join-Path $tempRoot "sw-cuzz-$RandomSeed" }
$appVerifier = Get-Command appverif.exe -ErrorAction Stop
$target = Split-Path -Leaf $WeedExe
$mountSmoke = Join-Path $PSScriptRoot 'mount-smoke.ps1'
$pwsh = (Get-Process -Id $PID).Path
$serverWeedExe = Join-Path (Split-Path -Parent $WeedExe) 'weed-server.exe'
Copy-Item -Force $WeedExe $serverWeedExe

Write-Host "== enabling AppVerifier Cuzz for $target (level=$FuzzingLevel seed=$RandomSeed)"
$appVerifierArgs = @(
    '-enable', 'Cuzz', '-for', $target, '-with',
    "Cuzz.FuzzingLevel=$FuzzingLevel", "Cuzz.RandomSeed=$RandomSeed"
)
& $appVerifier.Source @appVerifierArgs
if ($LASTEXITCODE -ne 0) {
    throw "appverif failed to enable Cuzz (exit $LASTEXITCODE)"
}

try {
    & $appVerifier.Source -query Cuzz -for $target
    if ($LASTEXITCODE -ne 0) {
        throw "appverif failed to query Cuzz settings (exit $LASTEXITCODE)"
    }

    $smokeArgs = @(
        '-NoLogo', '-NoProfile', '-File', $mountSmoke,
        '-WeedExe', $WeedExe, '-ServerWeedExe', $serverWeedExe,
        '-WorkRoot', $WorkRoot, '-TestCase', $TestCase
    )
    if ($Trace) { $smokeArgs += '-Trace' }
    if ($TraceSummary) { $smokeArgs += '-TraceSummary' }
    & $pwsh @smokeArgs
    $testExitCode = $LASTEXITCODE
} finally {
    Write-Host "== disabling AppVerifier for $target"
    & $appVerifier.Source -delete settings -for $target
    Remove-Item -Force $serverWeedExe -ErrorAction SilentlyContinue
}

exit $testExitCode
