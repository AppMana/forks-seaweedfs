# AppMana SeaweedFS

SeaweedFS 4.47 with Windows mounts and fixes for allocation, vacuum/recovery,
memory use, S3 streaming and shared-mount coherence.

## Install and use

| Package | Use |
| --- | --- |
| [ghcr.io/appmana/seaweedfs](https://github.com/orgs/AppMana/packages?repo_name=forks-seaweedfs) | Master, filer, volume and S3 services |
| [CSI packages](https://github.com/AppMana/forks-seaweedfs-csi-driver) | Linux and Windows Kubernetes mounts |
| [Synology package](https://github.com/AppMana/spk-seaweedfs) | DSM installation and service management |

Select a published server image in your role workloads or Helm values, and
deploy CSI for client mounts. Stock k0s works without a storage-specific fork.

**Windows CSI installs bundled WinFsp automatically.** No separate installation
is needed for that deployment. DLL-only packages retain the signed stock kernel
driver; they do not include fixes that require a driver replacement.
Standalone weed.exe is a developer binary, not a dependency installer.

Use matching **large-disk** builds across upgrades and rollbacks: their index
format differs from normal upstream builds. Set container memory limits and
leave explicit Go-memory/upload/download admission overrides unset to use
automatic sizing. Vacuum needs temporary disk space and refuses corrupt live
records rather than silently discarding them.

See [Windows behavior](WINDOWS_PORT.md) for caching and permission limitations.

## Compatibility

Results apply to the linked build and scope, not every historical test run.

| Scope | Result |
| --- | --- |
| Linux mount/filer/S3 and native Windows smoke gates | [tested](https://github.com/AppMana/forks-seaweedfs/actions/runs/37845560942) |
| Large-disk regressions: unit/race, mutants, maintenance, packaged image, etcd restore, isolated lab, rollback | [tested](https://github.com/AppMana/forks-seaweedfs/actions/runs/37845560963) |
| Linux amd64/arm64 server image publication | [tested](https://github.com/AppMana/forks-seaweedfs/actions/runs/37845561076) |
| Windows Git LFS mount smoke | intermittent: `lfs install --local` cannot resolve `.git` right after `git init` in 2 of 7 runs |
| VM fault gates (power loss, four-VM durability) | pending a self-hosted KVM runner |
| Same build: mixed-OS, MSVC and power-loss qualification | unknown |
| Linux arm64 storage workloads | unknown |
| Synology 4.47-15 public availability | unknown — release remains draft |

## Operating the AppMana deployment

Several repositories ship one storage system. Change them together and roll
out one component at a time.

### Branches

| Branch | Role |
| --- | --- |
| `merge/upstream-4.47` | Integration branch and default: every fork fix lands here (merged, never rebased), and CI runs here. |
| `release/<change>-<base>` | Ship branch: the deployed commit plus only the change being rolled out, e.g. `release/mount-notify-gate-dd2b9ef98`. It is merged back into `merge/upstream-4.47`. |

Build images from the ship branch whenever `merge/upstream-4.47` has unshipped
changes in the same component.

### Images and where they are pinned

| Image | Built by | Pinned in appmana-cluster |
| --- | --- | --- |
| `ghcr.io/appmana/seaweedfs` (master, filer, volume, S3, shell jobs) | this repo, [`appmana-server-images.yml`](.github/workflows/appmana-server-images.yml), `sha-<commit>_large_disk` | `clusters/appmana-cluster-03/seaweedfs/helm-release.yaml` (master, filer: `imageOverride`), `volume-statefulsets.yaml`, `harbor-s3-gateway.yaml`, `traces-s3-gateway.yaml`, and the maintenance jobs beside them |
| `ghcr.io/appmana/seaweedfs-mount` (CSI mount supervisor + `weed mount`) | [forks-seaweedfs-csi-driver](https://github.com/AppMana/forks-seaweedfs-csi-driver) `build-images.yml` on `merge/**` branches, `candidate-<sha>-<run>-<attempt>` | `seaweedfs/csi-driver.yaml` (DaemonSet `seaweedfs-mount`), `csi-driver-windows.yaml` |
| `ghcr.io/appmana/seaweedfs-csi-driver` | same workflow | `csi-driver.yaml` (node plugin), `csi-controller.yaml`, `csi-driver-windows.yaml` |

Every pin is a digest. The CSI fork selects this repo's source with
`SEAWEEDFS_COMMIT` in `build-images.yml` and both Linux mount Dockerfiles;
`test/kubernetes_lab/build_pins_test.go` requires them to agree and the weed
build to be static (`CGO_ENABLED=0`), `5BytesOffset`, go-fuse `1bdeec4` and
stamped (`weed version` prints the commit).

### Roles in appmana-cluster

- **Masters** (Raft, three ordinals) and **filers** (three ordinals, shared
  store) come from the upstream Helm chart, `seaweedfs/helm-release.yaml`.
- **Volume servers** are per-disk StatefulSets, `volume-statefulsets.yaml`.
- **S3 gateways**: Harbor's registry and the trace bucket run their own
  gateways, `harbor-s3-gateway.yaml` and `traces-s3-gateway.yaml`.
- **Mounts**: the CSI node plugin asks the per-node `seaweedfs-mount`
  supervisor to run one `weed mount` per volume. Both DaemonSets use
  `OnDelete`, so a new pin replaces nothing until a node's pod is deleted.

### Mount kernel notifications

A remote namespace change makes the mount expire the name in the kernel
(`EntryNotify`), and a remote content change drops cached pages
(`InodeNotify`). Linux takes the directory's lock, or the pages' locks, for
these, and a local request holds those locks until the mount answers it. The
mount's notify gate (`weed/mount/kernel_notify_gate.go`) therefore guarantees:

- no such notification is sent while this mount is serving a request whose
  caller holds the lock it needs (a lookup, create, rename, unlink, listing or
  attribute change in that directory; a read or write of that file);
- deferred notifications are sent, once each, after the last such request is
  answered, from a goroutine nothing else waits on;
- a request the kernel queued but the mount has not read yet can still delay
  a notification until that one request is answered.

Filer calls a FUSE request waits on give up after 60 s (`filerReplyTimeout`)
without cancelling the filer's work, so no request holds a kernel lock longer.

### Tests

```sh
# unit and race, as CI's large-disk job runs them
GOWORK=off go test -short -tags 5BytesOffset -race -count=1 ./weed/mount/... ./weed/filer/... ./weed/storage/...
python3 -m unittest discover -s test/storage_lab -v
# real FUSE mounts on this host (puts weed on PATH)
(cd test/fuse_integration && PATH=/path/to/weed-dir:$PATH go test -race -count=1 ./...)
# disposable-VM kernel scenarios through Labcontainers
SEAWEEDFS_NOTIFY_LAB_LIVE=1 SEAWEEDFS_LINUX_WEED=/abs/weed \
LABCONTAINERS_VM_IMAGE=labcontainers/vm-ubuntu:csi-f9df714 LABCONTAINERS_LABD=/abs/labd \
GOWORK=off go test ./test/storage_lab/vm -run '^TestFuseReverseInvalidationLab$' -v -timeout 120m
```

The storage lab README covers the four-VM fault, Windows and mixed-OS
scenarios. VM jobs need a self-hosted KVM runner.

### Rollout

- **Masters and filers**: set the component's `updatePartition` to the
  highest ordinal, verify that pod, then lower it one ordinal at a time. The
  chart renders no `updateStrategy` at partition 0, so Helm cannot remove a
  partition it never recorded: commit partition 1 as deployed before 0, or
  ordinal 0 keeps the old image.
- **Volume servers**: `hacking/seaweedfs-volume-restart.py` (preview, then
  `--apply`), one disk at a time; check index load, payload readback and
  registration before the next.
- **S3 gateways**: `hacking/seaweedfs-s3-probe.py` against the replaced
  gateway (write, read back, delete).
- **CSI mounts**: change the `seaweedfs-mount` digest, then delete the
  supervisor pod on one node. That remounts every SeaweedFS volume on the
  node. Verify the image digest, `weed version`, that pods' claims read, and
  that no weed thread is in `D` (`ps -eLo stat,comm | grep weed`), then move
  to the next node. Replace the node plugin before the supervisor when both
  change. Nodes running training wait for a scheduled window.

### When a node hangs or panics

Nodes run `kernel.hung_task_panic=1`: a FUSE request stuck 120 s reboots the
node. Kernel output reaches Loki through netconsole:
`{job="netconsole", host="appmana-NNN"}` (filter one `collector=`), the
journal is `{job="systemd-journal", host=...}`, and kdump leaves vmcores in
`/var/crash` on the node.

[Storage lab and configuration](test/storage_lab/README.md) ·
[Server builds](.github/workflows/appmana-server-images.yml) ·
[Upstream](https://github.com/seaweedfs/seaweedfs)
