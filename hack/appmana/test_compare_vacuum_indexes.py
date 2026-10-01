import tempfile
import unittest
from pathlib import Path

from compare_vacuum_indexes import compare, read_index


class VacuumIndexContract(unittest.TestCase):
    def test_loss_change_and_resurrection_fail(self):
        for after in ({}, {1: None}, {1: 11}, {1: 10, 2: 4}):
            with self.subTest(after=after):
                self.assertFalse(compare({1: 10, 2: None}, after)["passed"])

    def test_tombstone_removal_and_new_keys_are_explicit(self):
        result = compare({1: 0, 2: None}, {1: 0, 3: 7})
        self.assertTrue(result["passed"])
        self.assertEqual(result["added_live"], [3])
        self.assertEqual(result["before_live"], 1)

    def test_real_row_layout_and_last_entry_wins(self):
        for width in (4, 5):
            with self.subTest(width=width), tempfile.TemporaryDirectory() as directory:
                p = Path(directory) / "snapshot.idx"
                def row(key, offset, size):
                    return key.to_bytes(8, "big") + offset.to_bytes(width, "big") + size.to_bytes(4, "big", signed=True)
                p.write_bytes(row(1, 8, 10) + row(2, 16, 0) + row(1, 24, -1) + row(3, 0, -1))
                self.assertEqual(read_index(p, width), {1: None, 2: 0, 3: None})
                p.write_bytes(p.read_bytes()[:-1])
                with self.assertRaises(ValueError):
                    read_index(p, width)
                p.write_bytes(row(9, 0, 12))
                with self.assertRaises(ValueError):
                    read_index(p, width)


if __name__ == "__main__":
    unittest.main()
