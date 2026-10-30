# Windows mount harnesses

Scripts that exercise the Windows `weed mount` against a self-contained
SeaweedFS server, to reproduce filesystem contract failures before changing
mount code. The VM based gates that call them are described in
[`test/storage_lab/README.md`](../../test/storage_lab/README.md).

| Script | Does |
| --- | --- |
| `mount-smoke.ps1` | Focused WinFsp scenarios against a fresh `weed server` and mount (below). |
| `winfsp-cuzz.ps1` | Runs a `mount-smoke.ps1` case under Application Verifier Cuzz (below). |
| `winfsp-compat.ps1` | WinFsp's external `winfsp-tests` suite against a mount, with an exclusion list; advisory in CI. |
| `mount-bench.ps1` | Network-FS mode benchmark: sequential write, warm sequential read, 4 KiB reads, cold and repeated listing of 200 files, small-file creates. One markdown row per `-Label`. |
| `windows-access-regression.sh` | Paired ordinary-access latency comparison of two `weed.exe` builds in Windows VMs. |
| `compare-windows-access.py` | Compares the retained logs of that comparison. |
| `run-windows-vm-suite.sh` | Runs prebuilt, pinned inputs through the Labcontainers Windows VM gates and fails after reporting every result. |
| `build-winfsp-lab-dll.sh` | Builds a lab-only WinFsp user-mode DLL (never a kernel driver). |
| `install-lab-git-lfs.ps1` | Replaces Git LFS inside a disposable VM without overwriting Git's hard-linked launcher. |
| `analyze-fileio.py`, `select-fileio-trace.ps1`, `seaweed-fileio.wprp` | FileIO ETW capture profile and analysis (below). |
| `compare_vacuum_indexes.py` | Read-only check that a vacuum kept every live index record across replica snapshots. |
| `decode-filer-entry/` | Go tool that prints filer store values (etcd, LevelDB, Redis and so on) as JSON, one base64 value per stdin line, such as the `value` field of `etcdctl get -w json`. |

The Python scripts have `test_*.py` unit tests:
`python3 -m unittest discover -s hack/appmana -p 'test_*.py'`.

## mount-smoke.ps1 cases

- `NamespaceCoherence`: a file stays directly readable while its parent enumeration omits it and rename returns `NAME NOT FOUND`.
- `GitAtomicRename`: `git init` on a fresh mount.
- `GitAtomicRenamePrimed`: the metadata and rename-over-existing sequence from the full smoke test, then `git init`, which checks `config.lock` handling after metadata changes.
- `GitLfsTempMetadata`: repeated `git status` over modified LFS assets. Its seed `git add` exercises Git LFS's temp-file sequence. Status can use cached metadata, so status passes alone do not prove repeated object renames; pass `-WinFspTestExe` (built from `./test/winfsp`) to also run `TestGitLfsObjectRename`, which performs create, chmod, write, close, fan-out mkdir and rename with no retries and checks each final object byte for byte.

```powershell
./hack/appmana/mount-smoke.ps1 `
  -WeedExe (Resolve-Path ./weed.exe) `
  -TestCase GitAtomicRenamePrimed
```

Every run removes and recreates `WorkRoot`, so runs start from the same
filer, mount and metadata cache state. Logs stay under `WorkRoot\logs`.
`-Verbosity 4` logs SeaweedFS callback and filer ordering, `-WinFspOptions`
sets WinFsp cache options for A/B runs, and `-EtwFileIO -Trace` (in a
disposable VM) records WPR FileIO ETW alongside the WinFsp request/reply log.

To read decoded FileIO events:

```sh
python3 hack/appmana/analyze-fileio.py /path/to/GitLfsTempMetadata-01-fileio.xml.zip --path git-lfs-temp-metadata
```

The analyzer pairs FileIo requests and completions by IRP, keeps the
initiating PID, timestamps and NTSTATUS, and resolves paths from successful
creates on a file object; those paths name the open, not necessarily a later
renamed name. It reports unmatched events and trace-loss headers. Circular
buffers can drop older history even when `EventsLost` is zero. Nonzero
statuses include expected absent-file probes, reparses and fast-I/O fallback.
Compare the recorded `GIT FAILURE UTC` timestamp with the full WinFsp log
before drawing conclusions; `--all` includes successful completions. Keep the
ETL for WPA.

## Schedule fuzzing and replay

```powershell
./hack/appmana/winfsp-cuzz.ps1 `
  -WeedExe (Resolve-Path ./weed.exe) `
  -FuzzingLevel 4 `
  -RandomSeed 1 `
  -TraceSummary
```

Cuzz inserts delays at Win32 synchronization calls; rerun a failing seed to
recreate the interleaving. The wrapper removes its Application Verifier
settings when it finishes and runs the filer from a copy of the binary under
another image name, so only the mount process is perturbed. Application
Verifier comes with the Windows SDK. `-TraceSummary` writes the mount's
operation counters after unmount, so it does not change the schedule;
`-Trace` enables WinFsp's request/response log and changes timing a lot, so
use it only once a seed reproduces without it.

Which tool controls what:

- [Application Verifier Cuzz](https://learn.microsoft.com/en-us/windows-hardware/drivers/devtest/application-verifier-tests-within-application-verifier) explores schedules in the real x64 `weed.exe` process.
- [Time Travel Debugging](https://learn.microsoft.com/en-us/windows-hardware/drivers/debuggercmds/time-travel-debugging-ttd-exe-command-line-util) records and replays one failing user-mode execution. It does not find a failing schedule, and its 5x to 20x overhead changes timing.
- [WinFsp tests](https://winfsp.dev/doc/WinFsp-Testing/) exercise filesystem operations; Debug WinFsp builds force deferred driver paths. Run Driver Verifier only in a disposable VM.
- [Microsoft CHESS](https://www.microsoft.com/en-us/research/project/chess-find-and-reproduce-heisenbugs-in-concurrent-programs/) explores and replays schedules systematically, but its native package is the 2009 x86 test-DLL host: usable for a reduced x86 C model, not the x64 Go and WinFsp process.

Do not hide a `winfsp-compat.ps1` regression by adding it to the exclusion
list.
