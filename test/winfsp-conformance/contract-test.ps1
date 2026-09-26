# Exercise the real conformance driver with a synthetic process boundary.
# This tests evidence validation, not Windows filesystem behavior.
$ErrorActionPreference = 'Stop'
$root = Join-Path ([IO.Path]::GetTempPath()) ('conformance-contract-' + [guid]::NewGuid())
[void][IO.Directory]::CreateDirectory($root)
$savedTemp = $env:TEMP
$savedCase = $env:WINFSP_CONTRACT_CASE
try {
    $env:TEMP = $root
    $fixture = Join-Path $root 'fixture.ps1'
    @'
$global:LASTEXITCODE = 0
if ($args -contains '--list') {
    if ($env:WINFSP_CONTRACT_CASE -eq 'failed-list') { $global:LASTEXITCODE = 1 }
    if ($env:WINFSP_CONTRACT_CASE -ne 'empty-list') { 'create_test' }
    return
}
switch ($env:WINFSP_CONTRACT_CASE) {
    'empty' { }
    'wrong-name' { 'other_test........ OK 0.01s' }
    'duplicate' { 'create_test........ OK 0.01s'; 'create_test........ OK 0.01s' }
    'failure' { 'create_test........ KO 0.01s' }
    'mixed' { 'create_test........ OK 0.01s'; 'other_test........ KO 0.01s' }
    'exit-failure' { 'create_test........ OK 0.01s'; $global:LASTEXITCODE = 1 }
    default { 'create_test........ OK 0.01s' }
}
'@ | Set-Content -LiteralPath $fixture
    $known = Join-Path $root 'known.txt'
    'create_test' | Set-Content -LiteralPath $known
    $driver = Join-Path $PSScriptRoot 'run.ps1'
    $shell = (Get-Process -Id $PID).Path
    $errorsFound = @()
    foreach ($case in @('success', 'empty', 'wrong-name', 'duplicate', 'failure', 'mixed', 'exit-failure', 'empty-list', 'failed-list', 'all-excluded')) {
        $env:WINFSP_CONTRACT_CASE = $case
        $mount = Join-Path $root $case
        [void][IO.Directory]::CreateDirectory($mount)
        $arguments = @('-NoProfile', '-File', $driver, '-MountPoint', $mount, '-KnownFailures', $known, '-WinFspTestsExe', $fixture)
        if ($case -ne 'all-excluded') { $arguments += '-IncludeKnownFailures' }
        $output = & $shell @arguments 2>&1
        $accepted = $LASTEXITCODE -eq 0
        if ($accepted -ne ($case -eq 'success')) {
            $errorsFound += $case
            Write-Host "Incorrect evidence acceptance: $case (exit $LASTEXITCODE)"
            $output | ForEach-Object { Write-Host $_ }
        }
    }
    if ($errorsFound.Count) { throw "Conformance evidence failures: $($errorsFound -join ', ')" }
    Write-Host 'PASS: conformance driver rejects missing, conflicting and incomplete case evidence'
} finally {
    $env:TEMP = $savedTemp
    $env:WINFSP_CONTRACT_CASE = $savedCase
    Remove-Item -LiteralPath $root -Recurse -Force
}
