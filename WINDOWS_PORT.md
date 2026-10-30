# Windows mount port (WinFsp)

Provides a WinFsp-backed `weed mount` for `GOOS=windows` via
`github.com/winfsp/cgofuse` (no-cgo mode: `winfsp-x64.dll` is loaded at
runtime, so `CGO_ENABLED=0 GOOS=windows` cross-compilation works).

Runtime prerequisite on the host: the WinFsp MSI (https://winfsp.dev/rel/).
`weed mount` preflights the `HKLM\SOFTWARE\WOW6432Node\WinFsp` registry key
and fails with an actionable error when missing.

## `go-fuse` dependency

`go.mod` replaces `github.com/seaweedfs/go-fuse/v2` with a sibling checkout at
`../forks-go-fuse` containing the Windows-compile port of the `fuse` package
types. Builds and CI jobs must check out `forks-go-fuse` next to this
repository.

## Architecture and platform files

- `weed/mount/syscall_constants_{unix,windows}.go`, `weed/mount/syscall_shim_windows.go` — platform lock, open-mode and syscall constants
- `weed/mount/weedfs_attr_windows.go` — Windows attribute hooks
- `weed/mount/winfsp_fs_windows.go` — `cgofuse` adapter over the `WFS` raw handlers
- `weed/mount/winfsp_identity_windows.go`, `weed/mount/winfsp_metadata_windows.go` — ownership, permissions and Windows file attributes
- `weed/mount/winfsp_case_sensitivity.go` — case-insensitive name resolution
- `weed/mount/winfsp_readahead_windows.go` — per-handle sequential read-ahead buffer
- `weed/mount/winfsp_errno_windows.go` — `go-fuse` `Status` to `cgofuse` errno mapping
- `weed/mount/winfsp_flags_windows.go` — `cgofuse` `O_*` to Go `os.O_*` translation
- `weed/mount/winfsp_host_windows.go`, `weed/mount/winfsp_options.go` — WinFsp host wrapper and mount options
- `weed/mount/winfsp/` — reparse point encoding for symlinks and junctions, and mount point retargeting
- `weed/command/mount_windows.go` — `RunMountWindows` (WinFsp mountpoint preparation, `AF_UNIX` `localSocket`, ctrl-event unmount)
- `weed/command/mount_common.go`, `weed/command/mount_helpers.go` — shared mount setup and helper functions

## Behavior notes

- Case sensitivity: volumes default to Windows case-insensitive lookup (`SetCapCaseInsensitive(true)`); pass `-winfspCaseSensitive=true` for POSIX case-sensitive semantics.
- Extended attributes are the mount's xattrs: NT EAs set with `NtSetEaFile` are stored on the filer entry as `xattr-<NAME>`, where Linux mounts read them. Windows upper-cases EA names.
- Permissions and ownership: by default files are shared permissively; `-winfspBasicPermissions` enforces the stored uid/gid/mode. `-windows.uid` and `-windows.gid` set the uid/gid recorded on entries this mount creates (0 by default).
- Reparse points: same-volume symlinks and directory junctions are stored and resolved via WinFsp reparse points.
- POSIX byte-range locks are serviced locally by the WinFsp FSD; `-distributedLock` has no effect on Windows.
- The WinFsp directory mount point must not pre-exist; `weed mount` removes an empty directory or dangling reparse point at the target (such as a pre-created CSI globalmount directory).
- Graceful unmount: send `CTRL_BREAK_EVENT` to the process group; the Go runtime delivers it as `os.Interrupt` and the mount unmounts cleanly. `SIGKILL` leaves a dangling reparse point that the next mount removes.

## Build

```
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags 5BytesOffset -o weed.exe ./weed
```

## Container consumption: network-FS mode

Local WinFsp volumes cannot serve Windows containers because HCS refuses to
attach the container filters (`bindflt`/`wcifs`) to them
([winfsp/winfsp#498](https://github.com/winfsp/winfsp/issues/498)).

Setting `WEED_WINFSP_VOLUME_PREFIX` (for example `\seaweedfs`) switches the
mount to a WinFsp network file system: `weed mount` serves UNC
`\\<prefix>\<hash(dir)>`, mounts FUSE at an auto-assigned drive letter, and
symlinks `-dir` to the UNC path so containers consume the UNC path like SMB
volumes:

- Directory mount points are registered with the Windows Mount Manager (`\\.\C:\dir` syntax) so containerd's CRI `parseMount` resolves them.
- `--VolumePrefix=` is passed as a standalone argument rather than `-o VolumePrefix=`.
- Share names hash the lowercased absolute mount directory so remounts reproduce the share name deterministically.
- `-o FileSecurity=D:P(A;;FA;;;WD)` grants Everyone full access, mirroring Linux `-umask=000` when `-winfspBasicPermissions` is used.

Auto-assigned drive letters cap network-FS mode at ~22 concurrent volumes per node.

## Kernel data caching and read-ahead

`FileInfoTimeout=-1` engages the NT cache manager for file data, but WinFsp
completes `DeleteFile()` on cached files while deferring the FUSE unlink, so
an immediate recreate or rename of the same name can collide with the
still-existing backend entry. Because `FspFileSystemNotify` does not
invalidate FSD data caches, finite timeouts remain the default
(`FileInfoTimeout=1000,DirInfoTimeout=2000,VolumeInfoTimeout=5000`), paired
with per-handle sequential read-ahead in the adapter.

Read-mostly volumes that do not perform delete-then-recreate on the same path
can opt into full data caching via `-winfspOptions=FileInfoTimeout=-1` or
`SEAWEEDFS_WINFSP_OPTIONS=FileInfoTimeout=-1`.
