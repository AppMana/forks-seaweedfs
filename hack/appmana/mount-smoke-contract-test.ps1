# Test the real LFS scenario's failure handling without mounting or starting weed.
$ErrorActionPreference = 'Stop'
$tokens = $null
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile(
    (Join-Path $PSScriptRoot 'mount-smoke.ps1'), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
foreach ($name in @('Assert', 'Invoke-GitLfsTempMetadataTest', 'Assert-WinFspModule', 'Start-Mount', 'Invoke-NativeMountedSuite')) {
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
    # Execute the actual command builders with process boundaries mocked.
    # This proves the mount policy and the independent filer oracle are wired
    # together; it is not filesystem qualification (that runs in real VMs).
    function Start-Process {
        param($FilePath, [switch]$PassThru, $WindowStyle, $ArgumentList,
              $RedirectStandardOutput, $RedirectStandardError)
        $script:capturedMountArgs = @($ArgumentList)
        [pscustomobject]@{ Id = 42 }
    }
    function Wait-PathExists { param($Path, $Timeout) return $true }
    function Invoke-NativeContract {
        $script:capturedNativeArgs = @($args)
        $global:LASTEXITCODE = 0
        $pattern = @($args | Where-Object { $_ -like '-test.run=*' })[0].Substring(10)
        foreach ($name in $pattern.Substring(2, $pattern.Length - 4).Split('|')) {
            "--- PASS: $([regex]::Unescape($name)) (0.01s)"
        }
    }
    $script:WeedExe = 'contract-weed'
    $script:WinFspTestExe = 'Invoke-NativeContract'
    $script:ExpectedWinFspDll = $expectedDLL
    $script:Trace = $script:TraceSummary = $false
    $script:Verbosity = 0
    $script:WinFspOptions = ''
    $logDir = $root
    foreach ($script:BasicPermissions in @($false, $true)) {
        $script:failures = 0
        $null = Start-Mount $root $root $root 'policy-contract'
        if ($script:capturedMountArgs -contains '-volumeLabel=SmokeTest') {
            throw 'Shared mount command uses fork-only volumeLabel flag unsupported by vanilla baseline'
        }
        if (($script:capturedMountArgs -contains '-winfspBasicPermissions') -ne $script:BasicPermissions) {
            throw 'Mount permission switch does not match policy'
        }
        Invoke-NativeMountedSuite $root -MetadataOnly
        if (($script:capturedNativeArgs -contains '-check-basic-permissions') -ne $script:BasicPermissions -or
            ($script:capturedNativeArgs -contains '-check-legacy-permissions') -eq $script:BasicPermissions -or
            $script:capturedNativeArgs -notcontains '-filer=127.0.0.1:8888') {
            throw 'Permission policy lost independent filer metadata oracle'
        }
        $selected = @($script:capturedNativeArgs | Where-Object { $_ -like '-test.run=*' })[0]
        if ($selected.Contains('TestWindowsBasicAccessDenial') -ne $script:BasicPermissions -or
            $selected.Contains('TestWindowsCreateSecurity') -ne $script:BasicPermissions -or
            $selected.Contains('TestWindowsLegacyPermissionCompatibility') -eq $script:BasicPermissions) {
            throw 'Wrong policy-specific test inventory'
        }
        Invoke-NativeMountedSuite $root -Phase 'write'
        $selected = @($script:capturedNativeArgs | Where-Object { $_ -like '-test.run=*' })[0]
        if ($selected.Contains('TestWindowsPermissionsPersistence') -ne $script:BasicPermissions -or
            -not $selected.Contains('TestWindowsAttributesPersistence') -or
            -not $selected.Contains('TestPersistence') -or $script:failures -ne 0) {
            throw 'Wrong persistence policy inventory or incomplete execution'
        }
        if ($selected.Contains('RepeatVerify')) { throw 'write phase must not run repeat verifiers' }
        Invoke-NativeMountedSuite $root -Phase 'verify'
        $selected = @($script:capturedNativeArgs | Where-Object { $_ -like '-test.run=*' })[0]
        if (-not $selected.Contains('TestWindowsAttributesPersistenceRepeatVerify') -or
            $selected.Contains('TestWindowsPermissionsPersistenceRepeatVerify') -ne $script:BasicPermissions) {
            throw 'verify phase lost real mounted repeatability coverage'
        }
        Invoke-NativeMountedSuite $root -CacheLifecycleOnly
        $selected = @($script:capturedNativeArgs | Where-Object { $_ -like '-test.run=*' })[0]
        if ($selected -ne '-test.run=^(TestCachedDeleteRecreate|TestDeleteOnClose)$' -or $script:failures -ne 0) {
            throw 'Cache lifecycle selection lost the focused immediate-recreate regressions'
        }
    }
    $script:BasicPermissions = $false
    Invoke-NativeMountedSuite $root -MetadataOnly -FilerEndpoint '192.0.2.10:8888' -FilerRootPrefix '/buckets/pvc-test/qualification-token/native' -LegacyPermissionUID 0 -LegacyPermissionGID 0 -LegacyPermissionMode 504
    if ($script:capturedNativeArgs -notcontains '-filer=192.0.2.10:8888' -or
        $script:capturedNativeArgs -notcontains '-filer-root=/buckets/pvc-test/qualification-token/native' -or
        $script:capturedNativeArgs -notcontains '-check-legacy-permissions' -or
        $script:capturedNativeArgs -notcontains '-legacy-permission-uid=0' -or
        $script:capturedNativeArgs -notcontains '-legacy-permission-gid=0' -or
        $script:capturedNativeArgs -notcontains '-legacy-permission-mode=504') {
        throw 'CSI native suite lost configured filer namespace or identity'
    }
    $selected = @($script:capturedNativeArgs | Where-Object { $_ -like '-test.run=*' })[0]
    if ($selected.Contains('TestMappedImportSlotCandidate')) {
        throw 'stock mounted suite selected candidate-only DLL test'
    }
    Invoke-NativeMountedSuite $root -Phase 'write' -FilerEndpoint '192.0.2.10:8888' -FilerRootPrefix '/buckets/pvc-test/qualification-token/native'
    if ($script:capturedNativeArgs -notcontains '-filer=192.0.2.10:8888' -or
        $script:capturedNativeArgs -notcontains '-filer-root=/buckets/pvc-test/qualification-token/native') {
        throw 'CSI persistence lost configured filer endpoint or namespace prefix'
    }
    Remove-Item Function:Start-Process, Function:Wait-PathExists, Function:Invoke-NativeContract
    Write-Host 'PASS: basic/legacy mount switches, filer oracles and explicit test inventories'
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
# All expected failures have been asserted, and cleanup has succeeded. Do not
# leak the deliberately injected WPR exit code into Actions' pwsh wrapper.
# Keep this outside finally: an unexpected assertion/cleanup failure must throw.
$global:LASTEXITCODE = 0
