param([Parameter(Mandatory)][string]$GitRoot, [Parameter(Mandatory)][string]$Candidate)
$ErrorActionPreference = 'Stop'
$candidatePath = (Get-Item -LiteralPath $Candidate -ErrorAction Stop).FullName
$candidateHash = (Get-FileHash -LiteralPath $candidatePath -Algorithm SHA256).Hash
$targets = @(Get-ChildItem -LiteralPath $GitRoot -Filter git-lfs.exe -Recurse -File)
if ($targets.Count -eq 0) { throw 'installed Git LFS not found' }
if ($candidatePath -in $targets.FullName) { throw 'candidate must be outside installed LFS targets' }
$gitHashes = @(Get-ChildItem -LiteralPath $GitRoot -Filter git.exe -Recurse -File | Get-FileHash -Algorithm SHA256)
foreach ($target in $targets) {
    # Git for Windows cmd/git-lfs.exe is a HARD LINK to cmd/git.exe.
    # Overwriting its contents overwrites Git itself. Unlink only this LFS
    # directory entry before creating a separate file in this disposable VM.
    Remove-Item -LiteralPath $target.FullName -Force
    Copy-Item -LiteralPath $candidatePath -Destination $target.FullName
    if ((Get-FileHash -LiteralPath $target.FullName -Algorithm SHA256).Hash -ne $candidateHash) {
        throw "LFS override hash mismatch: $($target.FullName)"
    }
}
foreach ($gitHash in $gitHashes) {
    if ((Get-FileHash -LiteralPath $gitHash.Path -Algorithm SHA256).Hash -ne $gitHash.Hash) {
        throw "LFS override modified Git executable: $($gitHash.Path)"
    }
}
