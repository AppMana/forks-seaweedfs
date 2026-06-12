# AppMana Windows mount port (WinFsp)

Branch `appmana-4.23-large-disk-winfsp`, based on upstream tag `4.23`.
Adds a WinFsp-backed `weed mount` for GOOS=windows via
`github.com/winfsp/cgofuse` (no-cgo mode: `winfsp-x64.dll` is loaded at
runtime, so `CGO_ENABLED=0 GOOS=windows` cross-compilation works).

Runtime prerequisite on the host: the WinFsp MSI (https://winfsp.dev/rel/).
`weed mount` preflights the `HKLM\SOFTWARE\WOW6432Node\WinFsp` registry key
and fails with an actionable error when missing.

## go-fuse fork dependency

`go.mod` replaces `github.com/seaweedfs/go-fuse/v2` with a local sibling
checkout of https://github.com/AppMana/forks-go-fuse (branch
`appmana/windows-compile`, a compile-only Windows port of the `fuse`
package). CI and image builds must clone it next to this repo:

```
git clone -b appmana/windows-compile https://github.com/AppMana/forks-go-fuse ../forks-go-fuse
```

The repo-path of the replacement cannot be pointed at GitHub directly
because the fork keeps the upstream module path (intentionally, to keep
the rebase diff additive).

## Touched upstream files (keep this list current for rebases)

New files are all `*_windows.go` (or suffix-tagged) and do not affect
other platforms.

| File | Change |
|---|---|
| `go.mod` | `replace` for forks-go-fuse; add `github.com/winfsp/cgofuse` |
| `weed/mount/weedfs.go` | removed vestigial `fs.Inode` embed + `go-fuse/v2/fs` import |
| `weed/mount/weedfs_rename.go` | `fs.RENAME_EXCHANGE` constant inlined (0x2), `fs` import dropped |
| `weed/mount/weedfs_xattr.go` | build tag `!freebsd` → `!freebsd && !windows` |
| `weed/mount/posix_file_lock.go` | `syscall.F_{RD,WR,UN}LCK` → platform consts `fRdlck/fWrlck/fUnlck` |
| `weed/mount/weedfs_file_lock.go` | same lock-constant swap |
| `weed/mount/weedfs_access.go` | `syscall.O_ACCMODE` → platform const `oAccmode` |
| `weed/command/mount.go` | new `-volumeLabel` flag (all platforms, used by windows only) |
| `weed/command/mount_std.go` | moved `ensureBucketAllowEmptyFolders`, `bucketPathForMountRoot`, `peerStringOrEmpty` to `mount_helpers.go` (shared) |
| `weed/command/mount_notsupported.go` | build tag excludes windows |

New files:

- `weed/mount/syscall_constants_{unix,windows}.go`
- `weed/mount/weedfs_attr_windows.go`, `weed/mount/weedfs_xattr_windows.go`
- `weed/mount/winfsp_fs_windows.go` — cgofuse adapter over the WFS raw handlers
- `weed/mount/winfsp_errno_windows.go` — go-fuse Status → cgofuse errno map
- `weed/mount/winfsp_flags_windows.go` — cgofuse O_* → Go os.O_* translation
- `weed/mount/winfsp_host_windows.go` — WinFsp host wrapper (ReaddirPlus, case-sensitive)
- `weed/command/mount_windows.go` — `RunMountWindows` (WinFsp mountpoint prep, AF_UNIX localSocket, ctrl-event unmount)
- `weed/command/mount_helpers.go` — shared helpers extracted from mount_std.go

## Behavior notes / limitations

- The volume is declared case-sensitive (`SetCapCaseInsensitive(false)`);
  apps that rely on case-folding will see ENOENT for wrong-case names.
- xattrs return ENOTSUP / ENOSYS (freebsd precedent).
- POSIX byte-range locks are serviced locally by the WinFsp FSD;
  `-distributedLock` has no effect on windows.
- Files are presented as owned by the mounting user (`uid=-1,gid=-1`).
- The WinFsp directory mount point must NOT pre-exist; `weed mount`
  removes an empty directory or dangling reparse point at the target
  (kubelet pre-creates the CSI globalmount dir).
- Graceful unmount: send a console ctrl event (CTRL_BREAK_EVENT to the
  process group); the Go runtime delivers it as os.Interrupt and the
  mount unmounts cleanly. SIGKILL leaves a dangling reparse point that
  the next mount (or the CSI mount supervisor) removes.

## Build

```
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags 5BytesOffset -o weed.exe ./weed
```

## Container consumption: network-FS mode (added after lab e2e, 2026-06-11)

Local WinFsp volumes cannot serve Windows containers: HCS refuses to
attach the container filters (bindflt/wcifs) to them
("Do not attach the filter to the volume at this time",
[winfsp/winfsp#498](https://github.com/winfsp/winfsp/issues/498)).
Host processes are unaffected; pods are.

`WEED_WINFSP_VOLUME_PREFIX` (e.g. `\seaweedfs`) switches the mount to a
WinFsp NETWORK file system: weed serves UNC `\\<prefix>\<hash(dir)>`,
mounts FUSE at an auto-assigned drive letter, and symlinks `-dir` to the
UNC path. Containers then consume the UNC like SMB CSI volumes. The CSI
mount supervisor defaults this mode on Windows.

Related fixes in this mode:
- Directory mount points are registered with the Windows mount manager
  (`\\.\C:\dir` syntax) so containerd's CRI `parseMount` can resolve them.
- `--VolumePrefix=` must be the standalone argv form; `-o VolumePrefix=`
  fails FspFileSystemCreate with STATUS_INVALID_PARAMETER.
- Share names hash the lowercased absolute mount dir (kubelet passes
  drive-less rooted paths; containers freeze their UNC mapping at
  creation, so remounts must reproduce the share name exactly).
- `-o FileSecurity=D:P(A;;FA;;;WD)` (Everyone full access) mirrors the
  Linux `-umask=000`; without it pod users cannot reopen their own files
  for write.

Limitation: auto drive letters cap at ~22 concurrent volumes per node.
A cgofuse patch to skip the local mount point for network file systems
is the long-term cleanup.

## Kernel data caching investigation (2026-06-12)

`FileInfoTimeout=-1` engages the NT cache manager for file data and was
measured at 40-90x on warm/small reads (CI bench: warm 4KB reads
14.8 → 585 MB/s). It is NOT the default because of a verified
correctness hazard: for a file whose data the cache holds, the FSD
completes `DeleteFile()` while deferring the FUSE unlink, so an
immediate recreate/rename of the same name collides with the
still-existing backend file (~50% failure rate for pwsh7
`Move-Item -Force`; probe confirmed the backend still has the file at
the failure instant). `FspFileSystemNotify` fires watcher notifications
but does not invalidate FSD caches, so no adapter-side mitigation
exists.

Defaults stay finite (`FileInfoTimeout=1000,DirInfoTimeout=2000,
VolumeInfoTimeout=5000`). Read-mostly volumes (model caches, asset
distribution) can opt into full data caching per mount:
`-winfspOptions=FileInfoTimeout=-1`, or on the CSI DaemonSet via
`SEAWEEDFS_WINFSP_OPTIONS=FileInfoTimeout=-1`. Do not enable it for
volumes that see delete-then-recreate patterns (build outputs).

cgo vs no-cgo cgofuse: benched within 9% of each other across all
dimensions; no-cgo remains the default (the CI mount-bench job tracks
both).
