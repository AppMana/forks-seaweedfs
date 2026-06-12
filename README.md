# forks-seaweedfs — SeaweedFS with Windows mount support

AppMana fork of [seaweedfs/seaweedfs](https://github.com/seaweedfs/seaweedfs),
branched from the **4.23** release. It adds one thing: `weed mount` works on
Windows.

```
weed mount -filer=<filer:port> -dir=C:\mnt\seaweedfs
```

- The mount is served through [WinFsp](https://winfsp.dev/) via
  [cgofuse](https://github.com/winfsp/cgofuse) in its no-cgo mode, so
  `CGO_ENABLED=0 GOOS=windows` cross-compiles from Linux. WinFsp must be
  installed on the host (the mount preflights for it).
- The adapter (`weed/mount/winfsp_*_windows.go`) layers cgofuse's path-based
  API over the existing inode-based mount filesystem, so the battle-tested
  read/write/rename pipelines are reused, not reimplemented. Includes
  per-handle sequential read-ahead.
- `WEED_WINFSP_VOLUME_PREFIX=\seaweedfs` switches to a WinFsp *network* file
  system (UNC path). This is required when containers consume the mount:
  Windows HCS refuses to bind local WinFsp volumes into containers
  ([winfsp#498](https://github.com/winfsp/winfsp/issues/498)).
- `-winfspOptions=k=v,...` passes raw WinFsp options. `FileInfoTimeout=-1`
  enables kernel data caching (large speedup for small reads) but is only
  safe on read-mostly volumes; see `WINDOWS_PORT.md` for why.

## Large volumes

This fork is built and tested exclusively with the **large-disk** variant
(`-tags 5BytesOffset`), matching the upstream `*_large_disk` releases: 5-byte
needle offsets raise the per-volume size limit (8TB volume files) for big-disk
deployments. Build:

```
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -tags 5BytesOffset -o weed.exe ./weed
```

`go.mod` replaces `github.com/seaweedfs/go-fuse/v2` with a sibling checkout of
[AppMana/forks-go-fuse](https://github.com/AppMana/forks-go-fuse) (a
compile-only Windows port of the fuse package) — clone it next to this repo
as `../forks-go-fuse` before building.

`WINDOWS_PORT.md` documents every file changed relative to upstream, the
behavior notes (case sensitivity, xattrs, locking), and the caching
investigation. CI cross-compiles, runs native Windows tests, executes a real
WinFsp mount smoke test, and benchmarks every build.

Used by [AppMana/forks-seaweedfs-csi-driver](https://github.com/AppMana/forks-seaweedfs-csi-driver)
to serve SeaweedFS persistent volumes to Windows Kubernetes nodes.

Upstream README: https://github.com/seaweedfs/seaweedfs
