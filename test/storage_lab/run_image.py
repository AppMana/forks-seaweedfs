#!/usr/bin/env python3
"""Qualify an existing local image; never build, pull, publish, or mount host data."""
import argparse
import json
from pathlib import Path
import re
import subprocess
import tempfile
import time
import uuid


def docker(*args, check=True):
    return subprocess.run(['docker', *args], check=check, text=True,
                          stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)


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
    args = parser.parse_args()
    if not re.fullmatch(r'sha256:[0-9a-f]{64}', args.image_id):
        parser.error('--image-id must be a local immutable sha256 image ID')
    if not re.fullmatch(r'[0-9a-f]{64}', args.weed_sha256):
        parser.error('--weed-sha256 must be a SHA-256 digest')
    args.results_root.mkdir(parents=True, exist_ok=True)
    out = Path(tempfile.mkdtemp(prefix='server-image-check-', dir=args.results_root))
    name = 'seaweedfs-image-check-' + uuid.uuid4().hex
    manifest = dict(status='failed', image_id=args.image_id,
                    weed_sha256=args.weed_sha256, scope='volume entrypoint and automatic memory')
    created = False
    try:
        info = json.loads(docker('image', 'inspect', args.image_id).stdout)[0]
        if info['Id'] != args.image_id:
            raise RuntimeError('image ID mismatch')
        if any(e.startswith('GOMEMLIMIT=') for e in info['Config'].get('Env', [])):
            raise RuntimeError('image contains a GOMEMLIMIT override')
        (out / 'image.json').write_text(json.dumps(info, indent=2) + '\n')
        docker('create', '--pull=never', '--name', name, '--network=none',
               '--read-only', '--user=1000:1000', '--cap-drop=ALL',
               '--security-opt=no-new-privileges', '--memory=5g', '--memory-swap=5g',
               '--cpus=1', '--pids-limit=128', '--log-opt=max-size=8m', '--log-opt=max-file=1',
               '--tmpfs=/data:rw,uid=1000,gid=1000,size=64m',
               '--tmpfs=/tmp:rw,mode=1777,size=32m', args.image_id,
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
