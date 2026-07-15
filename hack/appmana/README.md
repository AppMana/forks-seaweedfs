# AppMana WinFsp test harnesses

These scripts exercise the Windows `weed mount` implementation against a
self-contained SeaweedFS server. They are intended to reproduce filesystem
contract failures before changing mount implementation code.

## Git atomic rename regression

`mount-smoke.ps1` supports three focused cases:

- `NamespaceCoherence` reproduces the traced WinFsp failure: a file remains
  directly readable while its parent enumeration omits it and rename returns
  `NAME NOT FOUND`.
- `GitAtomicRename` runs `git init` on a fresh mount.
- `GitAtomicRenamePrimed` first runs the metadata and rename-over-existing
  sequence from the full smoke test, then runs `git init`. This is the exact
  reduced workload for the observed stale `config.lock` failure.

```powershell
./hack/appmana/mount-smoke.ps1 `
  -WeedExe (Resolve-Path ./weed.exe) `
  -TestCase GitAtomicRenamePrimed
```

Every invocation removes and recreates `WorkRoot` so schedule comparisons
start with the same filer, mount, and metadata-cache state. Logs are retained
under `WorkRoot\logs`; use `-Verbosity 4` for SeaweedFS callback/filer ordering
and `-WinFspOptions` for explicit WinFsp cache-option A/B runs.

### Recorded failure signature

The authoritative RED capture used Procmon's native PML recording without
WinFsp debug logging. Windows successfully created, wrote, closed, reopened,
and read `hello.txt`; after `subdir/nested.txt` was created, root
`QueryDirectory` returned only `subdir`, and `SetRenameInformationFile` for
`hello.txt` returned `NAME NOT FOUND`.

Correlated SeaweedFS logs showed the parent `ListEntries` snapshot beginning
before the deferred `CreateEntry /hello.txt`. Overlapping WinFsp close/flush
callbacks all shared one FUSE handle and entered `doFlush`; the old transaction
boundary allowed every callback to observe dirty metadata before the first
commit cleared it. `TestConcurrentFlushCommitsDeferredCreateOnce` holds the
first filer commit open and asserts that concurrent flush callbacks emit one
authoritative create.

## Schedule fuzzing and replay

Use `winfsp-cuzz.ps1` to run the focused workload under the 64-bit Application
Verifier Cuzz layer:

```powershell
./hack/appmana/winfsp-cuzz.ps1 `
  -WeedExe (Resolve-Path ./weed.exe) `
  -FuzzingLevel 4 `
  -RandomSeed 1 `
  -TraceSummary
```

Cuzz inserts delays at Win32 synchronization calls. Keep a failing seed and
rerun the same command to increase the probability of recreating the same
interleaving. The wrapper always removes AppVerifier settings when the test
finishes. It runs the filer server from an identical binary with a different
image name, so Cuzz perturbs only the WinFsp mount process. Application
Verifier is installed through the Windows SDK. `-TraceSummary` writes Weed's
existing operation counters only after unmount, so it does not perturb the
failing schedule. `-Trace` enables WinFsp's request/response log; use it only
after a seed reproduces without tracing because it changes timing substantially.

Use the tools according to what they control:

- [Application Verifier Cuzz](https://learn.microsoft.com/en-us/windows-hardware/drivers/devtest/application-verifier-tests-within-application-verifier)
  explores schedules in the real x64 `weed.exe` process.
- [Time Travel Debugging](https://learn.microsoft.com/en-us/windows-hardware/drivers/debuggercmds/time-travel-debugging-ttd-exe-command-line-util)
  records and replays one failing user-mode execution. It does not generate a
  failing schedule and its 5x-20x overhead can perturb timing.
- [WinFsp tests](https://winfsp.dev/doc/WinFsp-Testing/) exercise filesystem
  operations and Debug WinFsp builds force deferred driver paths. Driver
  Verifier should be used in a disposable Windows VM for kernel checks.
- [Microsoft CHESS](https://www.microsoft.com/en-us/research/project/chess-find-and-reproduce-heisenbugs-in-concurrent-programs/)
  systematically explores and replays schedules, but the available native
  package is the 2009 x86 test-DLL host. It is suitable for a reduced x86 C
  model, not the production x64 Go/WinFsp process.

`winfsp-compat.ps1` runs WinFsp's external filesystem suite. The rename tests
must become required gates once the adapter supports their contracts; do not
hide a known regression by expanding its exclusion list.
