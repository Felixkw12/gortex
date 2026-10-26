import json
import os
from pathlib import Path
import subprocess
import sys
import unittest

sys.path.insert(0, str(Path(__file__).parent))
from test_ci_test_selection import Repository

RUNNER = Path(__file__).with_name("ci-test-partition.py").resolve()


class RunnerSelectionTests(Repository):
    def test_real_runner_executes_every_selected_test_and_full_mode_keeps_other_tests(self):
        head = self.changed()
        affected = self.plan(head)
        self.assertEqual(affected["mode"], "affected", affected["reason"])
        full = self.plan(head, full=True)
        target = self.native()
        outputs = {}
        for data in (affected, full):
            manifest = self.root / "selection.json"
            manifest.write_text(json.dumps(data))
            output = ""
            for row in data["matrix"]["include"]:
                if row["os"] != target:
                    continue
                race = data["mode"] == "affected" and os.name != "nt"
                env = dict(os.environ, TEST_RACE=str(race).lower(), TEST_WORKERS="2", TEST_TARGET=target)
                run = subprocess.run([sys.executable, str(RUNNER), "--partition", row["partition"],
                                      "--selection", str(manifest)], cwd=self.root, env=env,
                                     stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=180)
                self.assertEqual(run.returncode, 0, run.stdout)
                output += run.stdout
            outputs[data["mode"]] = output
            if data["mode"] == "affected" and os.name != "nt":
                self.assertTrue((self.root / "coverage.out").read_text().startswith("mode: atomic\n"))
        for name in ("TestLeaf", "TestConsumer", "TestIndirect", "TestExternalImport", "TestHelperImport", "TestMCP"):
            self.assertIn(name, outputs["affected"])
            self.assertIn(name, outputs["full"])
        for name in ("TestIndexer", "TestOtherIndexer", "TestStore"):
            self.assertNotIn(name, outputs["affected"])
            self.assertIn(name, outputs["full"])


    def test_corrupt_manifest_is_rejected_before_execution(self):
        head = self.changed()
        data = self.plan(head)
        data["packages"][self.native()] = []
        manifest = self.root / "selection.json"
        manifest.write_text(json.dumps(data))
        run = subprocess.run([sys.executable, str(RUNNER), "--partition", "rest", "--selection", str(manifest)],
                             cwd=self.root, env=dict(os.environ, TEST_RACE="false", TEST_TARGET=self.native()),
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=60)
        self.assertNotEqual(run.returncode, 0)
        self.assertIn("integrity check failed", run.stdout)
        self.assertNotIn("partition rest:", run.stdout)

    @unittest.skipUnless(sys.platform == "darwin", "native macOS watcher-race contract")
    def test_selected_indexer_keeps_macos_rest_watcher_race_even_without_rest_packages(self):
        data = self.plan(self.changed("internal/indexer/value.go"))
        manifest = self.root / "selection.json"
        manifest.write_text(json.dumps(data))
        run = subprocess.run([sys.executable, str(RUNNER), "--partition", "rest", "--selection", str(manifest)],
                             cwd=self.root, env=dict(os.environ, TEST_RACE="false", TEST_WORKERS="2", TEST_TARGET=self.native()),
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=180)
        self.assertEqual(run.returncode, 0, run.stdout)
        self.assertIn("indexer-race:1/1 TestWatchFixture", run.stdout)
        self.assertNotIn("rest: done", run.stdout)


if __name__ == "__main__":
    unittest.main()
