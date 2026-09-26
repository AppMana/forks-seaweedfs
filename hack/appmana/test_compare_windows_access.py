import importlib.util
import tempfile
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location("compare", Path(__file__).with_name("compare-windows-access.py"))
compare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(compare)


class SamplesTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / "run.log"
        self.text = "--- PASS: TestWindowsAccessPerformance\n" + "".join(
            f"ACCESS_PERF sample={i} operation={op} iterations={n} ns_per_op={100+i}\n"
            for op, n in compare.OPERATIONS.items() for i in range(5))

    def load(self, text):
        self.path.write_text(text)
        return compare.load_runs([self.path])

    def test_complete(self):
        self.assertEqual(self.load(self.text), {op: [102.5] for op in compare.OPERATIONS})

    def test_missing(self):
        with self.assertRaises(ValueError):
            self.load(self.text.replace("sample=4", "sample=9"))

    def test_duplicate(self):
        with self.assertRaises(ValueError):
            self.load(self.text + self.text.splitlines()[-1])

    def test_failed(self):
        with self.assertRaises(ValueError):
            self.load(self.text.replace("PASS", "FAIL"))

    def test_wrong_iterations(self):
        with self.assertRaises(ValueError):
            self.load(self.text.replace("iterations=256", "iterations=1"))

    def test_non_utf8_diagnostic(self):
        self.path.write_bytes(b"diagnostic: \xff\n" + self.text.encode())
        self.assertEqual(compare.load_runs([self.path]), {op: [102.5] for op in compare.OPERATIONS})


if __name__ == "__main__":
    unittest.main()
