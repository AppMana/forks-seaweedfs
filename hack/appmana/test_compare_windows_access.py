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
            for op, n in compare.OPERATIONS.items() for i in range(5)) + (
            "SCENARIO_COMPLETE:fixture:AccessPerformance-01\n"
            "--- PASS: TestWindowsMountLab (230.00s)\n")

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
            self.load(self.text + next(line for line in self.text.splitlines() if line.startswith("ACCESS_PERF")))

    def test_failed(self):
        with self.assertRaises(ValueError):
            self.load(self.text.replace("PASS", "FAIL"))

    def test_wrong_iterations(self):
        with self.assertRaises(ValueError):
            self.load(self.text.replace("iterations=256", "iterations=1"))

    def test_non_utf8_diagnostic(self):
        self.path.write_bytes(b"diagnostic: \xff\n" + self.text.encode())
        self.assertEqual(compare.load_runs([self.path]), {op: [102.5] for op in compare.OPERATIONS})

    def test_outer_vm_failure(self):
        with self.assertRaises(ValueError):
            self.load(self.text + "--- FAIL: TestWindowsMountLab (230.00s)\n")

    def test_missing_vm_pass(self):
        with self.assertRaises(ValueError):
            self.load(self.text.replace("--- PASS: TestWindowsMountLab (230.00s)\n", ""))

    def test_missing_scenario_completion(self):
        with self.assertRaises(ValueError):
            self.load(self.text.replace("SCENARIO_COMPLETE:fixture:AccessPerformance-01\n", ""))


if __name__ == "__main__":
    unittest.main()
