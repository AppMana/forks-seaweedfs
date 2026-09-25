"""Test orchestration only; native filesystem qualification still requires VMs."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class WindowsSuiteRunnerTest(unittest.TestCase):
    def test_failure_does_not_hide_remaining_scenarios(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            # A fake go command tests the shell's inventory and failure handling,
            # not any application assertion or Windows behavior.
            command = root / 'go'
            command.write_text('''#!/usr/bin/env python3
import json, os, sys
with open(os.environ['CALLS'], 'a') as output:
    output.write(json.dumps({'args': sys.argv[1:], 'scenario': os.getenv('SEAWEEDFS_WINDOWS_MOUNT_SCENARIO', ''), 'registration': os.getenv('SEAWEEDFS_WINDOWS_MOUNT_MANAGER_FROM_FSD', ''), 'cleanup': os.getenv('SEAWEEDFS_WINDOWS_MOUNT_MANAGER_CHECK_CLEANUP', '')}) + '\\n')
sys.exit(1 if os.getenv('SEAWEEDFS_WINDOWS_MOUNT_SCENARIO') == 'Conformance' else 0)
''')
            command.chmod(0o755)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                       RUNNER_TEMP=str(root), CALLS=str(root / 'calls.jsonl'))
            inputs = '''LABCONTAINERS_LABD LABCONTAINERS_WINDOWS_IMAGE LABCONTAINERS_VM_IMAGE
SEAWEEDFS_WINDOWS_WEED SEAWEEDFS_LINUX_WEED SEAWEEDFS_WINDOWS_STORAGE_TEST
SEAWEEDFS_WINDOWS_MOUNT_UNIT_TEST SEAWEEDFS_WINDOWS_WINFSP_TEST
SEAWEEDFS_WINDOWS_WINFSP_DLL SEAWEEDFS_WINFSP_MSI SEAWEEDFS_GIT_INSTALLER
SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC SEAWEEDFS_WINDOWS_CONFORMANCE_EXE
SEAWEEDFS_MIXED_LINUX_WORKLOAD SEAWEEDFS_MIXED_WINDOWS_WORKLOAD'''.split()
            env.update({name: 'fixture' for name in inputs})
            env['SEAWEEDFS_LAB_ARTIFACTS'] = str(root)
            result = subprocess.run(['bash', str(ROOT / 'hack/appmana/run-windows-vm-suite.sh')],
                                    env=env, capture_output=True, text=True)
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            calls = [json.loads(line) for line in (root / 'calls.jsonl').read_text().splitlines()]
            self.assertEqual(len(calls), 11)
            self.assertEqual([call['scenario'] for call in calls[-3:]], ['All', 'Conformance', ''])
            self.assertIn('^TestMixedOSMountLab$', calls[-1]['args'])
            for mode in ['', '1']:
                for scenario in ['MountManagerDirectoryLifecycle', 'MountManagerProcessCrash', 'MountManagerRegistrationRollback']:
                    selected = [call for call in calls if call['scenario'] == scenario and call['registration'] == mode]
                    self.assertEqual(len(selected), 1)
                    self.assertEqual(selected[0]['cleanup'], '1' if scenario.endswith('DirectoryLifecycle') else '')
            for call in calls:
                self.assertIn('-count=1', call['args'])
            self.assertIn('Conformance exit=1', result.stdout)
            self.assertIn('mixed exit=0', result.stdout)


if __name__ == '__main__':
    unittest.main()
