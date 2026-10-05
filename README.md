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
| Linux mount/filer/S3 and native Windows smoke gates | [tested](https://github.com/AppMana/forks-seaweedfs/actions/runs/36923202188) |
| Full storage reliability gate: isolation contract failure | [fails](https://github.com/AppMana/forks-seaweedfs/actions/runs/36923202055) |
| Same build: mixed-OS, MSVC and power-loss qualification | unknown |
| New Linux amd64/arm64 server publication | unknown — first successful build required |
| Linux arm64 storage workloads | unknown |
| Synology 4.47-15 public availability | unknown — release remains draft |

[Storage lab and configuration](test/storage_lab/README.md) ·
[Server builds](.github/workflows/appmana-server-images.yml) ·
[Upstream](https://github.com/seaweedfs/seaweedfs)
