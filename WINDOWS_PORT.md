# Windows mount (WinFsp)

`weed mount` on `GOOS=windows` serves the filer through WinFsp using
`github.com/winfsp/cgofuse`. In no-cgo mode `winfsp-x64.dll` is loaded at
runtime, so `CGO_ENABLED=0 GOOS=windows` cross-compilation works; cgo builds
compile against the WinFsp FUSE headers instead.

The host needs the WinFsp MSI (https://winfsp.dev/rel/). `weed mount` checks
the `HKLM\SOFTWARE\WOW6432Node\WinFsp` registry key first and fails with an
error naming the MSI when it is missing.

## Build

```
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags 5BytesOffset -o weed.exe ./weed
```

`go.mod` replaces `github.com/seaweedfs/go-fuse/v2` with a sibling checkout at
`../forks-go-fuse`, which carries the Windows compile port of the `fuse`
package types. Clone `AppMana/forks-go-fuse` next to this repository.

## Source layout

- `weed/mount/syscall_constants_{unix,windows}.go`, `weed/mount/syscall_shim_windows.go`: platform lock, open mode and syscall constants
- `weed/mount/weedfs_attr_windows.go`: Windows attribute hooks
- `weed/mount/winfsp_fs_windows.go`: `cgofuse` adapter over the `WFS` raw handlers
- `weed/mount/winfsp_identity_windows.go`, `weed/mount/winfsp_metadata_windows.go`: ownership, permissions and Windows file attributes
- `weed/mount/winfsp_case_sensitivity.go`: case-insensitive name resolution
- `weed/mount/winfsp_readahead_windows.go`: per-handle sequential read-ahead buffer
- `weed/mount/winfsp_errno_windows.go`: `go-fuse` `Status` to `cgofuse` errno mapping
- `weed/mount/winfsp_flags_windows.go`: `cgofuse` `O_*` to Go `os.O_*` translation
- `weed/mount/winfsp_host_windows.go`, `weed/mount/winfsp_options.go`: WinFsp host wrapper and mount options
- `weed/mount/winfsp/`: reparse point encoding for symlinks and junctions, and mount point retargeting
- `weed/command/mount_windows.go`: `RunMountWindows` (mount point preparation, network-FS mode, `AF_UNIX` `localSocket`, ctrl-event unmount)
- `weed/command/mount_common.go`, `weed/command/mount_helpers.go`: mount setup shared with Linux

## Behaviour

- Case sensitivity: volumes are case-insensitive by default; `-winfspCaseSensitive=true` gives POSIX case-sensitive names.
- Extended attributes: NT EAs set with `NtSetEaFile` are stored on the filer entry as `xattr-<NAME>`, where Linux mounts read them. Windows upper-cases EA names.
- Permissions: by default every entry is reported with `uid=-1,gid=-1`, `umask=000` and a `FileSecurity` DACL granting Everyone full access, so existing shared volumes stay accessible. `-winfspBasicPermissions` enforces the stored uid, gid and mode instead and rejects `-winfspOptions` that would override identity or the DACL. `-windows.uid` and `-windows.gid` set the ids recorded on entries this mount creates (0 by default).
- Symlinks: same-volume symlinks and directory junctions are stored as WinFsp reparse points. Absolute targets are interpreted inside the mounted volume (`rellinks`). `-winfspMountRoots` names a JSON file, written by the CSI node plugin, of where pods mount the volume, so absolute symlinks under those paths are stored volume-relative.
- A read-only mount (`-readOnly`) answers every mutation with `EROFS`.
- POSIX byte-range locks are handled locally by the WinFsp driver; `-distributedLock` has no effect on Windows.
- The directory mount point must not exist beforehand; `weed mount` removes an empty directory or dangling reparse point at the target (such as a pre-created CSI globalmount directory). A drive-letter-qualified directory is registered with the Windows Mount Manager (`\\.\C:\dir`) so containerd's CRI `parseMount` resolves it; `WEED_WINFSP_NO_MOUNTMGR=1` skips that for non-elevated use.
- Graceful unmount: send `CTRL_BREAK_EVENT` to the process group; Go delivers it as `os.Interrupt` and the mount unmounts. A killed process leaves a dangling reparse point that the next mount removes.

## Network file system mode for containers

HCS refuses to attach the container filters (`bindflt`/`wcifs`) to local
WinFsp volumes ([winfsp/winfsp#498](https://github.com/winfsp/winfsp/issues/498)),
so Windows containers cannot consume a directory mount.

Setting `WEED_WINFSP_VOLUME_PREFIX` (for example `\seaweedfs`) serves the
mount as a WinFsp network file system at `\\<prefix>\<hash>` instead, the
same path HCS takes for SMB volumes:

- The share name hashes the lowercased absolute mount directory, so a remount reproduces it however kubelet spells the path. Containers resolve their volume mappings to the UNC path once at creation.
- The prefix is passed as the standalone `--VolumePrefix=` argument; `-o VolumePrefix=` fails `FspFileSystemCreate` through cgofuse.
- Network file systems cannot use directory mount points, so FUSE mounts at an auto-assigned drive letter and `-dir` becomes a directory symlink to the UNC path once the share answers. Clean unmount removes the symlink.

Auto-assigned drive letters limit a node to about 22 concurrent network-FS
mounts.

## Caching

The mount passes `FileInfoTimeout=-1`, which engages the NT cache manager for
file data, with `DirInfoTimeout=2000`, `VolumeInfoTimeout=5000` and
`EaTimeout=1000` (milliseconds). `KeepFileCache` and `FlushOnCleanup` are
never set: WinFsp then flushes and purges a file's pages when its last handle
closes, and the flush stays at close instead of racing delete-on-close
unlinks. Metadata events from other clients invalidate cached data and
metadata through `FspFileSystemNotify`. Each handle also has a sequential
read-ahead buffer in the adapter.

`-winfspOptions` (comma separated WinFsp `-o` options) overrides these per
mount, for example a finite `FileInfoTimeout` for a volume where another
client rewrites files while they stay open here.
