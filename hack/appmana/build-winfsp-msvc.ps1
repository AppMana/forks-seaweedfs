# Builds an unsigned lab DLL only. Does not install/register it or build a driver.
[CmdletBinding()]
param(
    [string]$SourceDirectory,
    [string]$OutputDirectory,
    [string]$MSBuildPath,
    [string]$VCToolsVersion,
    [string]$PlatformToolset = 'v142',
    [string]$WindowsSDKVersion = '10.0.19041.0',
    [switch]$AllowTrackedPatch
)
$ErrorActionPreference = 'Stop'

function Get-WinFspMSBuildArguments {
    param([string]$Source, [string]$Output, [string]$CompilerVersion,
          [string]$Toolset, [string]$SDKVersion)
    if ($CompilerVersion -notmatch '^14\.[0-9]+\.[0-9]+$' -or
        $Toolset -notmatch '^v14[0-9]$' -or $SDKVersion -notmatch '^10\.0\.[0-9]+\.0$') {
        throw 'Explicit numeric MSVC/toolset/SDK versions are required'
    }
    # Upstream custom .pc generation commands do not quote these paths. Reject
    # metacharacters instead of allowing command interpretation by cmd/MSBuild.
    foreach ($path in @($Source, $Output)) {
        if ($path -notmatch '^[A-Za-z]:\\[A-Za-z0-9_.\\-]+$') {
            throw 'Build source/output paths must be local Windows paths without spaces or metacharacters'
        }
    }
    @(
        "$Source\build\VStudio\winfsp_dll.vcxproj", '/t:Build', '/m:1', '/nologo', '/nr:false',
        '/p:Configuration=Release', '/p:Platform=x64',
        "/p:PlatformToolset=$Toolset", "/p:VCToolsVersion=$CompilerVersion",
        "/p:MyTargetPlatformVersion=$SDKVersion", "/p:WindowsTargetPlatformVersion=$SDKVersion",
        '/p:MyBuildNumber=25156', '/p:MyCopyright=2015-2025 Bill Zissimopoulos',
        '/p:MyGitRevision=ddca7bd', "/p:SolutionDir=$Source\",
        "/p:OutDir=$Output\", "/p:IntDir=$Output\obj\",
        "/p:UserRootDir=$Output\empty-user-props\",
        "/bl:$Output\build.binlog", '/v:diagnostic'
    )
}

function Invoke-WinFspGit {
    param([string[]]$GitArguments)
    $result = & git @GitArguments
    if ($LASTEXITCODE -ne 0) { throw "git failed ($LASTEXITCODE): $GitArguments" }
    $result
}

function Invoke-WinFspMSVCBuild {
    if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
        throw 'MSVC build requires a disposable Windows lab guest'
    }
    foreach ($value in @($SourceDirectory, $OutputDirectory, $MSBuildPath, $VCToolsVersion)) {
        if ([string]::IsNullOrWhiteSpace($value)) { throw 'SourceDirectory, OutputDirectory, MSBuildPath and VCToolsVersion are required' }
    }
    $source = (Resolve-Path -LiteralPath $SourceDirectory).Path.TrimEnd('\')
    $output = [IO.Path]::GetFullPath($OutputDirectory).TrimEnd('\')
    if (Test-Path -LiteralPath $output) { throw 'OutputDirectory must not already exist' }
    if ($output.StartsWith($source + '\', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'OutputDirectory must be outside the source checkout'
    }
    $msbuild = (Resolve-Path -LiteralPath $MSBuildPath).Path
    if ([IO.Path]::GetFileName($msbuild) -ine 'MSBuild.exe') { throw 'Supply the explicit MSBuild.exe path' }
    $buildArguments = @(Get-WinFspMSBuildArguments $source $output $VCToolsVersion $PlatformToolset $WindowsSDKVersion)
    $revision = Invoke-WinFspGit -GitArguments @('-C', $source, 'rev-parse', 'HEAD')
    if ($revision -ne 'ddca7bd5481857a65ba552f643b8776fd070836f') { throw "Unexpected WinFsp revision: $revision" }
    if (Invoke-WinFspGit -GitArguments @('-C', $source, 'ls-files', '--others', '--exclude-standard')) {
        throw 'Untracked source files are not allowed'
    }
    $mode = 'baseline'
    if (Invoke-WinFspGit -GitArguments @('-C', $source, 'status', '--porcelain', '--untracked-files=all')) {
        if (-not $AllowTrackedPatch) { throw 'Tracked changes require -AllowTrackedPatch' }
        $mode = 'candidate'
    }
    $epoch = Invoke-WinFspGit -GitArguments @('-C', $source, 'show', '-s', '--format=%ct', 'HEAD')
    $msbuildVersion = (& $msbuild /nologo /version | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $msbuildVersion -notmatch '^[0-9]+(\.[0-9]+)+$') {
        throw 'Cannot determine MSBuild version'
    }
    New-Item -ItemType Directory -Path $output -ErrorAction Stop | Out-Null
    New-Item -ItemType Directory -Path "$output\empty-user-props" | Out-Null
    $patch = "$output\winfsp-x64.dll.source.patch"
    Invoke-WinFspGit -GitArguments @('-C', $source, 'diff', 'HEAD', '--binary', '--no-ext-diff', '--no-textconv', "--output=$patch")
    $patchHash = (Get-FileHash -LiteralPath $patch -Algorithm SHA256).Hash.ToLowerInvariant()
    $buildArguments | ConvertTo-Json | Set-Content -LiteralPath "$output\build-arguments.json" -Encoding UTF8
    & $msbuild @buildArguments 2>&1 | Tee-Object -FilePath "$output\build.log"
    if ($LASTEXITCODE -ne 0) { throw "MSBuild failed ($LASTEXITCODE); see $output\build.log" }
    # Never publish a manifest if the source changed while the compiler ran.
    Invoke-WinFspGit -GitArguments @('-C', $source, 'diff', 'HEAD', '--binary', '--no-ext-diff', '--no-textconv', "--output=$output\source-after.patch")
    if ((Invoke-WinFspGit -GitArguments @('-C', $source, 'rev-parse', 'HEAD')) -ne $revision -or
        (Get-FileHash -LiteralPath "$output\source-after.patch" -Algorithm SHA256).Hash.ToLowerInvariant() -ne $patchHash -or
        (Invoke-WinFspGit -GitArguments @('-C', $source, 'ls-files', '--others', '--exclude-standard'))) {
        throw 'Source changed during build; artifact is not qualified for testing'
    }
    $dllHash = (Get-FileHash -LiteralPath "$output\winfsp-x64.dll" -Algorithm SHA256).Hash.ToLowerInvariant()
    $recipeHash = (Get-FileHash -LiteralPath $PSCommandPath -Algorithm SHA256).Hash.ToLowerInvariant()
    @(
        "source_revision=$revision", "source_mode=$mode", "source_date_epoch=$epoch",
        "dll_sha256=$dllHash", "source_patch_sha256=$patchHash", "build_script_sha256=$recipeHash",
        'build_toolchain=msvc', "platform_toolset=$PlatformToolset", "vc_tools_version=$VCToolsVersion",
        "windows_sdk_version=$WindowsSDKVersion", "msbuild_version=$msbuildVersion",
        'version_build_number=25156', 'version_copyright_year=2025'
    ) | Set-Content -LiteralPath "$output\winfsp-x64.dll.manifest.txt" -Encoding ASCII
    Write-Host "LAB DLL (not deployment-qualified): $output\winfsp-x64.dll"
}

if ($MyInvocation.InvocationName -ne '.') { Invoke-WinFspMSVCBuild }
