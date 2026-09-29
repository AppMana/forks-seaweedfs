import json
import pathlib
import shutil
import subprocess
import tempfile
import unittest


class FileIOSelection(unittest.TestCase):
    def test_adjacent_events_and_cross_thread_completion(self):
        shell = shutil.which('pwsh')
        if not shell:
            self.skipTest('PowerShell required')
        def event(process, operation, irp):
            return (f'<Event><System><Execution ProcessID="{process}" ThreadID="9"/>'
                    '<TimeCreated SystemTime="2026-09-29T00:00:00Z"/></System>'
                    f'<EventData><Data Name="IrpPtr">{irp}</Data></EventData>'
                    f'<RenderingInfo><EventName xmlns="http://schemas.microsoft.com/win/2004/08/events/trace">FileIo</EventName><Opcode>{operation}</Opcode>'
                    '</RenderingInfo></Event>')
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            source, output = root / 'trace.xml', root / 'selected.jsonl'
            source.write_text('<Events>' + event(42, 'Create', '0x123') +
                              event(4, 'OperationEnd', '0x123') +
                              event(4, 'OperationEnd', '0x999') + '</Events>')
            command = [shell, '-NoProfile', '-File', str(pathlib.Path(__file__).with_name('select-fileio-trace.ps1')),
                       '-InputXml', str(source), '-TraceProcessId', '42', '-OutputJsonLines', str(output)]
            result = subprocess.run(command, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            selected = [json.loads(line) for line in output.read_text().splitlines()]
            self.assertEqual([e['operation'] for e in selected], ['Create', 'OperationEnd'])
            self.assertEqual([e['event'] for e in selected], ['FileIo', 'FileIo'])
            self.assertTrue(selected[1]['correlated_completion'])
            before = output.read_bytes()
            self.assertNotEqual(subprocess.run(command, capture_output=True).returncode, 0)
            self.assertEqual(before, output.read_bytes())


if __name__ == '__main__':
    unittest.main()
