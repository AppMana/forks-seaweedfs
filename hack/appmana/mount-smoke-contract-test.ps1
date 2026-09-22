# Test the real LFS scenario's failure handling without mounting or starting weed.
$ErrorActionPreference = 'Stop'
$tokens = $null
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile(
    (Join-Path $PSScriptRoot 'mount-smoke.ps1'), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
foreach ($name in @('Assert', 'Invoke-GitLfsTempMetadataTest')) {
    $definition = $ast.Find({ param($node)
        $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name
    }, $true)
    if (-not $definition) { throw "Missing function $name" }
    . ([scriptblock]::Create($definition.Extent.Text))
}

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

$root = Join-Path ([IO.Path]::GetTempPath()) ('weed-smoke-contract-' + [guid]::NewGuid())
[void][IO.Directory]::CreateDirectory($root)
try {
    $cases = @('init', 'config user.name AppMana mount smoke',
        'config user.email mount-smoke@appmana.invalid', 'lfs version',
        'lfs install --local', 'lfs track *.lfs', 'check-attr filter asset-1.lfs',
        'add .', 'commit -m seed lfs assets', 'status --short --untracked-files=no',
        'wrong-filter', 'missing-asset', 'success')
    foreach ($case in $cases) {
        $script:failures = 0
        $script:GitIterations = 2
        $script:commands = [System.Collections.Generic.List[string]]::new()
        $script:failCommand = $case
        $script:wrongFilter = $case -eq 'wrong-filter'
        $script:reportedAssets = if ($case -eq 'missing-asset') { 31 } else { 32 }
        $caseRoot = Join-Path $root ([guid]::NewGuid().ToString())
        [void][IO.Directory]::CreateDirectory($caseRoot)
        Invoke-GitLfsTempMetadataTest $caseRoot
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
} finally {
    # Only this script's newly created temporary fixtures, never mounted data.
    Remove-Item -LiteralPath $root -Recurse -Force
}
