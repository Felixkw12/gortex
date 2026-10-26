from pathlib import Path
import sys
import re
import unittest

sys.path.insert(0, str(Path(__file__).parent))
import ci_test_gate as gate

WORKFLOW = Path(__file__).parents[1] / ".github/workflows/ci.yml"


class WorkflowPolicyTests(unittest.TestCase):
    def needs(self, mode="affected", empty=False, event="pull_request"):
        data = {name: {"result": "success"} for name in
                ("plan", "test", "smoke", "build-native", "lint", "build-linux-static", "build-onnx", "benchmark", "parser-stress")}
        data["plan"]["outputs"] = {"mode": mode, "has_tests": "false" if empty else "true",
                                  "empty_ok": "true" if empty else "false"}
        if mode == "full":
            data["smoke"]["result"] = "skipped"
        if empty:
            data["test"]["result"] = "skipped"
        if event != "pull_request":
            data["benchmark"]["result"] = "skipped"
        if event != "workflow_dispatch":
            data["parser-stress"]["result"] = "skipped"
        return data

    def test_stable_gate_accepts_full_affected_and_explicit_empty_only(self):
        for event, mode, empty in (("pull_request", "affected", False), ("pull_request", "affected", True),
                                    ("pull_request", "full", False), ("push", "full", False),
                                    ("workflow_dispatch", "full", False)):
            gate.check(self.needs(mode, empty, event), event)
        for job, details in self.needs().items():
            for result in ("failure", "cancelled", "skipped"):
                if result == details["result"]:
                    continue
                data = self.needs()
                data[job]["result"] = result
                with self.assertRaises(ValueError):
                    gate.check(data, "pull_request")
        bad = self.needs(empty=True)
        bad["plan"]["outputs"]["empty_ok"] = "false"
        with self.assertRaises(ValueError):
            gate.check(bad, "pull_request")

        for outputs in ({}, {"mode": "affected", "has_tests": "oops"},
                        {"mode": "full", "has_tests": "false", "empty_ok": "true"}):
            invalid = self.needs()
            invalid["plan"]["outputs"] = outputs
            with self.assertRaises(ValueError):
                gate.check(invalid, "pull_request")

    def test_workflow_uses_dynamic_selection_and_keeps_unconditional_builds_and_gates(self):
        text = WORKFLOW.read_text()
        self.assertIn("matrix: ${{ fromJSON(needs.plan.outputs.matrix) }}", text)
        self.assertIn("CI_FULL_TESTS: ${{ vars.CI_FULL_TESTS }}", text)
        self.assertIn("CI_BASE_SHA: ${{ github.event.pull_request.base.sha }}", text)
        self.assertIn("fetch-depth: 0", text)
        self.assertIn("--selection test-selection.json", text)
        self.assertIn("needs.plan.outputs.mode == 'full'", text)
        self.assertIn("name: affected-coverage-", text)
        for name in ("build-native", "lint", "build-linux-static", "build-onnx", "ci-result"):
            self.assertIn("  " + name + ":\n", text)
        events = re.search(r"(?m)^    types: \[([^\]]+)\]", text)
        self.assertEqual(set(events.group(1).split(", ")),
                         {"opened", "synchronize", "reopened", "labeled", "unlabeled"})
        self.assertIn("if: always()", text)


if __name__ == "__main__":
    unittest.main()
