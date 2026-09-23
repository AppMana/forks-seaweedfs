"""Execute the actual workflow's optional LFS hash gate, without a VM."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest


class WindowsLFSWorkflowTest(unittest.TestCase):
    def test_optional_client_pin(self):
        workflow = (Path(__file__).resolve().parents[2] /
                    ".github/workflows/appmana-storage-reliability.yml").read_text()
        start = workflow.index('          if [[ -n "$SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC"')
        end = workflow.index('          fi\n', start) + len('          fi\n')
        script = textwrap.dedent(workflow[start:end])
        with tempfile.TemporaryDirectory(prefix="lfs-workflow-contract-") as directory:
            root = Path(directory)
            binary = root / "client with spaces.exe"
            binary.write_bytes(b"official-client-fixture")
            digest = hashlib.sha256(binary.read_bytes()).hexdigest()
            cases = [
                ("bundled", "", "", True),
                ("explicit", str(binary), digest, True),
                ("missing hash", str(binary), "", False),
                ("missing path", "", digest, False),
                ("bad hash", str(binary), "0" * 64, False),
                ("malformed hash", str(binary), "latest", False),
                ("relative path", binary.name, digest, False),
                ("missing file", str(root / "absent.exe"), digest, False),
            ]
            for name, path, checksum, success in cases:
                with self.subTest(name=name):
                    summary = root / (name + ".summary")
                    env = dict(os.environ,
                               SEAWEEDFS_WINDOWS_GIT_LFS_DIAGNOSTIC=path,
                               GIT_LFS_SHA256=checksum,
                               GITHUB_STEP_SUMMARY=str(summary))
                    result = subprocess.run(["bash", "-euo", "pipefail", "-c", script],
                                            env=env, capture_output=True, text=True)
                    self.assertEqual(result.returncode == 0, success, result.stderr)
                    if success:
                        self.assertIn(digest if path else "pinned Git installer bundle",
                                      summary.read_text())
                    else:
                        self.assertFalse(summary.exists(), "failed pin announced success")


if __name__ == "__main__":
    unittest.main()
