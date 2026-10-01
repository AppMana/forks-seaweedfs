"""Check campaign routing, not Windows filesystem behavior (that needs real VMs)."""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).with_name('run-windows-vm-suite.sh')


class WindowsSuitePolicy(unittest.TestCase):
    def test_explicit_permission_matrix(self):
        source = SCRIPT.read_text()
        required = re.search(r'for name in (.*?); do', source, re.S).group(1)
        with tempfile.TemporaryDirectory() as directory:
            env = dict(os.environ)
            for name in required.replace('\\\n', ' ').split():
                env[name] = directory
            # Shell function only records scheduling; it never executes a VM.
            command = '''
go() { printf 'SCHEDULE %s %s\\n' "${SEAWEEDFS_WINDOWS_MOUNT_SCENARIO:-default}" "${SEAWEEDFS_WINDOWS_BASIC_PERMISSIONS:-unset}" >> "$POLICY_RECORD"; }
export -f go
bash "$POLICY_SCRIPT"
'''
            record = Path(directory) / 'schedule'
            env.update(POLICY_RECORD=str(record), POLICY_SCRIPT=str(SCRIPT),
                       SEAWEEDFS_WINDOWS_BASIC_PERMISSIONS='1')
            result = subprocess.run(['bash', '-c', command], env=env,
                                    capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            scheduled = record.read_text().splitlines()
            self.assertIn('SCHEDULE All 0', scheduled)
            self.assertIn('SCHEDULE All 1', scheduled)
            self.assertIn('SCHEDULE Conformance 1', scheduled)
            self.assertNotIn('SCHEDULE Conformance 0', scheduled)
            self.assertIn('SCHEDULE default 0', scheduled)


if __name__ == '__main__':
    unittest.main()
