#!/usr/bin/env bash
# Prepare offline, read-only compiler media; never starts a VM or installs tools.
set -euo pipefail
if [[ $# != 4 ]]; then
  echo 'usage: prepare-winfsp-msvc-media.sh VERIFIED_LAYER WINFSP_CHECKOUT GIT_INSTALLER EXISTING_OUTPUT_PARENT' >&2
  exit 2
fi
layer=$(realpath -- "$1")
source_dir=$(realpath -- "$2")
git_installer=$(realpath -- "$3")
output_parent=$(realpath -- "$4")
test -d "$output_parent"
# These pins are coupled to provision-winfsp-msvc-lab.ps1 and the DLL recipe.
layer_sha=f8fbf23ad44b41aed722d760e5395a133271087b6b5ece9e4119886d0a71c23b
git_sha=843037416371600a7f289be8fe2b2224afe1c1bb0736bbab7b3ff393e6a7aaf2
source_sha=ddca7bd5481857a65ba552f643b8776fd070836f
printf '%s  %s\n' "$layer_sha" "$layer" "$git_sha" "$git_installer" | sha256sum -c -
[[ "$(git -C "$source_dir" rev-parse HEAD)" == "$source_sha" ]]
for command in tar genisoimage git sha256sum; do command -v "$command" >/dev/null; done
work=$(mktemp -d "$output_parent/winfsp-msvc-media.XXXXXXXX")
mkdir "$work/payload"
tar --warning=no-unknown-keyword --no-same-owner --no-same-permissions \
  -xzf "$layer" -C "$work/payload" \
  'Files/Program Files (x86)/Microsoft Visual Studio/2022/BuildTools' \
  'Files/Program Files (x86)/Windows Kits/10'
chmod -R u+rwX "$work/payload"
if [[ -n "$(find "$work/payload" -type l -print -quit)" ]]; then
  echo "Refusing symbolic links in compiler payload; inspect retained $work" >&2
  exit 1
fi
toolroot="$work/payload/Files/Program Files (x86)"
test -s "$toolroot/Microsoft Visual Studio/2022/BuildTools/MSBuild/Current/Bin/MSBuild.exe"
test -s "$toolroot/Microsoft Visual Studio/2022/BuildTools/VC/Tools/MSVC/14.44.35207/bin/Hostx64/x64/cl.exe"
test -s "$toolroot/Windows Kits/10/Include/10.0.26100.0/ucrt/ctype.h"
# Bundle only the pinned commit, never the candidate's dirty worktree.
git -C "$source_dir" bundle create "$work/payload/winfsp.bundle" HEAD
cp -- "$git_installer" "$work/payload/Git-2.51.0-64-bit.exe"
genisoimage -quiet -D -udf -iso-level 3 -J -joliet-long -V WINFSP_BUILD \
  -o "$work/compiler.iso" "$work/payload"
sha256sum "$work/compiler.iso" > "$work/compiler.iso.sha256"
printf 'SEAWEEDFS_WINDOWS_MSVC_ISO=%s\n' "$work/compiler.iso"
printf 'SEAWEEDFS_WINDOWS_MSVC_ISO_SHA256=%s\n' "$(sha256sum "$work/compiler.iso" | cut -d' ' -f1)"
printf 'Retained payload and ISO: %s\n' "$work"
