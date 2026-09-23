#!/usr/bin/env python3
"""Run precompiled regression suites without production network/filesystem access."""
import argparse
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import selectors
import subprocess
import tempfile
import time
import uuid

SUITES = {
    "storage": "Test(Vacuum|ConcurrentWriteCrossesOffsetBoundary|MakeupDiffRelocatesLargeOffsets|CommitCompact|CompactAborts|CompactByIndex|Reconcile|ApplyCompactSwap)",
    "migration": "^TestVolumeBinaryUpgradeVacuumRollback$",
    "admission": "^TestUploadLimitTimeoutIncludesReplication$",
    "synology-bootstrap": "^Test",
}

# Reviewed minimum inventories, deliberately independent of discovery from the
# test binary: deleting or renaming a required test must fail qualification.
REQUIRED_TESTS = {
    'storage': '''
TestCompactAbortsOnSourceSyncFailure
TestVacuumStableLiveDatasetHasBoundedGrowth
TestMakeupDiffRelocatesLargeOffsets
TestCommitCompactReportsReplayFailure
TestVacuumStableLiveDatasetHasBoundedAllocatedBlocks
TestReconcileAfterBothRenamesInvalidatesOldLevelDB
TestReconcileCacheInvalidationFailureRetainsMarker
TestVacuumPreservesIntactDataWithBadIndex
TestConcurrentWriteCrossesOffsetBoundary
TestReconcileNoOpWhenEachDiskIsSelfContained
TestCommitCompactDeletionTailKeepsWritable
TestCompactByIndex_RejectsDanglingNeedle
TestCompactByIndex_ConcurrentWriteDoesNotFailIntegrityCheck
TestReconcileRollForwardMarkerOnly
TestReconcileRollForwardPartialRename
TestReconcileRollBackNoMarker
TestReconcileSkipsLoadedVolumeMidVacuum
TestApplyCompactSwapMissingTempFilesPreservesLive
'''.split(),
    'migration': ['TestVolumeBinaryUpgradeVacuumRollback'],
    'admission': ['TestUploadLimitTimeoutIncludesReplication'],
    'enospc': ['TestCompactENOSPCPreservesOriginal', 'TestVolumePreallocateENOSPCFailsCreation'],
    'synology-bootstrap': '''
TestMaterializeWeed_NoImageNoOp
TestMaterializeWeed_ExtractsBinary
TestMaterializeWeed_TopLayerWins
TestMaterializeWeed_WhiteoutHides
TestMaterializeWeed_DigestPinMismatch
TestMaterializeWeed_CacheHitOffline
TestMaterializeWeed_MultipleBinaries
TestMaterializeWeed_MultiArchIndex
TestRenderArgs_BasicShape
TestValidate_Defaults
TestValidate_Required
TestInstanceConfig_Offsets
TestInstanceConfig_SingleInstanceUnchanged
TestValidate_InstancesDefaultAndBounds
TestValidate_MasterServiceSatisfiesRequirement
TestValidate_NeitherSeaweedNameNorMasterService
TestDiscoverMasters_FromMasterServiceNoCRD
TestDiscoverMasters_HeadlessServiceUsesIPNotDNSName
TestDiscoverMasters_DefaultsToServiceDNSNotPodIPs
TestDiscoverMasters_MasterServiceMissingEndpoints
TestDiscoverMasters_FromHeadlessFallback
TestDiscoverMasters_FromService
TestDiscoverMasters_PortFallback
TestMaterializeMTLS
TestMaterializeMTLS_NoOpWhenSecretNameEmpty
'''.split(),
}


def assess_results(suite, output, exit_code, boundary):
    passed = set(re.findall(r'^--- PASS: (Test\w+) \([^\n]*\)$', output, re.MULTILINE))
    required = set(REQUIRED_TESTS[suite])
    missing = sorted(required - passed)
    complete = (exit_code == 0 and not missing and boundary in output
                and '--- SKIP:' not in output and '--- FAIL:' not in output
                and re.search(r'^PASS$', output, re.MULTILINE) is not None)
    return {'status': 'passed' if complete else 'failed',
            'required_tests': sorted(required), 'passed_tests': sorted(passed),
            'missing_tests': missing}

# Runs in the SAME sandbox as the tests. A missing boundary is a fatal error.
PROBE = r'''
import os, socket
assert not os.path.exists('/home/administrator'), 'host home exposed'
assert not os.path.exists('/var/run/docker.sock'), 'host Docker exposed'
assert not os.path.exists('/dev/sda'), 'host disk exposed'
assert not os.path.exists('/dev/nvme0n1'), 'host disk exposed'
with open('/proc/net/dev') as stream:
    interfaces = {line.split(':')[0].strip() for line in stream if ':' in line}
assert interfaces == {'lo'}, 'non-loopback network exposed'
assert os.getuid() != 0, 'test payload must not run as root'
with open('/proc/self/status') as stream:
    effective = next(line.split()[1] for line in stream if line.startswith('CapEff:'))
assert int(effective, 16) == 0, 'capabilities retained'
assert 'KUBECONFIG' not in os.environ and 'AWS_ACCESS_KEY_ID' not in os.environ
s = socket.socket()
s.bind(('127.0.0.1', 0))
s.close()
assert os.statvfs('/tmp').f_blocks * os.statvfs('/tmp').f_frsize <= 2 * 1024**3
assert os.statvfs('/artifacts').f_flag & os.ST_RDONLY, 'artifacts writable'
print('PASS: sandbox boundary probe', flush=True)
'''


def artifact(value):
    path = Path(value).resolve(strict=True)
    if not path.is_file() or not os.access(path, os.X_OK):
        raise ValueError(f"not an executable file: {path}")
    return path


def digest(path):
    checksum = hashlib.sha256()
    with path.open('rb') as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b''):
            checksum.update(block)
    return checksum.hexdigest()


def require_bwrap_features():
    """Fail before staging/privilege if bounded tmpfs cannot be established."""
    result = subprocess.run(['/usr/bin/bwrap', '--help'], check=True,
                            capture_output=True, text=True, timeout=10)
    options = set(re.findall(r'--[a-z][a-z-]*', result.stdout))
    missing = {'--size', '--perms', '--remount-ro'} - options
    if missing:
        raise RuntimeError('Bubblewrap missing required isolation options: ' +
                           ', '.join(sorted(missing)) +
                           '; use the qualified Ubuntu 24.04/Bubblewrap 0.9 runner; '
                           'do not remove sandbox bounds')


def command(suite, artifacts, unit):
    # Privilege is used ONLY to establish namespaces and cgroup ceilings. The
    # payload drops to the invoking UID/GID with no capabilities or host mounts.
    cmd = ['sudo', '-n', 'systemd-run', '--quiet', '--wait', '--pipe', '--collect',
           '--service-type=exec', '--unit=' + unit,
           '-p', 'MemoryMax=4G', '-p', 'MemorySwapMax=0', '-p', 'CPUQuota=200%',
           '-p', 'TasksMax=256', '-p', 'RuntimeMaxSec=600',
           '/usr/bin/bwrap', '--unshare-all', '--die-with-parent', '--new-session',
           '--uid', str(os.getuid()), '--gid', str(os.getgid()), '--cap-drop', 'ALL',
           '--clearenv', '--ro-bind', '/usr', '/usr',
           '--symlink', 'usr/bin', '/bin', '--symlink', 'usr/lib', '/lib',
           '--symlink', 'usr/lib64', '/lib64', '--proc', '/proc', '--dev', '/dev',
           '--perms', '1777', '--size', str(2 * 1024**3), '--tmpfs', '/tmp',
           '--size', '1048576', '--tmpfs', '/artifacts', '--chdir', '/tmp',
           '--setenv', 'PATH', '/usr/bin:/bin', '--setenv', 'HOME', '/tmp',
           '--setenv', 'TMPDIR', '/tmp', '--setenv', 'GOMAXPROCS', '2']
    for name, path in sorted(artifacts.items()):
        cmd += ['--ro-bind', str(path), '/artifacts/' + name]
    cmd += ['--remount-ro', '/artifacts']
    if 'candidate' in artifacts:
        cmd += ['--setenv', 'WEED_BINARY', '/artifacts/candidate',
                '--setenv', 'WEED_CANDIDATE_BINARY', '/artifacts/candidate',
                '--setenv', 'WEED_VOLUME_BINARY', '/artifacts/' +
                ('baseline' if suite == 'migration' else 'candidate')]
    # No shell interpolation of user paths or test expressions.
    cmd += ['/bin/sh', '-ec', '/usr/bin/python3 -c "$1"; shift; exec "$@"',
            'storage-lab', PROBE, '/artifacts/tests', '-test.v', '-test.count=1',
            '-test.timeout=8m', '-test.run=' + SUITES[suite]]
    return cmd


def run_bounded(cmd, log, limit=32 * 1024**2):
    """Bound retained output as well as runtime, even for a noisy failure."""
    process = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    deadline = time.monotonic() + 630
    total = 0
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while True:
                if time.monotonic() > deadline:
                    raise TimeoutError('lab exceeded supervisor deadline')
                if not selector.select(timeout=1):
                    continue
                block = os.read(process.stdout.fileno(), 65536)
                if not block:
                    break
                total += len(block)
                if total > limit:
                    raise RuntimeError('lab output exceeded retention limit')
                log.write(block)
            return process.wait(timeout=5)
    finally:
        process.stdout.close()
        if process.poll() is None:
            process.kill()
            process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('suite', choices=SUITES)
    parser.add_argument('--tests', required=True, type=artifact)
    parser.add_argument('--candidate', type=artifact)
    parser.add_argument('--baseline', type=artifact)
    args = parser.parse_args()
    if os.getuid() == 0:
        parser.error('invoke as a normal user; the runner uses narrowly scoped sudo')
    require_bwrap_features()
    artifacts = {'tests': args.tests}
    if args.suite in ('migration', 'admission'):
        if args.candidate is None:
            parser.error('--candidate is required')
        artifacts['candidate'] = args.candidate
    if args.suite == 'migration':
        if args.baseline is None:
            parser.error('--baseline is required')
        if digest(args.baseline) == digest(args.candidate):
            parser.error('baseline and candidate have identical contents')
        artifacts['baseline'] = args.baseline
    results = Path(tempfile.mkdtemp(prefix='seaweedfs-lab-results-'))
    # bwrap drops capabilities before resolving binds; private source parents
    # may be inaccessible in its user namespace. Stage only selected binaries,
    # never chmod a caller's directory or expose a whole source/credential tree.
    required = sum(path.stat().st_size for path in artifacts.values())
    if shutil.disk_usage(results).free < required + 2 * 1024**3:
        parser.error('insufficient staging space plus 2 GiB host headroom')
    unit = 'seaweedfs-lab-' + uuid.uuid4().hex
    report = {'suite': args.suite, 'unit': unit, 'started': time.time(),
              'artifacts': {name: {'path': str(path), 'sha256': digest(path)}
                            for name, path in artifacts.items()},
              'scope': 'Linux tmpfs process/storage regression; NOT power-loss or platform qualification',
              'status': 'running'}
    report_path = results / 'manifest.json'
    report_path.write_text(json.dumps(report, indent=2) + '\n')
    print(f'Results: {results}', flush=True)
    try:
        with tempfile.TemporaryDirectory(prefix='seaweedfs-lab-artifacts-') as staging:
            os.chmod(staging, 0o755)
            staged = {}
            for name, source in artifacts.items():
                target = Path(staging) / name
                shutil.copyfile(source, target)
                target.chmod(0o555)
                if digest(target) != report['artifacts'][name]['sha256']:
                    raise RuntimeError('artifact changed during staging: ' + name)
                staged[name] = target
            with (results / 'test.log').open('wb') as log:
                exit_code = run_bounded(command(args.suite, staged, unit), log)
        report['exit_code'] = exit_code
        output = (results / 'test.log').read_text(errors='replace')
        # Go returns success when -run matches nothing or a gate is skipped.
        report.update(assess_results(args.suite, output, exit_code,
                                     'PASS: sandbox boundary probe'))
    finally:
        # Only this invocation's random unit, never a production service/PID.
        subprocess.run(['sudo', '-n', 'systemctl', 'stop', unit],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        report['finished'] = time.time()
        if report['status'] == 'running':
            report['status'] = 'interrupted'
        report_path.write_text(json.dumps(report, indent=2) + '\n')
    print(f"{report['status']}: {results / 'test.log'}", flush=True)
    return 0 if report['status'] == 'passed' else 1


if __name__ == '__main__':
    raise SystemExit(main())
