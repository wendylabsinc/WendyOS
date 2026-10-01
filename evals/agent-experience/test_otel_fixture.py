import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest

import otel_fixture as otel


class TelemetryEvidenceTests(unittest.TestCase):
    def test_final_claim_or_tool_input_is_not_query_evidence(self):
        text = "eval.processing_ms trial123"
        events = [{"type": "done", "text": text},
                  {"type": "tool_start", "id": "1", "tool": "telemetry_metrics", "arguments": {"invented": text}}]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "events.jsonl"
            path.write_text("\n".join(json.dumps(e) for e in events))
            with self.assertRaises(RuntimeError):
                otel.require_telemetry_query(path, "metrics", "trial123")
            events.append({"type": "tool_result", "id": "1", "text": text})
            path.write_text("\n".join(json.dumps(e) for e in events))
            otel.require_telemetry_query(path, "metrics", "trial123")

    def test_missing_or_broken_spans_cannot_pass(self):
        args = SimpleNamespace(run_id="trial123", signal="traces")
        responses = [[200, {"result": v*v, "nonce": args.run_id}] for v in (7,-13,23)]
        records = []
        for i in range(3):
            records.extend([
                {"type": "log", "severityNumber": 9},
                {"type": "metric", "name": "eval.processing_ms", "unit": "ms", "value": .1},
                {"type": "span", "name": "eval.request", "traceId": str(i), "spanId": "root"+str(i)},
                {"type": "span", "name": "eval.worker", "traceId": str(i), "spanId": "child"+str(i),
                 "parentSpanId": "root"+str(i), "durationMs": .1, "status": {"code": "STATUS_CODE_OK"}},
            ])
        otel.validate(responses, records, args, True)
        for change in (lambda r: r[-1].update(traceId="wrong"),
                       lambda r: r[-1].update(durationMs=300),
                       lambda r: r[-1].update(status={"code": "STATUS_CODE_ERROR"}),
                       lambda r: r.pop()):
            changed = json.loads(json.dumps(records))
            change(changed)
            with self.assertRaises(RuntimeError):
                otel.validate(responses, changed, args, True)


if __name__ == "__main__":
    unittest.main()
