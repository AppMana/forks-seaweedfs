param([Parameter(Mandatory)][ValidatePattern('^[a-zA-Z0-9-]+$')][string]$CompletionToken)
$ErrorActionPreference='Stop'
$media=(Get-Volume | Where-Object FileSystemLabel -eq 'WINFSP_BUILD').DriveLetter
if(-not $media){throw 'Build media missing'}
$media="$media`:\"
New-Item C:\lab -ItemType Directory -Force | Out-Null
& robocopy "$media\Files\Program Files (x86)" 'C:\Program Files (x86)' /E /COPY:DAT /R:0 /W:0 /NFL /NDL /NJH /NJS
if($LASTEXITCODE -ge 8){throw 'Toolchain copy failed'}
$p=Start-Process "$media\Git-2.51.0-64-bit.exe" -ArgumentList '/VERYSILENT /NORESTART /SP- /SUPPRESSMSGBOXES' -Wait -PassThru
if($p.ExitCode -ne 0){throw 'Git install failed'}
$env:PATH='C:\Program Files\Git\cmd;'+$env:PATH
$vs='C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools'
$sdk='C:\Program Files (x86)\Windows Kits\10'
# The image payload is staged, not installed: make SDK discovery explicit.
$env:WindowsSdkDir="$sdk\"
$env:WindowsSDKVersion='10.0.26100.0\'
$env:UniversalCRTSdkDir="$sdk\"
$env:UCRTVersion='10.0.26100.0'
$env:WindowsSdkDir_10="$sdk\"
$env:UniversalCRTSdkDir_10="$sdk\"
# The generic guest has OS-provided ucrt.props which also reads SDK registry
# roots. Register only these staged SDK paths in this disposable guest.
foreach($key in @('HKLM:\SOFTWARE\Microsoft\Microsoft SDKs\Windows\v10.0','HKLM:\SOFTWARE\Wow6432Node\Microsoft\Microsoft SDKs\Windows\v10.0')) {
 New-Item $key -Force | Out-Null
 New-ItemProperty $key -Name InstallationFolder -Value "$sdk\" -PropertyType String -Force | Out-Null
}
foreach($key in @('HKLM:\SOFTWARE\Microsoft\Windows Kits\Installed Roots','HKLM:\SOFTWARE\Wow6432Node\Microsoft\Windows Kits\Installed Roots')) {
 New-Item $key -Force | Out-Null
 New-ItemProperty $key -Name KitsRoot10 -Value "$sdk\" -PropertyType String -Force | Out-Null
}
$env:PreferredToolArchitecture='x64'
$tools=@("$vs\MSBuild\Current\Bin\MSBuild.exe","$vs\VC\Tools\MSVC\14.44.35207\bin\Hostx64\x64\cl.exe","$vs\VC\Tools\MSVC\14.44.35207\bin\Hostx64\x64\link.exe","$sdk\bin\10.0.26100.0\x64\rc.exe")
$tools | ForEach-Object { $f=Get-Item $_; [pscustomobject]@{path=$f.FullName;version=$f.VersionInfo.FileVersion;sha256=(Get-FileHash $_).Hash} } | ConvertTo-Json | Set-Content C:\lab\toolchain.json
& $tools[0] /nologo /version
if($LASTEXITCODE -ne 0){throw 'MSBuild cannot execute'}
& git -c core.autocrlf=false clone "$media\winfsp.bundle" C:\lab\winfsp
if($LASTEXITCODE -ne 0){throw 'Source clone failed'}
# git bundle does not transfer the shallow boundary of this pinned checkout.
'ddca7bd5481857a65ba552f643b8776fd070836f' | Set-Content C:\lab\winfsp\.git\shallow -Encoding ASCII
& git -C C:\lab\winfsp config core.autocrlf false
if($LASTEXITCODE -ne 0){throw 'Source line-ending configuration failed'}
# The caller stages the checked-in recipe and patch, not stale copies on media.
foreach($mode in @('baseline','candidate')) {
 if($mode -eq 'candidate') {
  & git -C C:\lab\winfsp apply C:\lab\winfsp-guid-mount.patch
  if($LASTEXITCODE -ne 0){throw 'Patch failed'}
 }
 & C:\lab\build-winfsp-msvc.ps1 -SourceDirectory C:\lab\winfsp -OutputDirectory "C:\lab\$mode" -MSBuildPath $tools[0] -VCToolsVersion 14.44.35207 -PlatformToolset v143 -WindowsSDKVersion 10.0.26100.0 -AllowTrackedPatch:($mode -eq 'candidate') *> "C:\lab\$mode-console.log"
 Write-Output "BUILD_COMPLETE_${mode}:$CompletionToken"
}
