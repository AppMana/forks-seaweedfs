# Host-independent command contract; not a substitute for an actual MSVC build.
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'build-winfsp-msvc.ps1')
$actual = @(Get-WinFspMSBuildArguments 'C:\lab\winfsp' 'C:\lab\build-candidate' '14.29.30133' 'v142' '10.0.19041.0')
foreach ($required in @(
    'C:\lab\winfsp\build\VStudio\winfsp_dll.vcxproj', '/t:Build', '/m:1', '/nr:false',
    '/p:Configuration=Release', '/p:Platform=x64', '/p:PlatformToolset=v142',
    '/p:VCToolsVersion=14.29.30133', '/p:WindowsTargetPlatformVersion=10.0.19041.0',
    '/p:MyTargetPlatformVersion=10.0.19041.0', '/p:MyBuildNumber=25156',
    '/p:MyCopyright=2015-2025 Bill Zissimopoulos', '/p:MyGitRevision=ddca7bd',
    '/p:OutDir=C:\lab\build-candidate\', '/p:IntDir=C:\lab\build-candidate\obj\',
    '/p:UserRootDir=C:\lab\build-candidate\empty-user-props\', '/bl:C:\lab\build-candidate\build.binlog'
)) {
    if (@($actual | Where-Object { $_ -ceq $required }).Count -ne 1) { throw "Missing/duplicate argument: $required" }
}
if (@($actual | Where-Object { $_ -match '\.sln$|\.sys$|/t:(Rebuild|Clean|Install|Sign)' }).Count) {
    throw 'Build must target only the user-mode DLL project'
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
