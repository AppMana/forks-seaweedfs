import ast
import fnmatch
from pathlib import Path
import re
import unittest


class ServerImageWorkflowTests(unittest.TestCase):
    def test_push_builds_maintained_release_only(self):
        root = Path(__file__).resolve().parents[2]
        workflow = (root / '.github/workflows/appmana-server-images.yml').read_text()
        push = re.search(r'^  push:\n(.*?)(?=^  \w)', workflow, re.M | re.S)
        self.assertIsNotNone(push)
        branches = re.search(r'^    branches: (\[.*\])$', push.group(1), re.M)
        self.assertIsNotNone(branches)
        patterns = ast.literal_eval(branches.group(1))
        for branch, builds in [('merge/upstream-4.51', True),
                               ('merge/upstream-4.48', False),
                               ('feature/test', False)]:
            with self.subTest(branch=branch):
                self.assertEqual(any(fnmatch.fnmatchcase(branch, p) for p in patterns), builds)
