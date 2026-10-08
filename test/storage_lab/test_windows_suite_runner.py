"""Test orchestration only; native filesystem qualification still requires VMs."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class WindowsSuiteRunnerTest(unittest.TestCase):
    def test_access_runner_uses_nested_module_from_any_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            command = root / 'go'
            command.write_text('''#!/usr/bin/env python3
import json, os, sys
with open(os.environ['CALLS'], 'a') as output:
    output.write(json.dumps({'cwd': os.getcwd(), 'gowork': os.getenv('GOWORK'), 'args': sys.argv[1:]}) + '\\n')
print('--- PASS: TestWindowsAccessPerformance (1.00s)')
for sample in range(5):
    for op, iterations in [('open_read_close', 256), ('handle_read', 4096)]:
        print(f'ACCESS_PERF sample={sample} operation={op} iterations={iterations} ns_per_op=100')
print('SCENARIO_COMPLETE:fixture:AccessPerformance-01')
print('--- PASS: TestWindowsMountLab (2.00s)')
''')
            command.chmod(0o755)
            baseline, candidate = root / 'baseline.exe', root / 'candidate.exe'
            baseline.write_bytes(b'baseline orchestration fixture')
            candidate.write_bytes(b'candidate orchestration fixture')
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                       GOWORK='off', RUNNER_TEMP=str(root), CALLS=str(root / 'calls.jsonl'),
                       LABCONTAINERS_LABD='fixture', LABCONTAINERS_WINDOWS_IMAGE='fixture',
                       SEAWEEDFS_WINDOWS_WINFSP_TEST=str(candidate))
            for key in list(env):
                if key.endswith('_DLL') or key == 'SEAWEEDFS_WINDOWS_WINFSP_NATIVE_PACKAGE':
                    env.pop(key)
            result = subprocess.run(['bash', str(ROOT / 'hack/appmana/windows-access-regression.sh'),
                                     str(baseline), str(candidate)], cwd=root, env=env,
                                    capture_output=True, text=True)
            calls = [json.loads(line) for line in (root / 'calls.jsonl').read_text().splitlines()]
            self.assertEqual(len(calls), 6)
            for call in calls:
                self.assertEqual(call['cwd'], str(ROOT / 'test/storage_lab/vm'))
                self.assertEqual(call['gowork'], 'off')
                self.assertEqual(call['args'][1], '.')
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_failure_does_not_hide_remaining_scenarios(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            # A fake go command tests the shell's inventory and failure handling,
            # not any application assertion or Windows behavior.
            command = root / 'go'
            command.write_text('''#!/usr/bin/env python3
import json, os, sys
with open(os.environ['CALLS'], 'a') as output:
    output.write(json.dumps({'cwd': os.getcwd(), 'gowork': os.getenv('GOWORK'), 'args': sys.argv[1:], 'scenario': os.getenv('SEAWEEDFS_WINDOWS_MOUNT_SCENARIO', ''), 'registration': os.getenv('SEAWEEDFS_WINDOWS_MOUNT_MANAGER_FROM_FSD', ''), 'cleanup': os.getenv('SEAWEEDFS_WINDOWS_MOUNT_MANAGER_CHECK_CLEANUP', ''), 'basic': os.getenv('SEAWEEDFS_WINDOWS_BASIC_PERMISSIONS', '')}) + '\\n')
sys.exit(1 if os.getenv('SEAWEEDFS_WINDOWS_MOUNT_SCENARIO') == 'Conformance' else 0)
''')
            command.chmod(0o755)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                       GOWORK='off', RUNNER_TEMP=str(root), CALLS=str(root / 'calls.jsonl'))
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
            # storage, git-lfs, 2x3 mount-manager lanes, All under both
            # permission policies, Conformance, mixed.
            self.assertEqual(len(calls), 12)
            self.assertEqual([(call['scenario'], call['basic']) for call in calls[-4:]],
                             [('All', '0'), ('All', '1'), ('Conformance', '1'), ('', '0')])
            self.assertIn('^TestMixedOSMountLab$', calls[-1]['args'])
            for mode in ['', '1']:
                for scenario in ['MountManagerDirectoryLifecycle', 'MountManagerProcessCrash', 'MountManagerRegistrationRollback']:
                    selected = [call for call in calls if call['scenario'] == scenario and call['registration'] == mode]
                    self.assertEqual(len(selected), 1)
                    self.assertEqual(selected[0]['cleanup'], '1' if scenario.endswith('DirectoryLifecycle') else '')
            for call in calls:
                self.assertIn('-count=1', call['args'])
                self.assertEqual(call['cwd'], str(ROOT / 'test/storage_lab/vm'))
                self.assertEqual(call['gowork'], 'off')
                self.assertEqual(call['args'][1], '.')
            self.assertIn('Conformance exit=1', result.stdout)
            self.assertIn('mixed exit=0', result.stdout)


if __name__ == '__main__':
    unittest.main()
