#!/usr/bin/env python3
"""Run storage regressions on disposable native XFS or Btrfs images."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import uuid

import run as lab


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('filesystem', choices=('xfs', 'btrfs'))
    parser.add_argument('--tests', required=True, type=lab.artifact)
    parser.add_argument('--enospc', action='store_true',
                        help='run the destructive-to-image disk-full preservation test')
    args = parser.parse_args()
    if os.getuid() == 0:
        parser.error('invoke as a normal user')
    for command in ('sudo', 'systemd-run', 'unshare', 'bwrap', 'losetup',
                    'mkfs.' + args.filesystem):
        if shutil.which(command) is None:
            parser.error('missing required command: ' + command)
    checker = 'xfs_repair' if args.filesystem == 'xfs' else 'btrfs'
    if shutil.which(checker) is None:
        parser.error('missing required checker: ' + checker)
    lab.require_bwrap_features()
    usage = shutil.disk_usage('/tmp')
    if usage.free < 4 * 1024**3:
        parser.error('less than 4 GiB host headroom')

    results = Path(tempfile.mkdtemp(prefix='seaweedfs-fs-results-'))
    unit = 'seaweedfs-fs-lab-' + uuid.uuid4().hex
    mode = 'enospc' if args.enospc else 'normal'
    image_size = 2 * 1024**3 if args.enospc else 8 * 1024**3
    test_regex = ('^(TestCompactENOSPCPreservesOriginal|'
                  'TestVolumePreallocateENOSPCFailsCreation)$'
                  if args.enospc else lab.SUITES['storage'])
    report = {
        'suite': 'native-filesystem', 'filesystem': args.filesystem,
        'mode': mode,
        'unit': unit, 'started': time.time(), 'status': 'running',
        'tests': {'path': str(args.tests), 'sha256': lab.digest(args.tests)},
        'scope': 'Disposable file-backed native filesystem; not power-loss or physical-device qualification',
    }
    manifest = results / 'manifest.json'
    manifest.write_text(json.dumps(report, indent=2) + '\n')
    print('Results:', results, flush=True)
    try:
        with tempfile.TemporaryDirectory(prefix='seaweedfs-fs-lab-') as work:
            os.chmod(work, 0o755)
            image = Path(work) / 'disk.img'
            with image.open('wb') as stream:
                stream.truncate(image_size)
            staged_tests = Path(work) / 'storage.test'
            shutil.copyfile(args.tests, staged_tests)
            staged_tests.chmod(0o555)
            if lab.digest(staged_tests) != report['tests']['sha256']:
                raise RuntimeError('test artifact changed during staging')
            helper = Path(__file__).with_name('filesystem_helper.sh').resolve()
            cmd = [
                'sudo', '-n', 'systemd-run', '--quiet', '--wait', '--pipe', '--collect',
                '--service-type=exec', '--unit=' + unit,
                '-p', 'MemoryMax=6G', '-p', 'MemorySwapMax=0', '-p', 'CPUQuota=200%',
                '-p', 'TasksMax=256', '-p', 'RuntimeMaxSec=900',
                '/usr/bin/unshare', '--mount', '--net', '--pid', '--fork', '--mount-proc',
                str(helper), str(image), args.filesystem, str(staged_tests),
                str(os.getuid()), str(os.getgid()), test_regex,
                str(image_size), mode,
            ]
            with (results / 'test.log').open('wb') as log:
                code = lab.run_bounded(cmd, log, limit=32 * 1024**2)
            report['exit_code'] = code
            output = (results / 'test.log').read_text(errors='replace')
            report.update(lab.assess_results('enospc' if args.enospc else 'storage',
                                            output, code,
                                            'PASS: native filesystem boundary probe'))
    finally:
        subprocess.run(['sudo', '-n', 'systemctl', 'stop', unit],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        report['finished'] = time.time()
        if report['status'] == 'running':
            report['status'] = 'interrupted'
        manifest.write_text(json.dumps(report, indent=2) + '\n')
    print(f"{report['status']}: {results / 'test.log'}", flush=True)
    return 0 if report['status'] == 'passed' else 1


if __name__ == '__main__':
    raise SystemExit(main())
