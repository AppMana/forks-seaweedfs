import unittest
import json
from pathlib import Path
import subprocess
import tempfile
from unittest import mock
import run_image
from run_image import check_memory


class ImageMemoryContract(unittest.TestCase):
    def test_soak_requires_exact_duration_multiple_cycles_and_complete_test(self):
        good = ('SOAK_COMPLETE duration_seconds=86400 cycles=2000\n'
                '--- PASS: TestS3QualificationSoak (86401s)\nPASS\n')
        run_image.check_soak_results(good, 86400)
        for bad in ('PASS\n', good.replace('86400', '30'),
                    good.replace('cycles=2000', 'cycles=1'),
                    good.replace('cycles=2000', 'cycles=0'),
                    good.replace('TestS3QualificationSoak', 'WrongTest'),
                    good.replace('--- PASS:', '--- SKIP:'),
                    good.removesuffix('PASS\n')):
            with self.subTest(log=bad), self.assertRaises(RuntimeError):
                run_image.check_soak_results(bad, 86400)

    def test_s3_inventory_rejects_missing_skipped_failed_and_nonterminal_results(self):
        good = ''.join('--- PASS: ' + name + ' (0.1s)\n' for name in run_image.S3_TESTS) + 'PASS\n'
        run_image.check_s3_results(good)
        for bad in ('PASS\n', good.replace(run_image.S3_TESTS[0], 'WrongTest'),
                    good + '--- SKIP: Child (0.1s)\n', good + '--- FAIL: Child (0.1s)\n',
                    good.removesuffix('PASS\n')):
            with self.subTest(log=bad), self.assertRaises(RuntimeError):
                run_image.check_s3_results(bad)

    def test_exact_cgroup_plan_and_negative_controls(self):
        good = ('memory limits: available 5368709120 (cgroup "/sys/fs/cgroup" '
                'limit 5368709120, physical 68719476736), GOMEMLIMIT env false, '
                'Go memory limit 4831838208 (set true), upload admission 2496 MiB '
                '(auto true), download admission 832 MiB (auto true)')
        check_memory(good)
        for bad in ['', good.replace('2496', '3072'), good.replace('832', '1024'),
                    good.replace('env false', 'env true'),
                    good.replace('(auto true)', '(auto false)'),
                    good.replace('(set true)', '(set false)'),
                    good.replace('4831838208', '4831838209')]:
            with self.subTest(log=bad), self.assertRaises(RuntimeError):
                check_memory(bad)

    def test_failure_retains_results_and_cleans_only_owned_container(self):
        image_id = 'sha256:' + 'a' * 64
        commands = []

        def execute(*args, **kwargs):
            commands.append(args)
            output = ''
            if args[:2] == ('image', 'inspect'):
                output = json.dumps([{'Id': image_id, 'Config': {'Env': []}}])
            elif 'curl' in args:
                output = '{}'
            elif 'sha256sum' in args:
                output = 'b' * 64 + ' /usr/bin/weed'
            return subprocess.CompletedProcess(args, 0, output)

        with tempfile.TemporaryDirectory() as root, mock.patch.object(
                run_image, 'docker', side_effect=execute), mock.patch(
                'sys.argv', ['run_image', '--image-id', image_id,
                             '--weed-sha256', 'a' * 64, '--results-root', root]):
            self.assertEqual(run_image.main(), 1)
            result = json.loads(next(Path(root).glob('*/manifest.json')).read_text())
            self.assertEqual(result['status'], 'failed')
            self.assertIn('hash mismatch', result['error'])
        create = next(cmd for cmd in commands if cmd[0] == 'create')
        for flag in ('--pull=never', '--network=none', '--read-only',
                     '--user=1000:1000', '--cap-drop=ALL', '--memory-swap=5g'):
            self.assertIn(flag, create)
        self.assertNotIn('--publish', create)
        self.assertNotIn('--volume', create)
        name = create[create.index('--name') + 1]
        self.assertTrue(name.startswith('seaweedfs-image-check-'))
        self.assertEqual(commands[-1], ('rm', '--force', '--volumes', name))

    def test_ci_runs_image_gate_and_retains_evidence(self):
        workflow = (Path(__file__).resolve().parents[2] /
                    '.github/workflows/appmana-storage-reliability.yml').read_text()
        self.assertIn('python3 test/storage_lab/run_image.py', workflow)
        self.assertIn('--s3-suite "$RUNNER_TEMP/s3-copying.test"', workflow)
        self.assertIn('--soak-seconds 30', workflow)
        self.assertEqual(workflow.count("'test/s3/copying/**'"), 2)
        self.assertIn('${{ runner.temp }}/server-image-results/', workflow)
