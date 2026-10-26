#!/usr/bin/env python3
"""Plan complete affected Go packages; uncertainty requests the full CI suite.

Only ordinary added/modified Go inputs are optimized in this initial trial.
Metadata describes each native CI target, including test imports, without
compiling grammar dependencies with `go list -test`.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess
import tempfile
import sys

POLICY_PATH = Path(__file__).with_name("ci-test-policy.json")
SCHEMA = 1
SHA = re.compile(r"[0-9a-f]{40,64}")


class FullSuite(Exception):
    """An anticipated uncertainty, safe only with the full suite."""


def policy():
    result = json.loads(POLICY_PATH.read_text())
    if (len(result["targets"]) != 3 or
            len({t["runner"] for t in result["targets"]}) != 3 or
            result["partitions"] != ["indexer:1/2", "indexer:2/2", "mcp+store", "rest"] or
            set(result["groups"]) != {"indexer", "mcp", "store"}):
        raise ValueError("invalid CI policy")
    return result


def full_matrix(config):
    return {"include": [{"os": t["runner"], "partition": p}
                        for t in config["targets"] for p in config["partitions"]]}


def command(root, args, env=None):
    try:
        return subprocess.check_output(args, cwd=root, env=env, timeout=180,
                                       stderr=subprocess.PIPE).decode("utf-8")
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired, UnicodeError) as error:
        raise FullSuite(f"metadata command failed: {args[0]} {args[1]} ({type(error).__name__})") from error


def changed_paths(root, base, head):
    if not SHA.fullmatch(base or "") or not SHA.fullmatch(head or ""):
        raise FullSuite("missing or invalid comparison revisions")
    for revision in (base, head):
        if command(root, ["git", "rev-parse", "--verify", revision + "^{commit}"]).strip() != revision:
            raise FullSuite("comparison revision is not an exact commit")
    if command(root, ["git", "rev-parse", "HEAD"]).strip() != head:
        raise FullSuite("checkout does not match the event revision")
    ancestor = command(root, ["git", "merge-base", base, head]).strip()
    fields = command(root, ["git", "diff", "--name-status", "-z", "--find-renames",
                            ancestor, head, "--"]).split("\0")
    changes = []
    while fields and fields[0]:
        status = fields.pop(0)
        count = 2 if status.startswith(("R", "C")) else 1
        if len(fields) < count or any(not p for p in fields[:count]):
            raise FullSuite("incomplete NUL-delimited git diff")
        changes.append((status, fields[:count]))
        del fields[:count]
    return changes


def json_stream(text):
    decoder, objects = json.JSONDecoder(), []
    while text.strip():
        text = text.lstrip()
        obj, end = decoder.raw_decode(text)
        objects.append(obj)
        text = text[end:]
    return objects


def graph(root, config):
    # A same-directory alternate modfile preserves relative replacements.
    # Any checksum discovery goes to its disposable .sum, never go.sum.
    with tempfile.NamedTemporaryFile(prefix=".ci-selection-", suffix=".mod", dir=root, delete=False) as out:
        modfile = Path(out.name)
    sumfile = modfile.with_suffix(".sum")
    try:
        modfile.write_bytes((root / "go.mod").read_bytes())
        if (root / "go.sum").exists():
            sumfile.write_bytes((root / "go.sum").read_bytes())
        return package_graph(root, config, ["-mod=readonly", "-modfile=" + str(modfile)])
    finally:
        modfile.unlink(missing_ok=True)
        sumfile.unlink(missing_ok=True)


def package_graph(root, config, flags):
    if os.environ.get("GOEXPERIMENT") or os.environ.get("GOFLAGS") or os.environ.get("GOWORK") not in (None, "", "off"):
        raise FullSuite("unsupported GOFLAGS, GOEXPERIMENT or workspace context")
    module = command(root, ["go", "list"] + flags + ["-m"]).strip()
    if not module or "\n" in module:
        raise FullSuite("ambiguous module identity")
    packages, reverse, active_files = {}, {}, {}
    for target in config["targets"]:
        census = None
        for race in (False, True):
            env = dict(os.environ, GOOS=target["goos"], GOARCH=target["goarch"], CGO_ENABLED="1", GOWORK="off")
            listing = ["go", "list"] + flags + (["-race"] if race else [])
            try:
                rows = json_stream(command(root, listing + ["-json", "./..."], env))
            except (ValueError, TypeError) as error:
                raise FullSuite("invalid Go package metadata") from error
            # Test imports are reported without -test but their dependencies are
            # not traversed by ./... alone. Include those dependency roots, so a
            # local replacement/helper module can connect two root packages.
            test_imports = sorted({name for row in rows for field in ("TestImports", "XTestImports")
                                   for name in row.get(field, [])})
            try:
                dependencies = json_stream(command(root, listing + ["-deps", "-json", "./..."] + test_imports, env))
            except (ValueError, TypeError) as error:
                raise FullSuite("invalid Go dependency metadata") from error
            current = set()
            for row in dependencies:
                if row.get("Error") or row.get("DepsErrors") or row.get("Incomplete"):
                    raise FullSuite("incomplete Go dependency graph")
                name = row.get("ImportPath")
                if not name or not row.get("Dir"):
                    raise FullSuite("invalid Go package census")
                for dependency in row.get("Imports", []):
                    reverse.setdefault(dependency, set()).add(name)
            for row in rows:
                if row.get("Error") or row.get("DepsErrors") or row.get("Incomplete"):
                    raise FullSuite("incomplete Go package graph")
                name = row.get("ImportPath")
                if not name or name in current or not row.get("Dir") or not row.get("Module", {}).get("Main"):
                    raise FullSuite("invalid root Go package census")
                current.add(name)
                try:
                    directory = Path(row["Dir"]).resolve().relative_to(root.resolve()).as_posix()
                except ValueError as error:
                    raise FullSuite("package outside checkout") from error
                for field in ("EmbedFiles", "TestEmbedFiles", "XTestEmbedFiles"):
                    for filename in row.get(field, []):
                        path = (PurePosixPath(directory) / filename).as_posix()
                        active_files.setdefault(path, set()).add("<embed>")
                for field in ("GoFiles", "CgoFiles", "TestGoFiles", "XTestGoFiles"):
                    for filename in row.get(field, []):
                        path = (PurePosixPath(directory) / filename).as_posix()
                        active_files.setdefault(path, set()).add(name)
                        if field == "CgoFiles":
                            active_files[path].add("<cgo>")
                for field in ("TestImports", "XTestImports"):
                    for dependency in row.get(field, []):
                        reverse.setdefault(dependency, set()).add(name)
            if not current:
                raise FullSuite("empty Go package census")
            if census is not None and current != census:
                raise FullSuite("race-specific package census requires full testing")
            census = current
        packages[target["runner"]] = sorted(census)
    return module, packages, reverse, active_files


def select(root, changes, metadata):
    _, _, reverse, active_files = metadata
    selected = set()
    for status, paths in changes:
        # Removed dependencies need a base graph. Conservatively run full
        # instead of guessing from an incomplete head-only graph.
        if status not in ("A", "M"):
            raise FullSuite(f"delete/rename or unsupported change status: {status}")
        path = paths[0]
        if not path.endswith(".go"):
            raise FullSuite(f"non-Go input: {path}")
        owners = active_files.get(path)
        if not owners or owners & {"<cgo>", "<embed>"}:
            raise FullSuite(f"unmapped, unsupported build-tag, embedded, or cgo input: {path}")
        text = (root / path).read_text(encoding="utf-8")
        if re.search(r"(?m)^//(?:go:generate|go:embed| Code generated.*DO NOT EDIT)", text):
            raise FullSuite(f"generated or embedded input: {path}")
        selected.update(owners)
    pending = list(selected)
    while pending:
        for dependent in reverse.get(pending.pop(), set()) - selected:
            selected.add(dependent)
            pending.append(dependent)
    return selected


def selected_matrix(config, module, by_target):
    large = {g: module + suffix for g, suffix in config["groups"].items()}
    rows = []
    for target in config["targets"]:
        selected = set(by_target[target["runner"]])
        groups = {g for g, package in large.items() if package in selected}
        if selected - set(large.values()):
            groups.add("rest")
        # The rest job owns the existing macOS indexer watcher race pass.
        if target["goos"] == "darwin" and "indexer" in groups:
            groups.add("rest")
        for partition in config["partitions"]:
            if groups & {part.split(":")[0] for part in partition.split("+")}:
                rows.append({"os": target["runner"], "partition": partition})
    return {"include": rows}


def plan(root, event, base, head, full=False, labels=(), author=""):
    config = policy()
    result = {"schema": SCHEMA, "head": head, "base": base, "mode": "full", "reason": "",
              "empty_ok": False, "toolchain": "", "module": "", "packages": {}, "census": {}, "targets": config["targets"],
              "matrix": full_matrix(config)}
    if event != "pull_request":
        result["reason"] = f"full suite for {event}"
    elif full or "ci:full" in labels:
        result["reason"] = "explicit full-suite override"
    elif author == "dependabot[bot]":
        result["reason"] = "full suite for Dependabot dependency updates"
    else:
        try:
            changes = changed_paths(root, base, head)
            # Classify before discovery; CI/dependency/resource changes must
            # not pay for metadata only to fall back to full anyway.
            for status, paths in changes:
                if (status not in ("A", "M") or not paths[0].endswith(".go") or
                        paths[0].startswith(("scripts/", ".github/"))):
                    raise FullSuite(f"full fallback for {status}: {' -> '.join(paths)}")
            metadata = graph(root, config)
            selected = select(root, changes, metadata)
            module, census, _, _ = metadata
            by_target = {target: sorted(selected & set(names)) for target, names in census.items()}
            result.update(mode="affected", empty_ok=not changes, toolchain=command(root, ["go", "env", "GOVERSION"]).strip(), module=module, packages=by_target, census=census,
                          matrix=selected_matrix(config, module, by_target),
                          reason="changed packages and transitive reverse test/import dependents" if changes
                          else "no changed files; explicit empty package selection")
        except (FullSuite, OSError, UnicodeError) as error:
            result["reason"] = str(error)
    result["sha256"] = seal(result)
    return result


def seal(data):
    payload = {key: value for key, value in data.items() if key != "sha256"}
    return hashlib.sha256(json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def validate_selection(data, root, target, packages):
    """Fail closed before filtering; do not accept stale/partial manifests."""
    config = policy()
    required = {"schema", "head", "base", "mode", "reason", "empty_ok", "toolchain", "module",
                "packages", "census", "targets", "matrix", "sha256"}
    if set(data) != required or not isinstance(data.get("empty_ok"), bool):
        raise ValueError("incomplete test selection manifest")
    if data.get("sha256") != seal(data):
        raise ValueError("test selection integrity check failed")
    if data.get("schema") != SCHEMA or data.get("mode") not in ("full", "affected") or not data.get("reason"):
        raise ValueError("invalid test selection manifest")
    if data.get("targets") != config["targets"] or data.get("matrix") is None:
        raise ValueError("selection policy does not match checkout")
    if data.get("head") != command(root, ["git", "rev-parse", "HEAD"]).strip():
        raise ValueError("stale test selection revision")
    if data["mode"] == "full":
        if data.get("packages") != {} or data.get("census") != {} or data.get("module") != "" or data.get("toolchain") != "":
            raise ValueError("full-suite manifest contains selective inputs")
        if data["matrix"] != full_matrix(config):
            raise ValueError("incomplete full-suite matrix")
        return None
    context = next((t for t in config["targets"] if t["runner"] == target), None)
    if context is None:
        raise ValueError("unknown selection target")
    if data.get("toolchain") != command(root, ["go", "env", "GOVERSION"]).strip():
        raise ValueError("Go toolchain does not match selection")
    if data.get("module") != command(root, ["go", "list", "-m"]).strip():
        raise ValueError("module identity does not match selection")
    actual = command(root, ["go", "env", "GOOS", "GOARCH", "CGO_ENABLED"]).splitlines()
    if actual != [context["goos"], context["goarch"], "1"] or os.environ.get("GOFLAGS") or os.environ.get("GOEXPERIMENT"):
        raise ValueError("native build context changed; use CI_FULL_TESTS=true to run full")
    census, selected = data.get("census", {}), data.get("packages", {})
    if set(census) != {t["runner"] for t in config["targets"]} or set(selected) != set(census):
        raise ValueError("incomplete target census")
    for key in census:
        if (not census[key] or census[key] != sorted(set(census[key])) or
                selected[key] != sorted(set(selected[key])) or not set(selected[key]) <= set(census[key])):
            raise ValueError("invalid selected package census")
    if sorted(packages) != census[target]:
        raise ValueError("native package census changed; refusing incomplete selection")
    if data["matrix"] != selected_matrix(config, data.get("module", ""), selected):
        raise ValueError("selected matrix does not cover selected packages")
    return set(selected[target])


def summary(data):
    rows = ["## Test selection", "", f"Mode: **{data['mode']}**", "", data["reason"], "",
            f"Test jobs: **{len(data['matrix']['include'])} / 12**", ""]
    for target, names in data["packages"].items():
        rows += [f"### {target}: {len(names)} / {len(data['census'][target])} packages", ""]
        rows += [f"- `{name}`" for name in names] or ["- No selected packages."]
        rows.append("")
    rows += ["Full lint, builds and security checks remain enabled.",
             "Affected-run coverage is partial and is not uploaded as whole-repository coverage."]
    return "\n".join(rows) + "\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    event = os.environ.get("GITHUB_EVENT_NAME", "")
    payload = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text())
    labels = [label["name"] for label in payload.get("pull_request", {}).get("labels", [])]
    data = plan(Path.cwd(), event, os.environ.get("CI_BASE_SHA", ""),
                os.environ.get("GITHUB_SHA", ""), os.environ.get("CI_FULL_TESTS", "").lower() == "true", labels,
                payload.get("pull_request", {}).get("user", {}).get("login", ""))
    args.output.write_text(json.dumps(data, indent=2) + "\n")
    report = summary(data)
    print(report)
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as out:
            out.write(report)
    if os.environ.get("GITHUB_OUTPUT"):
        with open(os.environ["GITHUB_OUTPUT"], "a") as out:
            out.write(f"mode={data['mode']}\nhas_tests={str(bool(data['matrix']['include'])).lower()}\n")
            out.write(f"empty_ok={str(data['empty_ok']).lower()}\n")
            out.write("matrix=" + json.dumps(data["matrix"], separators=(",", ":")) + "\n")


if __name__ == "__main__":
    main()
