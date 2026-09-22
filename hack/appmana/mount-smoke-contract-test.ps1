# Test the real LFS scenario's failure handling without mounting or starting weed.
$ErrorActionPreference = 'Stop'
$tokens = $null
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile(
    (Join-Path $PSScriptRoot 'mount-smoke.ps1'), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
foreach ($name in @('Assert', 'Invoke-GitLfsTempMetadataTest', 'Assert-WinFspModule')) {
    $definition = $ast.Find({ param($node)
        $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name
    }, $true)
    if (-not $definition) { throw "Missing function $name" }
    . ([scriptblock]::Create($definition.Extent.Text))
}

# Verify the process-module gate without launching a process or mounting data.
function Get-Process {
    param([int]$Id)
    if ($Id -ne 42) { throw 'Wrong process inspected' }
    if ($script:moduleQueryFails) { throw 'Injected module inspection failure' }
    return [pscustomobject]@{ Modules = $script:loadedModules }
}
$expectedDLL = Join-Path ([IO.Path]::GetTempPath()) 'lab-winfsp-x64.dll'
foreach ($case in @('correct', 'missing', 'fallback', 'duplicate', 'query-failure')) {
    $script:moduleQueryFails = $case -eq 'query-failure'
    $module = [pscustomobject]@{ ModuleName = 'winfsp-x64.dll'; FileName = $expectedDLL }
    $script:loadedModules = switch ($case) {
        'correct' { @($module) }
        'duplicate' { @($module, $module) }
        'fallback' { @([pscustomobject]@{ ModuleName = 'winfsp-x64.dll'; FileName = $expectedDLL + '.other' }) }
        default { @() }
    }
    $threw = $false
    try { Assert-WinFspModule 42 $expectedDLL } catch { $threw = $true }
    if ($threw -ne ($case -ne 'correct')) { throw "Incorrect DLL verification for $case" }
}
Remove-Item Function:Get-Process
Write-Host 'PASS: process DLL gate rejects missing, fallback, duplicate and uninspectable modules'

function git {
    $command = ($args | Select-Object -Skip 2) -join ' '
    $script:commands.Add($command)
    $global:LASTEXITCODE = 0
    if ($command -eq $script:failCommand) {
        $global:LASTEXITCODE = 1
        'injected native command failure'
    # PowerShell removes the -- delimiter when invoking a function mock.
    } elseif ($command -eq 'check-attr filter asset-1.lfs') {
        if ($script:wrongFilter) { 'asset-1.lfs: filter: unspecified' }
        else { 'asset-1.lfs: filter: lfs' }
    } elseif ($command -eq 'status --short --untracked-files=no') {
        1..$script:reportedAssets | ForEach-Object { " M asset-$_.lfs" }
    }
}

function mountvol.exe {
    if ($args.Count -ne 0) { throw 'mountvol must only list mappings' }
    if (-not $script:commands.Contains($script:failCommand)) { throw 'diagnostics ran before Git failure' }
    $script:mountDiagnostics++
    $global:LASTEXITCODE = 0
}
function fsutil.exe {
    if ($args.Count -ne 3 -or $args[0] -ne 'reparsepoint' -or $args[1] -ne 'query' -or $args[2] -ne $caseRoot) {
        throw 'fsutil must only query the test mount'
    }
    $script:mountDiagnostics++
    $global:LASTEXITCODE = 0
}

$root = Join-Path ([IO.Path]::GetTempPath()) ('weed-smoke-contract-' + [guid]::NewGuid())
[void][IO.Directory]::CreateDirectory($root)
try {
    $cases = @('init', 'config user.name AppMana mount smoke',
        'config user.email mount-smoke@appmana.invalid', 'lfs version',
        'lfs install --local', 'lfs track *.lfs', 'check-attr filter asset-1.lfs',
        'add .', 'commit -m seed lfs assets', 'status --short --untracked-files=no',
        'wrong-filter', 'missing-asset', 'success')
    foreach ($case in $cases) {
        $script:Trace = $false
        $script:mountDiagnostics = 0
        $script:failures = 0
        $script:GitIterations = 2
        $script:commands = [System.Collections.Generic.List[string]]::new()
        $script:failCommand = $case
        $script:wrongFilter = $case -eq 'wrong-filter'
        $script:reportedAssets = if ($case -eq 'missing-asset') { 31 } else { 32 }
        $caseRoot = Join-Path $root ([guid]::NewGuid().ToString())
        [void][IO.Directory]::CreateDirectory($caseRoot)
        Invoke-GitLfsTempMetadataTest $caseRoot
        $expectedDiagnostics = if ($case -in @('success', 'wrong-filter', 'missing-asset', 'check-attr filter asset-1.lfs', 'status --short --untracked-files=no')) { 0 } else { 2 }
        if ($script:mountDiagnostics -ne $expectedDiagnostics) { throw "Incorrect mount diagnostics for $case" }
        if ($case -eq 'success') {
            if ($script:failures -ne 0) { throw 'Healthy scenario rejected' }
            $statusCalls = @($script:commands | Where-Object { $_ -eq 'status --short --untracked-files=no' })
            if ($statusCalls.Count -ne $script:GitIterations) { throw 'Healthy scenario did not run every iteration' }
        } elseif ($script:failures -eq 0) {
            throw "False green for $case"
        }
        if ($case -notin @('success', 'wrong-filter', 'missing-asset') -and
            -not $script:commands.Contains($case)) {
            throw "Did not reach injected failure: $case"
        }
        if ($case -notin @('success', 'missing-asset', 'status --short --untracked-files=no') -and
            $script:commands.Contains('status --short --untracked-files=no')) {
            throw "Ran status after failed prerequisite: $case"
        }
    }
    Write-Host "PASS: all $($cases.Count) LFS harness contract cases"
    # Exercise the real finally block: an early exit in a scenario must not
    # turn a failed WPR flush into success, and cleanup must still run.
    $traceTry = $ast.Find({ param($node)
        $node -is [System.Management.Automation.Language.TryStatementAst] -and
        $null -ne $node.Finally -and $node.Finally.Extent.Text.Contains('$etwStopFailed')
    }, $true)
    if (-not $traceTry) { throw 'Missing ETW cleanup block' }
    $body = $traceTry.Finally.Extent.Text
    $cleanup = [scriptblock]::Create($body.Substring(1, $body.Length - 2))
    function wpr.exe { $global:LASTEXITCODE = $script:wprExit }
    $etwStarted = $true
    $logDir = $root
    $mount = $mount2 = $mountB = $null
    $server = [pscustomobject]@{ HasExited = $true }
    foreach ($script:wprExit in @(0, 1)) {
        $threw = $false
        try { . $cleanup 2>$null } catch { $threw = $true }
        if ($threw -ne ($script:wprExit -ne 0)) { throw "Incorrect WPR cleanup outcome for $script:wprExit" }
    }
    Write-Host 'PASS: ETW cleanup accepts successful flush and rejects failed flush'
} finally {
    # Only this script's newly created temporary fixtures, never mounted data.
    Remove-Item -LiteralPath $root -Recurse -Force
}
