#!/usr/bin/env python3
"""Dependency-free regressions for the ONNX fixture's session policy.

These mocks test fallback policy, not CUDA compatibility or hardware inference.
Run: python3 -m unittest discover -s go/scripts -p onnx_gpu_validation_test.py
"""
import ast
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock


FIXTURE = Path(__file__).resolve().parents[2] / ".github/ci-tests/python-onnx-gpu/main.py"


def session_policy():
    tree = ast.parse(FIXTURE.read_text())
    # Execute the real session-creation block without importing native wheels
    # or constructing the model. Keep subsequent result assertions out of scope.
    start = next(i for i, node in enumerate(tree.body)
                 if isinstance(node, ast.Assign)
                 and any(isinstance(t, ast.Name) and t.id == "so" for t in node.targets))
    end = next(i for i, node in enumerate(tree.body[start:], start)
               if isinstance(node, ast.Assign)
               and any(isinstance(t, ast.Name) and t.id == "active" for t in node.targets))
    return compile(ast.Module(body=tree.body[start:end], type_ignores=[]), str(FIXTURE), "exec")


class SessionPolicyTests(unittest.TestCase):
    def run_policy(self, factory=None):
        options = SimpleNamespace(log_severity_level=None, add_session_config_entry=Mock())
        session = Mock()
        ort = SimpleNamespace(SessionOptions=Mock(return_value=options),
                              InferenceSession=Mock(side_effect=factory, return_value=session))
        model = Mock()
        model.SerializeToString.return_value = b"model"
        exec(session_policy(), {"ort": ort, "model": model})
        return options, session, ort

    def test_cpu_node_assignment_is_forbidden(self):
        options, _, _ = self.run_policy()
        options.add_session_config_entry.assert_called_once_with("session.disable_cpu_ep_fallback", "1")

    def test_only_cuda_is_requested(self):
        options, _, ort = self.run_policy()
        ort.InferenceSession.assert_called_once_with(
            b"model", sess_options=options, providers=["CUDAExecutionProvider"],
            enable_fallback=False)

    def test_wrapper_retry_is_disabled_at_construction(self):
        _, session, ort = self.run_policy()
        self.assertIs(ort.InferenceSession.call_args.kwargs["enable_fallback"], False)
        session.run.assert_not_called()

    def test_unsupported_graph_does_not_continue(self):
        def refuse(*args, **kwargs):
            raise RuntimeError("graph cannot be assigned to CUDA")
        with self.assertRaisesRegex(RuntimeError, "cannot be assigned to CUDA"):
            self.run_policy(refuse)


if __name__ == "__main__":
    unittest.main()
