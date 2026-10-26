"""Real Git/Go fixtures exercise selection and the existing runner together."""

import copy
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).parent))
import ci_test_selection as selection



class Repository(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="ci-selection-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.write("go.mod", "module example.org/fixture\n\ngo 1.22\n\nrequire example.org/helper v0.0.0\n"
                   "replace example.org/helper => ./internal/thirdparty/helper\n")
        self.package("leaf", "func Value() int { return 42 }", "TestLeaf")
        self.package("consumer", 'import "example.org/fixture/leaf"\nfunc Value() int { return leaf.Value() }', "TestConsumer")
        self.package("indirect", 'import "example.org/fixture/consumer"\nfunc Value() int { return consumer.Value() }', "TestIndirect")
        self.package("testconsumer", "func Value() int { return 42 }", "TestTestConsumer")
        self.write("testconsumer/external_test.go", 'package testconsumer_test\nimport ("testing"; "example.org/fixture/leaf")\n'
                   'func TestExternalImport(t *testing.T) { if leaf.Value()!=42 { t.Fatal("bad value") } }\n')
        self.package("raceconsumer", "func Value() int { return 42 }", "TestRaceConsumer")
        self.write("raceconsumer/race_test.go", '//go:build race\n\npackage raceconsumer\nimport ("testing"; "example.org/fixture/leaf")\n'
                   'func TestRaceImport(t *testing.T) {if leaf.Value()!=42 {t.Fatal("bad race value")} }\n')
        self.package("platform", "func Value() int { return 42 }", "TestPlatform")
        self.write("platform/value_windows.go", 'package platform\nimport "example.org/fixture/leaf"\nvar WindowsValue = leaf.Value()\n')
        self.write("internal/thirdparty/helper/go.mod", "module example.org/helper\n\ngo 1.22\n")
        self.write("internal/thirdparty/helper/helper.go", 'package helper\nimport "example.org/fixture/leaf"\nfunc Value() int {return leaf.Value()}\n')
        self.package("helperconsumer", "func Value() int { return 42 }", "TestHelperConsumer")
        self.write("helperconsumer/helper_test.go", 'package helperconsumer\nimport ("testing"; "example.org/helper")\n'
                   'func TestHelperImport(t *testing.T) {if helper.Value()!=42 {t.Fatal("bad helper")} }\n')
        self.package("internal/mcp", 'import "example.org/fixture/leaf"\nfunc Value() int { return leaf.Value() }', "TestMCP")
        self.package("internal/indexer", "func Value() int { return 42 }", "TestIndexer")
        self.write("internal/indexer/other_test.go", 'package indexer\nimport "testing"\nfunc TestOtherIndexer(t *testing.T) {}\nfunc TestWatchFixture(t *testing.T) {}\n')
        self.package("internal/graph/store_sqlite", "func Value() int { return 42 }", "TestStore")
        self.git("init", "-q", "-b", "main")
        self.base = self.commit()

    def write(self, name, text):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)

    def package(self, path, source, name):
        package = path.rsplit("/", 1)[-1]
        self.write(path + "/value.go", f"package {package}\n{source}\n")
        self.write(path + "/value_test.go", f'package {package}\nimport "testing"\n'
                   f'func {name}(t *testing.T) {{if Value()!=42 {{t.Fatal("bad value")}}}}\n')

    def git(self, *args):
        return subprocess.check_output(["git", "-c", "user.name=CI Fixture", "-c", "user.email=ci@example.org", *args],
                                       cwd=self.root, text=True, stderr=subprocess.PIPE).strip()

    def commit(self):
        self.git("add", "--all")
        self.git("commit", "-qm", "fixture change")
        return self.git("rev-parse", "HEAD")

    def changed(self, path="leaf/value.go", extra="\n// changed\n"):
        self.write(path, (self.root / path).read_text() + extra)
        return self.commit()

    def plan(self, head, **kwargs):
        return selection.plan(self.root, "pull_request", self.base, head, **kwargs)

    def native(self):
        actual = subprocess.check_output(["go", "env", "GOOS", "GOARCH"], text=True).splitlines()
        return next(t["runner"] for t in selection.policy()["targets"] if actual == [t["goos"], t["goarch"]])


class SelectionTests(Repository):
    def test_reverse_closure_includes_test_imports_platform_union_and_helper_module(self):
        head = self.changed()
        original = (self.root / "go.mod").read_bytes()
        data = self.plan(head)
        self.assertEqual(data["mode"], "affected", data["reason"])
        expected = {"leaf", "consumer", "indirect", "testconsumer", "platform", "raceconsumer", "helperconsumer", "internal/mcp"}
        for packages in data["packages"].values():
            self.assertEqual(set(packages), {"example.org/fixture/" + p for p in expected})
        self.assertEqual(len(data["matrix"]["include"]), 6)
        self.assertNotIn("indexer:1/2", [r["partition"] for r in data["matrix"]["include"]])
        self.assertEqual((self.root / "go.mod").read_bytes(), original)
        self.assertFalse((self.root / "go.sum").exists())
        self.assertEqual(list(self.root.glob(".ci-selection-*")), [])

    def test_test_only_change_still_selects_complete_package_and_reverse_dependents(self):
        data = self.plan(self.changed("consumer/value_test.go"))
        self.assertEqual(data["mode"], "affected", data["reason"])
        self.assertEqual(data["packages"][self.native()],
                         ["example.org/fixture/consumer", "example.org/fixture/indirect"])

    def test_added_package_and_external_test_import_are_selected(self):
        self.package("newpkg", 'import "example.org/fixture/leaf"\nfunc Value() int {return leaf.Value()}', "TestNew")
        data = self.plan(self.commit())
        self.assertEqual(data["mode"], "affected", data["reason"])
        self.assertEqual(data["packages"][self.native()], ["example.org/fixture/newpkg"])

    def test_resource_dependency_workflow_and_unknown_changes_fall_back(self):
        for path in ("leaf/testdata/input.txt", "go.sum", "go.mod", "go.work", "go.work.sum",
                     "internal/thirdparty/helper/go.mod", "internal/thirdparty/helper/go.sum",
                     ".github/workflows/ci.yml", "README.md", "unknown.data"):
            with self.subTest(path=path):
                self.git("reset", "--hard", self.base)
                self.write(path, (self.root / path).read_text() + "\n" if (self.root / path).exists() else "new input\n")
                data = self.plan(self.commit())
                self.assertEqual(data["mode"], "full")
                self.assertIn(path, data["reason"])
                self.assertEqual(len(data["matrix"]["include"]), 12)

    def test_deleted_and_renamed_paths_use_full_base_graph_fallback(self):
        self.git("mv", "leaf/value.go", "leaf/a name\nwith newline.go")
        head = self.commit()
        changes = selection.changed_paths(self.root, self.base, head)
        self.assertEqual(changes[0][1], ["leaf/value.go", "leaf/a name\nwith newline.go"])
        self.assertEqual(self.plan(head)["mode"], "full")
        self.git("reset", "--hard", self.base)
        (self.root / "leaf/value_test.go").unlink()
        self.assertEqual(self.plan(self.commit())["mode"], "full")

    def test_nested_module_cgo_generated_and_unsupported_build_inputs_fall_back(self):
        cases = {
            "internal/thirdparty/helper/helper.go": "\n// change\n",
            "leaf/ignored.go": "//go:build ci_unsupported\n\npackage leaf\n",
            "leaf/cgo.go": 'package leaf\nimport "C"\n',
            "leaf/generated.go": "// Code generated by fixture. DO NOT EDIT.\npackage leaf\n",
            "leaf/generate.go": "package leaf\n//go:generate echo fixture\n",
        }
        for path, text in cases.items():
            with self.subTest(path=path):
                self.git("reset", "--hard", self.base)
                if path.endswith("helper.go"):
                    text = (self.root / path).read_text() + text
                self.write(path, text)
                data = self.plan(self.commit())
                self.assertEqual(data["mode"], "full", data["reason"])
                self.assertTrue(data["matrix"]["include"])

    def test_failed_missing_or_unsupported_metadata_never_selects_empty(self):
        head = self.changed()
        for base in ("", "0" * 40):
            data = selection.plan(self.root, "pull_request", base, head)
            self.assertEqual(data["mode"], "full")
            self.assertFalse(data["empty_ok"])
        self.write("consumer/value.go", 'package consumer\nimport "example.org/missing"\n')
        data = self.plan(self.commit())
        self.assertEqual(data["mode"], "full")
        with mock.patch.dict(os.environ, {"GOFLAGS": "-tags=unknown"}):
            self.assertEqual(self.plan(head)["mode"], "full")
        with mock.patch.object(selection, "command", side_effect=RuntimeError("unexpected implementation failure")):
            with self.assertRaises(RuntimeError):
                self.plan(head)

    def test_push_manual_and_both_overrides_are_full(self):
        head = self.changed()
        for event in ("push", "workflow_dispatch"):
            self.assertEqual(selection.plan(self.root, event, self.base, head)["mode"], "full")
        for kwargs in ({"full": True}, {"labels": ["ci:full"]}):
            self.assertEqual(self.plan(head, **kwargs)["mode"], "full")
        bot = self.plan(head, author="dependabot[bot]")
        self.assertEqual(bot["mode"], "full")
        self.assertIn("Dependabot", bot["reason"])
        overridden = self.plan(head, full=True, author="dependabot[bot]")
        self.assertEqual(overridden["reason"], "explicit full-suite override")
        empty = selection.plan(self.root, "pull_request", head, head)
        self.assertEqual(empty["mode"], "affected", empty["reason"])
        self.assertTrue(empty["empty_ok"])
        self.assertEqual(empty["matrix"], {"include": []})

    def test_native_validation_rejects_stale_tampered_incomplete_and_context_changed(self):
        head = self.changed()
        data = self.plan(head)
        self.assertEqual(data["mode"], "affected", data["reason"])
        target = self.native()
        census = data["census"][target]
        self.assertEqual(selection.validate_selection(data, self.root, target, census), set(data["packages"][target]))
        for edit in (lambda d: d.update(head="0" * 40), lambda d: d["census"].pop(target),
                     lambda d: d.update(matrix={"include": []})):
            changed = copy.deepcopy(data)
            edit(changed)
            changed["sha256"] = selection.seal(changed)
            with self.assertRaises(ValueError):
                selection.validate_selection(changed, self.root, target, census)
        tampered = copy.deepcopy(data)
        tampered["packages"][target] = []
        with self.assertRaisesRegex(ValueError, "integrity"):
            selection.validate_selection(tampered, self.root, target, census)
        with self.assertRaisesRegex(ValueError, "census"):
            selection.validate_selection(data, self.root, target, census[:-1])
        with mock.patch.dict(os.environ, {"GOARCH": "unsupported"}):
            with self.assertRaisesRegex(ValueError, "context"):
                selection.validate_selection(data, self.root, target, census)

    def test_macos_indexer_selection_keeps_rest_watcher_race_owner(self):
        data = self.plan(self.changed("internal/indexer/value.go"))
        self.assertEqual(data["mode"], "affected", data["reason"])
        mac = [r["partition"] for r in data["matrix"]["include"] if r["os"] == "macos-latest"]
        self.assertEqual(mac, ["indexer:1/2", "indexer:2/2", "rest"])



if __name__ == "__main__":
    unittest.main()
