#!/usr/bin/env python3
"""Stable CI result: failed planning/execution can never look like an empty PR."""

import json
import os


def check(needs, event):
    def require(name, expected="success"):
        if needs.get(name, {}).get("result") != expected:
            raise ValueError(f"{name} must be {expected}")

    require("plan")
    outputs = needs["plan"].get("outputs", {})
    mode, has_tests = outputs.get("mode"), outputs.get("has_tests")
    if mode not in ("affected", "full") or has_tests not in ("true", "false"):
        raise ValueError("planning outputs are missing or invalid")
    if has_tests == "false" and (mode != "affected" or outputs.get("empty_ok") != "true"):
        raise ValueError("empty test selection was not explicitly validated")
    require("test", "success" if has_tests == "true" else "skipped")
    require("smoke", "success" if mode == "affected" else "skipped")
    for name in ("build-native", "lint", "build-linux-static", "build-onnx"):
        require(name)
    require("benchmark", "success" if event == "pull_request" else "skipped")
    require("parser-stress", "success" if event == "workflow_dispatch" else "skipped")


if __name__ == "__main__":
    check(json.loads(os.environ["CI_NEEDS"]), os.environ["GITHUB_EVENT_NAME"])
    print("All required CI checks passed for the validated test selection.")
