$ErrorActionPreference = 'Stop'
$root = Join-Path ([IO.Path]::GetTempPath()) ('lfs-hardlink-test-' + [guid]::NewGuid())
try {
    $gitRoot = Join-Path $root 'Git with spaces'
    $cmd = New-Item -ItemType Directory -Path (Join-Path $gitRoot 'cmd') -Force
    $bin = New-Item -ItemType Directory -Path (Join-Path $gitRoot 'mingw64/bin') -Force
    $git = Join-Path $cmd.FullName 'git.exe'
    $shim = Join-Path $cmd.FullName 'git-lfs.exe'
    $real = Join-Path $bin.FullName 'git-lfs.exe'
    $candidate = Join-Path $root 'candidate.exe'
    [IO.File]::WriteAllText($git, 'intact git launcher')
    New-Item -ItemType HardLink -Path $shim -Target $git | Out-Null
    [IO.File]::WriteAllText($real, 'old LFS')
    [IO.File]::WriteAllText($candidate, 'candidate LFS')
    & (Join-Path $PSScriptRoot 'install-lab-git-lfs.ps1') -GitRoot $gitRoot -Candidate $candidate
    if ([IO.File]::ReadAllText($git) -ne 'intact git launcher') { throw 'LFS override corrupted hard-linked git.exe' }
    foreach ($path in @($shim, $real)) {
        if ([IO.File]::ReadAllText($path) -ne 'candidate LFS') { throw "LFS override missing at $path" }
    }
    Write-Host 'PASS: LFS override preserves hard-linked Git launcher and replaces both LFS entries'
} finally {
    if (Test-Path -LiteralPath $root) { Remove-Item -LiteralPath $root -Recurse -Force }
}
