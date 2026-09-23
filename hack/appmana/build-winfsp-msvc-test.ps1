# Host-independent command contract; not a substitute for an actual MSVC build.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'build-winfsp-msvc.ps1')
$actual = @(Get-WinFspMSBuildArguments 'C:\lab\winfsp' 'C:\lab\build-candidate' '14.29.30133' 'v142' '10.0.19041.0')
$expected = @(
    'C:\lab\winfsp\build\VStudio\winfsp_dll.vcxproj', '/t:Build', '/m:1', '/nologo', '/nr:false',
    '/p:Configuration=Release', '/p:Platform=x64', '/p:PlatformToolset=v142',
    '/p:VCToolsVersion=14.29.30133', '/p:MyTargetPlatformVersion=10.0.19041.0',
    '/p:WindowsTargetPlatformVersion=10.0.19041.0', '/p:MyBuildNumber=25156',
    '/p:MyCopyright=2015-2025 Bill Zissimopoulos', '/p:MyGitRevision=ddca7bd',
    '/p:SolutionDir=C:\lab\winfsp\', '/p:OutDir=C:\lab\build-candidate\',
    '/p:IntDir=C:\lab\build-candidate\obj\',
    '/p:UserRootDir=C:\lab\build-candidate\empty-user-props\', '/bl:C:\lab\build-candidate\build.binlog',
    '/v:diagnostic'
)
function Assert-ExactBuildArguments {
    param([string[]]$Observed)
    if ($Observed.Count -ne $expected.Count) { throw 'Unexpected build argument count' }
    for ($i = 0; $i -lt $expected.Count; $i++) {
        if ($Observed[$i] -cne $expected[$i]) { throw "Unexpected build argument at index $i" }
    }
}
Assert-ExactBuildArguments -Observed $actual
foreach ($mutation in @('extra target', 'combined target', 'extra project', 'missing diagnostic', 'duplicate target')) {
    $changed = @($actual)
    switch ($mutation) {
        'extra target' { $changed += '/t:Build;Sign' }
        'combined target' { $changed[1] = '/t:Build;Sign' }
        'extra project' { $changed += 'C:\lab\winfsp\build\VStudio\winfsp.sln' }
        'missing diagnostic' { $changed = @($changed | Where-Object { $_ -ne '/v:diagnostic' }) }
        'duplicate target' { $changed += '/t:Build' }
    }
    $rejected = $false
    try { Assert-ExactBuildArguments -Observed $changed } catch { $rejected = $true }
    if (-not $rejected) { throw "Build argument mutation accepted: $mutation" }
}
foreach ($case in @(
    @('C:\space path', 'C:\out', '14.29.30133', 'v142', '10.0.19041.0'),
    @('C:\src', 'C:\out&whoami', '14.29.30133', 'v142', '10.0.19041.0'),
    @('C:\src', 'C:\out', 'latest', 'v142', '10.0.19041.0'),
    @('C:\src', 'C:\out', '14.29.30133', 'v142;Other=1', '10.0.19041.0'),
    @('C:\src', 'C:\out', '14.29.30133', 'v142', 'latest')
)) {
    $rejected = $false
    try { Get-WinFspMSBuildArguments @case | Out-Null } catch { $rejected = $true }
    if (-not $rejected) { throw "Unsafe build arguments accepted: $case" }
}
Write-Host 'PASS: scoped MSVC DLL build arguments, pinned versions, and unsafe-input rejection'
function git {
    $script:gitObserved = @($args)
    $global:LASTEXITCODE = $script:gitExit
    'fixture-result'
}
try {
    $script:gitExit = 0
    $result = Invoke-WinFspGit -GitArguments @('-C', 'C:\lab\src', 'rev-parse', 'HEAD')
    if ($result -ne 'fixture-result' -or ($script:gitObserved -join '|') -ne '-C|C:\lab\src|rev-parse|HEAD') {
        throw 'Git argument forwarding failed'
    }
    $script:gitExit = 7
    $rejected = $false
    try { Invoke-WinFspGit -GitArguments @('rev-parse', 'HEAD') | Out-Null } catch { $rejected = $true }
    if (-not $rejected) { throw 'Git error was swallowed' }
} finally { Remove-Item Function:git }
Write-Host 'PASS: Git argument forwarding and native failure propagation'
