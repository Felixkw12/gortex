import importlib.util
from pathlib import Path
import re
import shutil
import tempfile
import time
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "ci_test_partition", Path(__file__).with_name("ci-test-partition.py"))
planner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(planner)

WORKFLOW = Path(__file__).parents[1] / ".github/workflows/ci.yml"


class LayoutTests(unittest.TestCase):
    def test_workflow_runs_every_group_once_on_each_os(self):
        layout = planner.check_layout(planner.workflow_rows(WORKFLOW.read_text()))
        self.assertEqual(sorted(layout), ["macos-latest", "ubuntu-latest", "windows-latest"])

    def test_layout_rejects_a_missing_or_doubled_shard(self):
        complete = [("linux", "indexer:1/2"), ("linux", "indexer:2/2"), ("linux", "mcp+store"),
                    ("linux", "rest")]
        planner.check_layout(complete)
        broken = {
            "missing shard": complete[1:],
            "doubled shard": complete + [("linux", "indexer:2/2")],
            "mixed counts": [("linux", "indexer:1/2"), ("linux", "indexer:2/3")] + complete[2:],
            "missing group": complete[:2] + [("linux", "mcp+rest")],
            "group twice in a job": complete[:2] + [("linux", "mcp+store+mcp"), ("linux", "rest")],
        }
        for case, rows in broken.items():
            with self.subTest(case), self.assertRaises(ValueError):
                planner.check_layout(rows)
        for spec in ("indexer:0/2", "indexer:3/2", "rest:1/2", "nope", "indexer-1", ""):
            with self.subTest(spec=spec), self.assertRaises(ValueError):
                planner.parse_partition(spec)

    def test_race_setting_is_strict(self):
        for environ in ({"TEST_RACE": "False"}, {"TEST_WINDOWS": "true", "TEST_RACE": "true"}):
            with self.subTest(environ=environ), \
                    mock.patch.dict(planner.os.environ, environ, clear=True), \
                    mock.patch.object(planner.sys, "argv", ["partition", "--partition", "rest"]), \
                    mock.patch.object(planner, "capture") as capture, \
                    mock.patch("sys.stderr"), self.assertRaises(SystemExit):
                planner.main()
            capture.assert_not_called()


class PlanTests(unittest.TestCase):
    names = sorted([f"Test{family}{index}" for family in ("Chain", "Fold", "Parity") for index in range(40)]
                   + ["Example", "FuzzRoundTrip"])

    def test_shards_and_batches_run_each_test_exactly_once(self):
        for count in (1, 2, 3):
            shards = [planner.plan_batches(self.names, shard, count, 8) for shard in range(count)]
            seen = [name for batches in shards for batch in batches for name in batch]
            self.assertEqual(sorted(seen), self.names)
            sizes = [sum(map(len, batches)) for batches in shards]
            self.assertLessEqual(max(sizes) - min(sizes), 1)
            for batches in shards:
                for batch in batches:
                    pattern = planner.exact_pattern(batch)
                    self.assertEqual([name for name in self.names if re.fullmatch(pattern, name)], batch)

    def test_results_count_only_top_level_tests(self):
        output = "\n".join([
            "=== RUN   TestA", "--- PASS: TestA (1.50s)", "=== RUN   TestB", "=== RUN   TestB/sub",
            "    --- FAIL: TestB/sub (0.00s)", "--- FAIL: TestB (0.20s)", "panic: boom", "goroutine 1",
            "=== RUN   TestC", "--- SKIP: TestC (0.00s)"])
        self.assertEqual(planner.parse_results(output),
                         {"TestA": ("PASS", 1.5), "TestB": ("FAIL", 0.2), "TestC": ("SKIP", 0.0)})
        self.assertEqual(planner.test_output(output, "TestB").splitlines()[-1], "goroutine 1")

    def test_coverage_merge_sums_the_same_block(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "a.out").write_text("mode: atomic\nf.go:1.1,2.2 1 3\nf.go:3.1,4.2 1 0\n")
            (root / "b.out").write_text("mode: atomic\nf.go:1.1,2.2 1 2\n")
            self.assertTrue(planner.merge_coverage([root / "a.out", root / "b.out", root / "gone.out"],
                                                   root / "c.out"))
            self.assertEqual((root / "c.out").read_text(),
                             "mode: atomic\nf.go:1.1,2.2 1 5\nf.go:3.1,4.2 1 0\n")


class RerunTests(unittest.TestCase):
    """The batch outcomes that decide between a rerun alone and a hard failure."""

    def job(self):
        scratch = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, scratch, True)
        (scratch / "logs").mkdir()
        (scratch / "coverage").mkdir()
        with mock.patch.object(planner, "capture", return_value="/go\n"):
            return planner.Job(False, False, 2, scratch, time.monotonic() + 600)

    def batch(self, outputs, names=("TestA", "TestB")):
        job = self.job()
        runs = iter(outputs)
        with mock.patch.object(job, "run", side_effect=lambda *args, **kwargs: next(runs)):
            reruns = job.run_batch("indexer:1/1", Path("indexer.test"), ".", list(names), 0, "b")
            for name in reruns:
                job.rerun_binary("indexer:1/1", Path("indexer.test"), ".", name)
        return job

    def test_failure_beside_other_tests_that_passes_alone_is_flaky(self):
        job = self.batch([(1, "--- PASS: TestA (0.10s)\n--- FAIL: TestB (0.20s)\n"),
                          (0, "--- PASS: TestB (0.10s)\n")])
        self.assertEqual((job.failures, job.flaky), ([], ["indexer:1/1 TestB"]))

    def test_failure_that_repeats_alone_fails_the_job(self):
        job = self.batch([(1, "--- PASS: TestA (0.10s)\n--- FAIL: TestB (0.20s)\n"),
                          (1, "--- FAIL: TestB (0.10s)\n")])
        self.assertEqual([(label, name) for label, name, _, _ in job.failures], [("indexer:1/1", "TestB")])
        self.assertEqual(job.flaky, [])

    def test_race_panic_or_timeout_is_never_rerun(self):
        for report in (planner.RACE_REPORT, "panic: boom", planner.TIMEOUT_REPORT):
            with self.subTest(report=report):
                job = self.batch([(1, f"=== RUN   TestB\n{report}\n--- FAIL: TestB (0.20s)\n"
                                      "--- PASS: TestA (0.10s)\n")])
                self.assertEqual([(name, rerun) for _, name, _, rerun in job.failures], [("TestB", False)])

    def test_tests_an_exiting_test_kept_from_running_get_their_first_run(self):
        job = self.batch([(1, "--- FAIL: TestA (0.10s)\nexit status 1\n"),
                          (0, "--- PASS: TestA (0.10s)\n"), (0, "--- PASS: TestB (0.10s)\n")])
        self.assertEqual((job.failures, job.flaky), ([], ["indexer:1/1 TestA"]))

    def test_clean_exit_that_skipped_a_test_fails(self):
        job = self.batch([(0, "--- PASS: TestA (0.10s)\n")])
        self.assertEqual([name for _, name, _, _ in job.failures], [None])


if __name__ == "__main__":
    unittest.main()
