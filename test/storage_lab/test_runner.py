import importlib.util
import os
from pathlib import Path
import unittest
import tempfile
import sys
import subprocess
from unittest import mock
from offset_mutant import BROKEN, FIXED, revert
from compaction_mutant import MUTATIONS, mutate
from fsck_mutant import MUTATIONS as FSCK_MUTATIONS, mutate as mutate_fsck
from preallocate_mutant import (BROKEN as PREALLOC_BROKEN, FIXED as PREALLOC_FIXED,
                                revert as revert_preallocate)

spec = importlib.util.spec_from_file_location('lab', Path(__file__).with_name('run.py'))
lab = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lab)


class IsolationContract(unittest.TestCase):
    def test_bwrap_requires_bounded_tmpfs_features(self):
        for help_text, accepted in [('--size BYTES\n--perms OCTAL\n--remount-ro DEST', True),
                                    ('--tmpfs DEST\n--remount-ro DEST', False),
                                    ('--size BYTES\n--remount-ro DEST', False)]:
            with self.subTest(help=help_text), mock.patch.object(lab.subprocess, 'run',
                    return_value=subprocess.CompletedProcess([], 0, help_text, '')):
                if accepted:
                    lab.require_bwrap_features()
                else:
                    with self.assertRaisesRegex(RuntimeError, 'Bubblewrap.*missing'):
                        lab.require_bwrap_features()

    def test_hosted_storage_runner_has_supported_bubblewrap(self):
        workflow = (Path(__file__).resolve().parents[2] /
                    '.github/workflows/appmana-storage-reliability.yml').read_text()
        job = workflow.split('  large-disk-regressions:\n', 1)[1].split('\n  vm-fault-gates:', 1)[0]
        self.assertIn('runs-on: ubuntu-24.04', job)

    def test_required_inventory_rejects_missing_skipped_or_failed_tests(self):
        for suite, names in lab.REQUIRED_TESTS.items():
            lines = ['--- PASS: ' + name + ' (0.01s)' for name in names]
            output = 'boundary\n' + '\n'.join(lines) + '\nPASS\n'
            self.assertEqual(lab.assess_results(suite, output, 0, 'boundary')['status'], 'passed')
            for name in names:
                incomplete = output.replace('--- PASS: ' + name + ' (0.01s)\n', '')
                self.assertIn(name, lab.assess_results(suite, incomplete, 0, 'boundary')['missing_tests'])
            for broken, code in [(output, 1), (output.replace('boundary', ''), 0),
                                 (output.replace('\nPASS\n', '\n'), 0),
                                 (output + '    --- SKIP: TestExtra/subtest (0s)\n', 0),
                                 (output + '--- FAIL: TestExtra (0s)\n', 0)]:
                self.assertEqual(lab.assess_results(suite, broken, code, 'boundary')['status'], 'failed')

    def test_missing_storage_tests_cannot_pass_on_one_success(self):
        def one_test_only(cmd, log, **kwargs):
            log.write(b'PASS: sandbox boundary probe\n--- PASS: TestVacuumStableLiveDatasetHasBoundedGrowth (0.01s)\nPASS\n')
            return 0
        with mock.patch.object(sys, 'argv', ['run.py', 'storage', '--tests', sys.executable]), \
                mock.patch.object(lab, 'require_bwrap_features'), \
                mock.patch.object(lab, 'run_bounded', side_effect=one_test_only), \
                mock.patch.object(lab.subprocess, 'run'):
            self.assertEqual(lab.main(), 1, 'a partial suite was reported as qualified')

    def test_filesystem_helper_rejects_physical_device_before_privileged_work(self):
        helper = Path(__file__).with_name('filesystem_helper.sh')
        result = subprocess.run([str(helper), '/dev/sda', 'xfs', '/tmp/tests',
                                 str(os.getuid()), str(os.getgid()), 'TestVacuum',
                                 str(8 * 1024**3), 'normal'],
                                text=True, capture_output=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('unsafe image path', result.stderr)
        self.assertNotIn('mkfs', result.stdout + result.stderr)

    def test_compaction_mutants_replace_exact_reviewed_block(self):
        for kind, (_, old, new) in MUTATIONS.items():
            self.assertEqual(mutate(kind, 'before\n' + old + 'after'),
                             'before\n' + new + 'after')
            with self.assertRaises(ValueError):
                mutate(kind, old + old)

    def test_preallocation_mutant_reverts_exact_reviewed_block(self):
        self.assertEqual(revert_preallocate('before\n' + PREALLOC_FIXED + 'after'),
                         'before\n' + PREALLOC_BROKEN + 'after')
        for source in ['', PREALLOC_BROKEN, PREALLOC_FIXED + PREALLOC_FIXED]:
            with self.assertRaises(ValueError):
                revert_preallocate(source)

    def test_fsck_mutants_replace_exact_reviewed_block(self):
        for kind, (old, new) in FSCK_MUTATIONS.items():
            self.assertEqual(mutate_fsck(kind, 'before\n' + old + 'after'),
                             'before\n' + new + 'after')
            with self.assertRaises(ValueError):
                mutate_fsck(kind, old + old)

    def test_output_is_bounded(self):
        with tempfile.TemporaryFile() as log:
            with self.assertRaisesRegex(RuntimeError, 'retention limit'):
                lab.run_bounded([sys.executable, '-c', 'print("x" * 4096)'], log, limit=128)
            self.assertLessEqual(log.tell(), 128)

    def test_mutant_reverts_exactly_one_known_fix(self):
        self.assertEqual(revert('before\n' + FIXED + '\nafter'),
                         'before\n' + BROKEN + '\nafter')
        for source in ['', BROKEN, FIXED + FIXED]:
            with self.assertRaises(ValueError):
                revert(source)

    def test_no_host_network_or_writable_bind(self):
        cmd = lab.command('storage', {'tests': Path('/tmp/tests')}, 'seaweedfs-lab-test')
        self.assertIn('--unshare-all', cmd)
        self.assertIn('--clearenv', cmd)
        for forbidden in ['--share-net', '--bind', '--dev-bind', '--ro-bind-try']:
            self.assertNotIn(forbidden, cmd)
        sources = [cmd[i + 1] for i, value in enumerate(cmd) if value == '--ro-bind']
        self.assertEqual(sources, ['/usr', '/tmp/tests'])
        for limit in ['MemoryMax=4G', 'MemorySwapMax=0', 'CPUQuota=200%',
                      'TasksMax=256', 'RuntimeMaxSec=600']:
            self.assertIn(limit, cmd)

    def test_migration_and_admission_select_correct_volume_binary(self):
        artifacts = {'tests': Path('/tmp/tests'), 'candidate': Path('/tmp/new'),
                     'baseline': Path('/tmp/old')}
        for suite, expected in [('migration', 'baseline'), ('admission', 'candidate')]:
            cmd = lab.command(suite, artifacts, 'seaweedfs-lab-test')
            index = cmd.index('WEED_VOLUME_BINARY')
            self.assertEqual(cmd[index + 1], '/artifacts/' + expected)

    def test_probe_precedes_exec_and_no_path_interpolation(self):
        path = Path('/tmp/file with spaces; touch bad')
        cmd = lab.command('storage', {'tests': path}, 'seaweedfs-lab-test')
        self.assertIn(str(path), cmd)
        self.assertIn(lab.PROBE, cmd)
        self.assertIn('/usr/bin/python3 -c "$1"; shift; exec "$@"', cmd)


if __name__ == '__main__':
    unittest.main()
