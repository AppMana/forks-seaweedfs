#!/usr/bin/env python3
"""Stream tracerpt XML (or its ZIP) into correlated FileIo JSON lines.

Nonzero NTSTATUS values include normal probes and retry/control statuses; this
tool identifies evidence, not bugs. A circular trace can omit earlier requests
even when EventsLost is zero. Preserve the raw ETL for WPA and stack analysis.
"""
import argparse
import contextlib
import json
import xml.etree.ElementTree as ET
import zipfile


def correlate(stream):
    pending, paths = {}, {}
    matched = unmatched = 0
    for _, event in ET.iterparse(stream, events=("end",)):
        if event.tag.rsplit("}", 1)[-1] != "Event":
            continue
        data = {e.get("Name"): (e.text or "").strip()
                for e in event.findall("{*}EventData/{*}Data")}
        if "EventsLost" in data:
            yield {"kind": "trace_header", "data": data}
        rendering = event.find("{*}RenderingInfo")
        name = rendering.findtext("{*}EventName") if rendering is not None else None
        operation = rendering.findtext("{*}Opcode") if rendering is not None else None
        system = event.find("{*}System")
        if name == "FileIo" and system is not None:
            irp = data.get("IrpPtr")
            clock = system.find("{*}TimeCreated")
            timestamp = clock.get("SystemTime") if clock is not None else None
            if operation == "OperationEnd" and irp:
                request = pending.pop(irp, None)
                if request is None:
                    unmatched += 1
                else:
                    matched += 1
                    status = int(data["NtStatus"], 0)
                    request.update(kind="completion", completion_time=timestamp,
                                   status=f"0x{status:08x}")
                    if request["operation"] == "Create" and status == 0:
                        paths[request["file_object"]] = request["path"]
                    yield request
                    if request["operation"] == "Close":
                        paths.pop(request["file_object"], None)
            elif irp:
                execution = system.find("{*}Execution")
                obj = data.get("FileObject", "")
                if operation == "Create":
                    # Kernel object addresses are reused. Never inherit the
                    # previous open's path when this new create fails.
                    paths.pop(obj, None)
                pending[irp] = {
                    "irp": irp, "operation": operation, "request_time": timestamp,
                    "process_id": execution.get("ProcessID") if execution is not None else None,
                    "thread_id": execution.get("ThreadID") if execution is not None else None,
                    "file_object": obj, "path": data.get("OpenPath", paths.get(obj, "")),
                    "request_data": data,
                }
        event.clear()
    yield {"kind": "summary", "matched_completions": matched,
           "unmatched_completions": unmatched, "uncompleted_requests": len(pending),
           "note": "Nonzero status is not proof of a bug; circular capture may omit history."}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("trace", help="tracerpt XML or ZIP containing exactly one XML")
    parser.add_argument("--path", default="git-lfs-temp-metadata", help="case-insensitive path substring")
    parser.add_argument("--all", action="store_true", help="include successful completions")
    args = parser.parse_args()
    with contextlib.ExitStack() as stack:
        if zipfile.is_zipfile(args.trace):
            archive = stack.enter_context(zipfile.ZipFile(args.trace))
            members = [n for n in archive.namelist() if n.lower().endswith(".xml")]
            if len(members) != 1:
                parser.error("expected exactly one XML in archive")
            stream = stack.enter_context(archive.open(members[0]))
        else:
            stream = stack.enter_context(open(args.trace, "rb"))
        for record in correlate(stream):
            if record["kind"] == "completion":
                if args.path.lower() not in record["path"].lower():
                    continue
                if not args.all and record["status"] == "0x00000000":
                    continue
            print(json.dumps(record))


if __name__ == "__main__":
    try:
        main()
    except BrokenPipeError:
        pass
