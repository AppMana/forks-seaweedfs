"""Run the contract suite through GitHub's PowerShell exit-code wrapper."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent


class SmokeContractExit(unittest.TestCase):
    def run_contract(self, path):
        return subprocess.run([
            'pwsh', '-NoProfile', '-Command',
            "$ErrorActionPreference='Stop'; & $env:SMOKE_CONTRACT_PATH; "
            'if (Test-Path variable:LASTEXITCODE) { exit $LASTEXITCODE }'],
            env=dict(os.environ, SMOKE_CONTRACT_PATH=str(path)),
            capture_output=True, text=True, timeout=60)

    def test_passing_negative_controls_exit_zero_in_actions(self):
        result = self.run_contract(ROOT / 'mount-smoke-contract-test.ps1')
        self.assertIn('PASS: all 13 LFS harness contract cases', result.stdout)
        self.assertIn('PASS: ETW cleanup accepts successful flush and rejects failed flush', result.stdout)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_real_assertion_failure_still_exits_nonzero(self):
        with tempfile.TemporaryDirectory(prefix='smoke-contract-exit-') as directory:
            root = Path(directory)
            shutil.copyfile(ROOT / 'mount-smoke.ps1', root / 'mount-smoke.ps1')
            source = (ROOT / 'mount-smoke-contract-test.ps1').read_text()
            needle = "if ($script:failures -ne 0) { throw 'Healthy scenario rejected' }"
            self.assertEqual(source.count(needle), 1)
            source = source.replace(needle, "throw 'injected real assertion failure'")
            script = root / 'mount-smoke-contract-test.ps1'
            script.write_text(source)
            result = self.run_contract(script)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('injected real assertion failure', result.stderr)


if __name__ == '__main__':
    unittest.main()
