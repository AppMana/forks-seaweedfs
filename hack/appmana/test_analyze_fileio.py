import importlib.util
import io
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("fileio", Path(__file__).with_name("analyze-fileio.py"))
fileio = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fileio)


def event(op, irp, **data):
    fields = dict(IrpPtr=irp, **data)
    payload = "".join(f'<Data Name="{k}">{v}</Data>' for k, v in fields.items())
    return ('<Event><System><TimeCreated SystemTime="now"/><Execution ProcessID="42" ThreadID="7"/></System>'
            f'<EventData>{payload}</EventData><RenderingInfo><EventName>FileIo</EventName><Opcode>{op}</Opcode></RenderingInfo></Event>')


class CorrelationTests(unittest.TestCase):
    def test_interleaving_pointer_reuse_and_missing_history(self):
        xml = '<Events>' + ''.join([
            event('Create', 'A', FileObject='F', OpenPath='repo/.git'),
            event('Create', 'B', FileObject='G', OpenPath='other'),
            event('OperationEnd', 'B', NtStatus='3221225524'),
            event('OperationEnd', 'A', NtStatus='0'),
            event('QueryInformation', 'A', FileObject='F'),
            event('OperationEnd', 'A', NtStatus='3221225530'),
            event('OperationEnd', 'missing', NtStatus='0'),
        ]) + '</Events>'
        records = list(fileio.correlate(io.BytesIO(xml.encode())))
        self.assertEqual([(r['path'], r['status']) for r in records[:-1]],
                         [('other', '0xc0000034'), ('repo/.git', '0x00000000'), ('repo/.git', '0xc000003a')])
        self.assertEqual(records[-1]['unmatched_completions'], 1)
        self.assertEqual(records[-1]['uncompleted_requests'], 0)
        self.assertEqual(records[2]['process_id'], '42')


if __name__ == '__main__':
    unittest.main()
