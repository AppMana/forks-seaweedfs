#!/usr/bin/env python3
"""Qualify an existing local image; only an optional read-only test binary is mounted."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile
import time
import uuid


def docker(*args, check=True, timeout=30):
    return subprocess.run(['docker', *args], check=check, text=True,
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)


S3_TESTS = ('TestBasicPutGet', 'TestBasicLargeObject', 'TestObjectCopySameBucket',
            'TestObjectCopyDiffBucket', 'TestMultipartCopySmall',
            'TestMultipartCopyWithoutRange', 'TestMultipartCompleteAndAbortPreservesObject')


def check_s3_results(log, required=S3_TESTS):
    passed = set(re.findall(r'^--- PASS: (\w+) ', log, re.MULTILINE))
    if set(required) - passed or re.search(r'--- (?:SKIP|FAIL):', log):
        raise RuntimeError('S3 suite did not pass its complete required inventory')
    if not re.search(r'^PASS$', log, re.MULTILINE):
        raise RuntimeError('S3 suite did not report terminal PASS')


def check_soak_results(log, seconds):
    check_s3_results(log, ('TestS3QualificationSoak',))
    if not re.search(r'^SOAK_COMPLETE duration_seconds=' + str(seconds) +
                     r' cycles=(?:[2-9]|[1-9][0-9]+)$', log, re.MULTILINE):
        raise RuntimeError('soak completion/duration inventory missing')


def docker_logged(path, *args, timeout):
    # Persist progress as it happens, including if the controller is interrupted.
    with path.open('w') as log:
        result = subprocess.run(['docker', *args], stdout=log,
                                stderr=subprocess.STDOUT, timeout=timeout)
    return subprocess.CompletedProcess(result.args, result.returncode, path.read_text())


def run_s3(name, suite, out, soak_seconds=0):
    docker('exec', name, 'mkdir', '/tmp/filer')
    docker('exec', '--detach', '--workdir=/tmp/filer', name, '/bin/sh', '-c',
           'exec /usr/bin/weed filer -ip=127.0.0.1 -master=127.0.0.1:9333 '
           '>/tmp/filer.log 2>&1')
    docker('exec', '--detach', name, '/bin/sh', '-c',
           'exec /usr/bin/weed s3 -ip.bind=127.0.0.1 -filer=127.0.0.1:8888 '
           '>/tmp/s3.log 2>&1')
    try:
        deadline = time.monotonic() + 45
        while docker('exec', name, 'curl', '-fsS', '--max-time', '2',
                     'http://127.0.0.1:8333/', check=False).returncode:
            if time.monotonic() >= deadline:
                raise RuntimeError('isolated S3 endpoint did not become ready')
            time.sleep(1)
        result = docker('exec', '-e', 'S3_ENDPOINT=http://127.0.0.1:8333',
                        '-e', 'MASTER_ENDPOINT=http://127.0.0.1:9333',
                        name, '/s3-copying.test', '-test.v', '-test.count=1',
                        '-test.timeout=5m', '-test.run=^(' + '|'.join(S3_TESTS) + ')$',
                        check=False, timeout=320)
        (out / 's3-tests.log').write_text(result.stdout)
        if result.returncode:
            raise RuntimeError('S3 suite failed; see s3-tests.log')
        check_s3_results(result.stdout)
        if soak_seconds:
            result = docker_logged(out / 'soak-tests.log', 'exec', '-e', 'S3_ENDPOINT=http://127.0.0.1:8333',
                            '-e', 'MASTER_ENDPOINT=http://127.0.0.1:9333',
                            '-e', 'SEAWEEDFS_ISOLATED_IMAGE=1',
                            '-e', 'SEAWEEDFS_SOAK_SECONDS=' + str(soak_seconds),
                            name, '/s3-copying.test', '-test.v', '-test.count=1',
                            '-test.timeout=' + str(soak_seconds + 600) + 's',
                            '-test.run=^TestS3QualificationSoak$',
                            timeout=soak_seconds + 620)
            if result.returncode:
                raise RuntimeError('soak failed; see soak-tests.log')
            check_soak_results(result.stdout, soak_seconds)
    finally:
        for service in ('filer', 's3'):
            log = docker('exec', name, 'cat', '/tmp/' + service + '.log', check=False)
            (out / (service + '.log')).write_text(log.stdout)


def check_memory(log):
    # Independent fixed expectation for a 5 GiB cgroup: 90% Go limit,
    # minus 1.25 GiB reserve, then 3:1 upload/download, truncated to MiB.
    expected = (r'available 5368709120 \(cgroup "[^"]+" limit 5368709120, '
                r'physical \d+\), GOMEMLIMIT env false, Go memory limit '
                r'4831838208 \(set true\), upload admission 2496 MiB '
                r'\(auto true\), download admission 832 MiB \(auto true\)')
    if not re.search(expected, log):
        raise RuntimeError('5 GiB automatic memory/admission contract not observed')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image-id', required=True)
    parser.add_argument('--weed-sha256', required=True)
    parser.add_argument('--results-root', required=True, type=Path)
    parser.add_argument('--s3-suite', type=Path,
                        help='optional static Go test binary from test/s3/copying')
    parser.add_argument('--soak-seconds', type=int, default=0,
                        help='intensive fixed-live-data S3/vacuum lane, up to 86400 seconds')
    args = parser.parse_args()
    if not 0 <= args.soak_seconds <= 86400 or (args.soak_seconds and not args.s3_suite):
        parser.error('--soak-seconds requires --s3-suite and must be 0..86400')
    if not re.fullmatch(r'sha256:[0-9a-f]{64}', args.image_id):
        parser.error('--image-id must be a local immutable sha256 image ID')
    if not re.fullmatch(r'[0-9a-f]{64}', args.weed_sha256):
        parser.error('--weed-sha256 must be a SHA-256 digest')
    args.results_root.mkdir(parents=True, exist_ok=True)
    out = Path(tempfile.mkdtemp(prefix='server-image-check-', dir=args.results_root))
    name = 'seaweedfs-image-check-' + uuid.uuid4().hex
    manifest = dict(status='failed', image_id=args.image_id,
                    weed_sha256=args.weed_sha256, scope='volume entrypoint and automatic memory')
    manifest['soak_seconds'] = args.soak_seconds
    if args.s3_suite:
        args.s3_suite = args.s3_suite.resolve(strict=True)
        manifest.update(scope='volume memory and isolated S3 payload/copy checks',
                        s3_suite_sha256=hashlib.sha256(args.s3_suite.read_bytes()).hexdigest(),
                        s3_required_tests=list(S3_TESTS))
    created = False
    print('Starting qualification; retained progress: ' + str(out), flush=True)
    try:
        info = json.loads(docker('image', 'inspect', args.image_id).stdout)[0]
        if info['Id'] != args.image_id:
            raise RuntimeError('image ID mismatch')
        if any(e.startswith('GOMEMLIMIT=') for e in info['Config'].get('Env', [])):
            raise RuntimeError('image contains a GOMEMLIMIT override')
        (out / 'image.json').write_text(json.dumps(info, indent=2) + '\n')
        test_mount = []
        if args.s3_suite:
            test_mount = ['--mount', 'type=bind,src=' + str(args.s3_suite) +
                          ',dst=/s3-copying.test,readonly']
        docker('create', *test_mount, '--pull=never', '--name', name, '--network=none',
               '--read-only', '--user=1000:1000', '--cap-drop=ALL',
               '--security-opt=no-new-privileges', '--memory=5g', '--memory-swap=5g',
               '--cpus=1', '--pids-limit=128', '--log-opt=max-size=8m', '--log-opt=max-file=1',
               '--tmpfs=/data:rw,uid=1000,gid=1000,size=512m',
               '--tmpfs=/tmp:rw,mode=1777,size=128m', args.image_id,
               'volume', '-ip=127.0.0.1', '-mserver=127.0.0.1:9333')
        created = True
        docker('start', name)
        # Volume startup fetches the master's configuration before listening.
        # Keep its real master on loopback in this same isolated container.
        docker('exec', name, 'mkdir', '/tmp/master')
        docker('exec', '--detach', name, '/usr/bin/weed', 'master',
               '-mdir=/tmp/master', '-ip=127.0.0.1', '-volumeSizeLimitMB=32')
        deadline = time.monotonic() + 45
        while True:
            health = docker('exec', name, 'curl', '-fsS', '--max-time', '2',
                            'http://127.0.0.1:8080/status', check=False)
            if health.returncode == 0:
                json.loads(health.stdout)
                (out / 'status.json').write_text(health.stdout)
                break
            if time.monotonic() >= deadline:
                raise RuntimeError('volume HTTP status did not become ready: ' + health.stdout)
            time.sleep(1)
        actual = docker('exec', name, 'sha256sum', '/usr/bin/weed').stdout.split()[0]
        if actual != args.weed_sha256:
            raise RuntimeError('packaged executable hash mismatch')
        (out / 'version.txt').write_text(docker('exec', name, '/usr/bin/weed', 'version').stdout)
        check_memory(docker('logs', name).stdout)
        if args.s3_suite:
            run_s3(name, args.s3_suite, out, args.soak_seconds)
        manifest['status'] = 'passed'
    except Exception as exc:
        manifest['error'] = str(exc)
    finally:
        if created:
            try:
                (out / 'volume.log').write_text(docker('logs', name).stdout)
                docker('stop', '--time=5', name)
                (out / 'container.json').write_text(docker('inspect', name).stdout)
            except Exception as exc:
                manifest.update(status='failed', cleanup_error=str(exc))
            finally:
                try:
                    docker('rm', '--force', '--volumes', name)
                except Exception as exc:
                    manifest.update(status='failed', cleanup_error=str(exc))
        (out / 'manifest.json').write_text(json.dumps(manifest, indent=2) + '\n')
        print(out, flush=True)
        print(json.dumps(manifest), flush=True)
    return 0 if manifest['status'] == 'passed' else 1


if __name__ == '__main__':
    raise SystemExit(main())
