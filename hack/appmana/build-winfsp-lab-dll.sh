#!/usr/bin/env bash
# Lab-only user-mode DLL build; never builds or installs a kernel driver.
set -euo pipefail
source_dir=$(cd "${1:?usage: build-winfsp-lab-dll.sh /path/to/winfsp-v2.1}" && pwd)
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
expected=ddca7bd5481857a65ba552f643b8776fd070836f
actual=$(git -C "$source_dir" rev-parse HEAD)
if [[ "$actual" != "$expected" ]]; then
    echo "Expected WinFsp v2.1 source $expected; found $actual" >&2
    exit 1
fi
build_dir=$(mktemp -d "${TMPDIR:-/tmp}/winfsp-dll-build.XXXXXXXX")
exec > >(tee "$build_dir/build.log") 2>&1
echo "source=$source_dir revision=$actual output=$build_dir"
git -C "$source_dir" diff HEAD --binary > "$build_dir/source.patch"
sha256sum "$build_dir/source.patch" "$script_dir/winfsp-lab-mingw-shim.h"
clang --version
x86_64-w64-mingw32-gcc --version
cd "$source_dir"
for source_file in src/dll/*.c src/dll/fuse/*.c src/dll/fuse3/*.c src/shared/ku/mountmgr.c src/shared/ku/posix.c; do
    clang --target=x86_64-w64-windows-gnu -fms-extensions -O2 \
        -Wno-ignored-pragma-intrinsic -Wno-unknown-pragmas \
        -Wno-error=incompatible-function-pointer-types -ferror-limit=3 \
        -c -include "$script_dir/winfsp-lab-mingw-shim.h" \
        -DUNICODE -D_UNICODE -DNDEBUG -DWINFSPDLL_EXPORTS \
        '-DMyEventLogRegisterPath="."' '-DMyFsctlRegisterPath="."' '-DMyNpRegisterPath="."' \
        -Iinc -Isrc "$source_file" -o "$build_dir/${source_file##*/}.o"
done
x86_64-w64-mingw32-windres -DMyVersionWithCommas=2,1,25156,0 \
    -DMyCompanyName=Navimatics -DMyDescription=WinFsp-Lab \
    -DMyFullVersion=2.1.25156.lab -DMyProductFileName=winfsp \
    -DMyCopyright=2015-2025 -DMyProductName=WinFsp -DMyProductVersion=2025 \
    src/dll/version.rc -o "$build_dir/version.res.o"
x86_64-w64-mingw32-windres -Isrc/dll/eventlog src/dll/eventlog/eventlog.rc \
    -o "$build_dir/eventlog.res.o"
x86_64-w64-mingw32-gcc -shared -nostartfiles -Wl,--entry,_DllMainCRTStartup \
    -o "$build_dir/winfsp-x64.dll" "$build_dir"/*.o src/dll/library.def \
    -lntdll -luser32 -ladvapi32 -lcredui -lnetapi32 -lrpcrt4 -lsecur32 \
    -lshlwapi -lversion -lwldap32 -lshell32 -lole32 -luuid -lkernel32
sha256sum "$build_dir/winfsp-x64.dll"
echo "LAB DLL (not deployment-qualified): $build_dir/winfsp-x64.dll"
