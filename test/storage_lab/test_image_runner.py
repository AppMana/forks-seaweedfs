import unittest
import json
from pathlib import Path
import subprocess
import tempfile
from unittest import mock
import run_image
from run_image import check_memory


class ImageMemoryContract(unittest.TestCase):
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
        self.assertIn('${{ runner.temp }}/server-image-results/', workflow)
