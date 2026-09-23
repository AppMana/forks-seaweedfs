"""Execute the real CI input gate and media-builder rejection paths, without VMs."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest

ROOT = Path(__file__).resolve().parents[2]


class WindowsMSVCWorkflowTest(unittest.TestCase):
    def test_privileged_jobs_exclude_pull_requests(self):
        workflow = (ROOT / '.github/workflows/appmana-storage-reliability.yml').read_text()
        native = workflow.split('  windows-msvc-qualification:\n', 1)[1].split('    runs-on:', 1)[0]
        self.assertIn("(github.event_name == 'workflow_dispatch' && inputs.windows_msvc_build)", native)
        self.assertIn("(github.event_name == 'push' && vars.SEAWEEDFS_RELIABILITY_WINFSP_MSVC_ENABLED == '1')", native)
        faults = workflow.split('  vm-fault-gates:\n', 1)[1].split('    runs-on:', 1)[0]
        self.assertIn("if: ${{ github.event_name == 'push' || github.event_name == 'workflow_dispatch' }}", faults)

    def test_all_native_payloads_require_matching_pins(self):
        workflow = (ROOT / '.github/workflows/appmana-storage-reliability.yml').read_text()
        start = workflow.index('          for pair in ')
        end = workflow.index('          done\n', start) + len('          done\n')
        script = textwrap.dedent(workflow[start:end])
        pairs = [
            ('SEAWEEDFS_WINDOWS_MSVC_ISO', 'SEAWEEDFS_WINDOWS_MSVC_ISO_SHA256'),
            ('SEAWEEDFS_WINFSP_MSI', 'WINFSP_MSI_SHA256'),
            ('SEAWEEDFS_GIT_INSTALLER', 'GIT_INSTALLER_SHA256'),
            ('SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC', 'GIT_LFS_SHA256'),
        ]
        with tempfile.TemporaryDirectory(prefix='msvc-input-contract-') as directory:
            fixture = Path(directory) / 'payload with spaces'
            fixture.write_bytes(b'offline-payload-fixture')
            digest = hashlib.sha256(fixture.read_bytes()).hexdigest()
            env = dict(os.environ)
            for path, checksum in pairs:
                env[path], env[checksum] = str(fixture), digest
            def run(values):
                return subprocess.run(['bash', '-euo', 'pipefail', '-c', script],
                                      env=values, capture_output=True, text=True)
            self.assertEqual(run(env).returncode, 0)
            for path, checksum in pairs:
                for key, value in [(path, ''), (path, fixture.name),
                                   (path, str(fixture) + '.missing'),
                                   (checksum, ''), (checksum, 'latest'),
                                   (checksum, '0' * 64)]:
                    with self.subTest(variable=key, value=value):
                        changed = dict(env, **{key: value})
                        self.assertNotEqual(run(changed).returncode, 0)

    def test_media_builder_rejects_unverified_payload_before_staging(self):
        builder = ROOT / 'hack/appmana/prepare-winfsp-msvc-media.sh'
        with tempfile.TemporaryDirectory(prefix='msvc-media-reject-') as directory:
            root = Path(directory)
            bad = root / 'bad-payload'
            bad.write_bytes(b'not the reviewed image layer')
            result = subprocess.run(['bash', str(builder), str(bad), str(root),
                                     str(bad), str(root)], capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(list(root.iterdir()), [bad])


if __name__ == '__main__':
    unittest.main()
