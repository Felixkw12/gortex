#!/usr/bin/env python3
"""Run one CI test job: whole package groups, or one shard of a large package.

TEST_PARTITION names the job's work as "+"-joined groups, each optionally one
shard of n: "indexer:1/2" or "mcp+store+rest". A large package is compiled
once, and its top-level tests run in batches on one worker process per CPU.
The "rest" group runs every other package under one `go test -p <cpus>`.

Every compiled top-level test, example and fuzz seed must report a result, or
the job fails. A test that fails while other workers share the machine is
rerun once on an idle machine. A pass on that rerun is reported as flaky, not
hidden. A data race, a panic (a test timeout is one) or a build failure is
never rerun.
"""

import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import queue
import re
import subprocess
import sys
import tempfile
import threading
import time

GROUPS = {
    "indexer": "/internal/indexer",
    "mcp": "/internal/mcp",
    "store": "/internal/graph/store_sqlite",
}
REST = "rest"
# The pure-Go SQLite driver dominates -race time: instrumenting its
# transpiled C makes store tests about 9x slower. Race checks still cover
# every gortex package, and every access our code makes to shared memory.
RACE_FLAGS = ["-race", "-gcflags=modernc.org/...=-race=false"]
COVER_FLAGS = ["-cover", "-covermode=atomic"]
# macOS runs without -race, but its FSEvents and kqueue watchers start
# goroutines no other platform compiles. These small packages keep race
# checks there.
DARWIN_RACE_PACKAGES = ["/internal/thirdparty/fswatcher", "/internal/thirdparty/fsnotify"]
TEST_BUDGET_SECONDS = 35 * 60
BATCHES_PER_WORKER = 8
RESULT = re.compile(r"^--- (PASS|FAIL|SKIP): (\S+) \(([\d.]+)s\)")
RACE_REPORT = "WARNING: DATA RACE"
TIMEOUT_REPORT = "panic: test timed out"
PANIC = re.compile(r"(?m)^panic: ")


def capture(command, **kwargs):
    return subprocess.check_output(command, text=True, encoding="utf-8", **kwargs)


def hard_failure(output):
    """A data race or a panic (a test timeout is one) is a bug, not a flake.

    A panic also ends the test binary, so the tests after it never ran.
    """
    return RACE_REPORT in output or bool(PANIC.search(output))


def parse_partition(spec):
    """Return [(group, shard, count)] with 0-based shards; reject anything else."""
    items = []
    for item in spec.split("+"):
        match = re.fullmatch(r"([a-z]+)(?::([1-9]\d*)/([1-9]\d*))?", item)
        if not match:
            raise ValueError(f"bad partition item {item!r}: want group or group:i/n")
        group, shard, count = match.group(1), int(match.group(2) or 1), int(match.group(3) or 1)
        if group != REST and group not in GROUPS:
            raise ValueError(f"unknown group {group!r}")
        if shard > count or (group == REST and count != 1):
            raise ValueError(f"bad shard in {item!r}")
        items.append((group, shard - 1, count))
    if len({group for group, _, _ in items}) != len(items):
        raise ValueError(f"partition {spec!r} names a group twice")
    return items


def check_layout(rows):
    """Every OS must run every group exactly once: all n shards of one count."""
    by_os = {}
    for os_name, spec in rows:
        for group, shard, count in parse_partition(spec):
            by_os.setdefault(os_name, {}).setdefault(group, []).append((shard, count))
    expected = set(GROUPS) | {REST}
    for os_name, groups in by_os.items():
        if set(groups) != expected:
            raise ValueError(f"{os_name} runs groups {sorted(groups)}, want {sorted(expected)}")
        for group, shards in groups.items():
            counts = {count for _, count in shards}
            if len(counts) != 1 or sorted(shard for shard, _ in shards) != list(range(counts.pop())):
                raise ValueError(f"{os_name} {group} shards {sorted(shards)} are not exactly 1..n of one n")
    return by_os


def workflow_rows(text):
    """(os, partition) jobs of ci.yml's test matrix: os x partition lists, plus includes."""
    def values(key):
        match = re.search(rf"(?m)^\s+{key}:\s*\[([^\]]*)\]", text)
        return [value.strip().strip("'\"") for value in match.group(1).split(",")] if match else []

    rows = [(os_name, partition) for os_name in values("os") for partition in values("partition")]
    for body in re.findall(r"\{([^{}]*\bpartition:[^{}]*)\}", text):
        fields = dict(part.split(":", 1) for part in body.split(",") if ":" in part)
        fields = {key.strip(): value.strip().strip("'\"") for key, value in fields.items()}
        rows.append((fields["os"], fields["partition"]))
    if not rows:
        raise ValueError("found no test matrix rows (os/partition) in the workflow")
    return rows


def exact_pattern(names):
    # Factoring shared prefixes keeps Windows' process command line short.
    # Anchors apply only to top-level names; Go runs all their subtests.
    trie = {}
    for name in names:
        node = trie
        for char in name:
            node = node.setdefault(char, {})
        node[""] = {}

    def render(node):
        suffixes = {}
        for char, child in sorted(node.items()):
            if char:
                suffixes.setdefault(render(child), []).append(char)
        alternatives = []
        for suffix, chars in suffixes.items():
            prefix = (re.escape(chars[0]) if len(chars) == 1 else
                      "[" + "".join(re.escape(char) for char in chars) + "]")
            alternatives.append(prefix + suffix)
        if not alternatives:
            return ""
        body = alternatives[0] if len(alternatives) == 1 else "(?:" + "|".join(alternatives) + ")"
        return "(?:" + body + ")?" if "" in node else body

    return "^(?:" + render(trie) + ")$"


def test_names(output):
    names = []
    for line in output.splitlines():
        if line.startswith(("Test", "Example", "Fuzz")):
            if not line.isidentifier():
                raise ValueError(f"unexpected test census line: {line!r}")
            names.append(line)
    if not names or len(names) != len(set(names)):
        raise ValueError("compiled test census must be nonempty and unique")
    return sorted(names)


def plan_batches(names, shard, count, batches):
    # Round-robin twice: name families (TestDirtyChain*, TestKindParity*) are
    # often slow together, so spreading them evens out shards and batches.
    selected = names[shard::count]
    if not selected:
        raise ValueError("every shard must have at least one compiled test")
    batches = min(batches, len(selected))
    return [selected[index::batches] for index in range(batches)]


def parse_results(output):
    results = {}
    for line in output.splitlines():
        match = RESULT.match(line)
        if match and "/" not in match.group(2):
            results[match.group(2)] = (match.group(1), float(match.group(3)))
    return results


def test_output(output, name):
    """The log lines of one top-level test, or all of them if it cannot tell."""
    lines = output.splitlines()
    start = next((index for index, line in enumerate(lines) if line == f"=== RUN   {name}"), None)
    end = next((index for index, line in enumerate(lines)
                if line.startswith(f"--- FAIL: {name} (")), None)
    if start is None or end is None or end < start:
        return output
    # Panics print after the FAIL line; keep everything up to the next test.
    tail = next((index for index in range(end + 1, len(lines))
                 if lines[index].startswith("=== RUN   ") and "/" not in lines[index]), len(lines))
    return "\n".join(lines[start:tail])


def merge_coverage(paths, destination):
    mode, blocks = None, {}
    for path in paths:
        if not path.exists():
            continue
        for line in path.read_text().splitlines():
            if line.startswith("mode: "):
                if mode not in (None, line):
                    raise ValueError(f"coverage modes differ: {mode} and {line}")
                mode = line
            elif line:
                block, count = line.rsplit(" ", 1)
                blocks[block] = blocks.get(block, 0) + int(count)
    if mode is None:
        return False
    with destination.open("w") as out:
        out.write(mode + "\n")
        out.writelines(f"{block} {count}\n" for block, count in blocks.items())
    return True


class Job:
    def __init__(self, race, windows, workers, scratch, deadline):
        self.race, self.windows, self.workers = race, windows, workers
        self.scratch, self.deadline = scratch, deadline
        self.durations = {}
        self.failures = []  # (label, name or None, output, rerunnable)
        self.flaky = []
        self.coverage = []
        self.slots = queue.Queue()
        for slot in range(workers):
            self.slots.put(slot)
        self.lock = threading.Lock()
        self.base_env = dict(os.environ)
        for key in ("GOCACHE", "GOMODCACHE", "GOPATH", "GOENV"):
            self.base_env[key] = capture(["go", "env", key]).strip()

    def remaining(self):
        left = self.deadline - time.monotonic()
        if left <= 0:
            raise ValueError("test execution budget exhausted")
        return left

    def env(self, slot):
        # Each worker gets its own home and temp directories, so tests of one
        # package that touch the user's config or cache never share them.
        root = self.scratch / f"w{slot}"
        home, temp = root / "h", root / "t"
        temp.mkdir(parents=True, exist_ok=True)
        home.mkdir(parents=True, exist_ok=True)
        env = dict(self.base_env, HOME=str(home), TMPDIR=str(temp), TMP=str(temp), TEMP=str(temp))
        if self.windows:
            env.update(USERPROFILE=str(home), APPDATA=str(home / "AppData" / "Roaming"),
                       LOCALAPPDATA=str(home / "AppData" / "Local"))
        else:
            env.update(XDG_CONFIG_HOME=str(home / ".config"), XDG_CACHE_HOME=str(home / ".cache"),
                       XDG_DATA_HOME=str(home / ".local" / "share"),
                       XDG_STATE_HOME=str(home / ".local" / "state"))
        return env

    def run(self, command, slot, log, **kwargs):
        left = self.remaining()
        with log.open("w", encoding="utf-8") as out:
            try:
                code = subprocess.call(command, env=self.env(slot), stdout=out,
                                       stderr=subprocess.STDOUT, timeout=left + 120, **kwargs)
            except subprocess.TimeoutExpired:
                out.write(f"\n{TIMEOUT_REPORT}: the test process outlived the job budget\n")
                code = -1
        return code, log.read_text(encoding="utf-8", errors="replace")

    def build_flags(self):
        return RACE_FLAGS + COVER_FLAGS if self.race else []

    def record(self, label, results):
        with self.lock:
            for name, (_, seconds) in results.items():
                self.durations[f"{label} {name}"] = seconds

    def fail(self, label, name, output, rerunnable):
        with self.lock:
            self.failures.append((label, name, output, rerunnable))

    # Large packages: one compiled binary, batches spread across workers.

    def run_package(self, group, package, shard, count, directory):
        binary = self.scratch / (group + (".test.exe" if self.windows else ".test"))
        started = time.monotonic()
        subprocess.run(["go", "test", "-c", "-o", str(binary)] + self.build_flags() + [package], check=True)
        names = test_names(capture([str(binary), "-test.list", "."], cwd=directory))
        batches = plan_batches(names, shard, count, BATCHES_PER_WORKER * self.workers)
        label = f"{group}:{shard + 1}/{count}"
        print(f"{label}: {package} has {len(names)} tests; this shard runs "
              f"{sum(map(len, batches))} in {len(batches)} batches on {self.workers} workers "
              f"(built in {time.monotonic() - started:.0f}s)", flush=True)

        def batch(index, names):
            slot = self.slots.get()
            try:
                return self.run_batch(label, binary, directory, names, slot, f"{group}-{index}")
            finally:
                self.slots.put(slot)

        with ThreadPoolExecutor(self.workers) as pool:
            reruns = [name for result in pool.map(batch, range(len(batches)), batches) for name in result]
        for name in reruns:
            self.rerun_binary(label, binary, directory, name)
        print(f"{label}: done in {time.monotonic() - started:.0f}s", flush=True)

    def binary_command(self, binary, names, cover):
        command = [str(binary), "-test.run", exact_pattern(names), "-test.v",
                   "-test.paniconexit0", f"-test.timeout={self.remaining():.0f}s"]
        return command + (["-test.coverprofile", str(cover)] if self.race else [])

    def run_batch(self, label, binary, directory, names, slot, tag):
        log = self.scratch / "logs" / f"{tag}.log"
        cover = self.scratch / "coverage" / f"{tag}.out"
        self.coverage.append(cover)
        started = time.monotonic()
        code, output = self.run(self.binary_command(binary, names, cover), slot, log, cwd=directory)
        results = parse_results(output)
        self.record(label, results)
        failed = [name for name in names if results.get(name, ("",))[0] == "FAIL"]
        missing = [name for name in names if name not in results]
        print(f"{label} [{tag}] {len(names)} tests, exit {code}, {time.monotonic() - started:.0f}s", flush=True)
        if code == 0 and not failed and not missing:
            return []
        if code == 0 or hard_failure(output) or not failed:
            # A clean exit that skipped tests, a race, a panic (a timeout is
            # one), or an exit outside any test: none is a contention flake.
            for name in failed:
                self.fail(label, name, test_output(output, name), False)
            if not failed:
                self.fail(label, None, output, False)
            return []
        for name in failed:
            self.fail(label, name, test_output(output, name), True)
        # A test that exits the binary (log.Fatal) leaves later tests unrun;
        # they get their first run alone.
        return failed + missing

    def rerun_binary(self, label, binary, directory, name):
        tag = f"rerun-{label.replace(':', '-').replace('/', 'of')}-{name}"
        cover = self.scratch / "coverage" / f"{tag}.out"
        self.coverage.append(cover)
        code, output = self.run(self.binary_command(binary, [name], cover), 0,
                                self.scratch / "logs" / f"{tag}.log", cwd=directory)
        result = parse_results(output).get(name, ("",))[0]
        self.resolve(label, name, code == 0 and result in ("PASS", "SKIP"), output)

    def resolve(self, label, name, passed, output):
        with self.lock:
            first = [entry for entry in self.failures if entry[:2] == (label, name)]
            for entry in first:
                self.failures.remove(entry)
            if passed:
                if first:
                    self.flaky.append(f"{label} {name}")
            else:
                self.failures.append((label, name, output, False))

    # Every other package: one go test over all of them.

    def run_rest(self, packages, race_packages):
        started = time.monotonic()
        cover = self.scratch / "coverage" / "rest.out"
        self.coverage.append(cover)
        command = (["go", "test", "-json", "-count=1", f"-p={self.workers}", f"-timeout={self.remaining():.0f}s"]
                   + self.build_flags() + (["-coverprofile", str(cover)] if self.race else []))
        reruns = self.run_go_test("rest", command, packages, "rest")
        if race_packages:
            # The race subset runs alone: plain -cover off, -race on.
            reruns += self.run_go_test("rest-race", ["go", "test", "-json", "-count=1", f"-p={self.workers}",
                                                     f"-timeout={self.remaining():.0f}s"] + RACE_FLAGS,
                                       race_packages, "rest-race")
        for label, package, names in reruns:
            flags = RACE_FLAGS if label == "rest-race" else self.build_flags()
            for name in names:
                tag = f"rerun-{label}-{package.rsplit('/', 1)[-1]}-{name}"
                rerun_cover = self.scratch / "coverage" / f"{tag}.out"
                command = ["go", "test", "-json", "-count=1", f"-timeout={self.remaining():.0f}s"] + flags
                if self.race and label == "rest":
                    command += ["-coverprofile", str(rerun_cover)]
                    self.coverage.append(rerun_cover)
                code, output = self.run(command + ["-run", exact_pattern([name]), package], 0,
                                        self.scratch / "logs" / f"{tag}.log")
                events = go_test_events(output)
                result = events["tests"].get((package, name), ("",))[0]
                self.resolve(f"{label} {package}", name, code == 0 and result in ("pass", "skip"),
                             events["output"].get((package, name), output))
        print(f"rest: done in {time.monotonic() - started:.0f}s", flush=True)

    def run_go_test(self, label, command, packages, tag):
        code, output = self.run(command + packages, 0, self.scratch / "logs" / f"{tag}.log")
        events = go_test_events(output)
        for (package, name), (_, seconds) in events["tests"].items():
            self.durations[f"{label} {package} {name}"] = seconds
        for package, (action, seconds) in sorted(events["packages"].items()):
            print(f"{action:4} {package} {seconds:.1f}s", flush=True)
        reruns = []
        for package in packages:
            action = events["packages"].get(package, ("missing",))[0]
            if action in ("pass", "skip"):
                continue
            text = events["package_output"].get(package, "")
            failed = sorted(name for (pkg, name), (result, _) in events["tests"].items()
                            if pkg == package and result == "fail")
            rerunnable = action == "fail" and not hard_failure(text)
            for name in failed:
                self.fail(f"{label} {package}", name, events["output"].get((package, name), text), rerunnable)
            if not failed or not rerunnable:
                # A build failure, a crash outside any test, a race or a timeout.
                if not failed:
                    self.fail(f"{label} {package}", None, text or output, False)
                continue
            reruns.append((label, package, failed))
        if code != 0 and not any(entry[0].startswith(label + " ") for entry in self.failures):
            self.fail(label, None, output, False)
        return reruns

    def report(self):
        slowest = sorted(self.durations.items(), key=lambda item: -item[1])[:20]
        lines = ["### Slowest tests", "", "| seconds | test |", "| ---: | --- |"]
        lines += [f"| {seconds:.1f} | `{name}` |" for name, seconds in slowest]
        print("\nSlowest tests:", flush=True)
        for name, seconds in slowest:
            print(f"  {seconds:8.1f}s {name}")
        if self.flaky:
            lines += ["", "### Flaky: failed beside other tests, passed alone", ""]
            lines += [f"- `{name}`" for name in self.flaky]
            for name in self.flaky:
                print(f"::warning title=Flaky test::{name} failed while sharing the machine and passed when rerun alone")
        for label, name, output, _ in self.failures:
            title = f"{label} {name}" if name else label
            print(f"\n::group::FAIL {title}\n{output.rstrip()}\n::endgroup::")
            print(f"::error title=Test failed::{title}")
        if self.failures:
            lines += ["", "### Failed", ""] + [f"- `{label} {name or ''}`" for label, name, _, _ in self.failures]
        summary = os.environ.get("GITHUB_STEP_SUMMARY")
        if summary:
            with open(summary, "a", encoding="utf-8") as out:
                out.write("\n".join(lines) + "\n")


def go_test_events(output):
    """Index `go test -json` output by package and top-level test."""
    tests, packages, test_out, package_out = {}, {}, {}, {}
    for line in output.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        package, name, action = event.get("Package"), event.get("Test"), event.get("Action")
        if action == "output":
            package_out[package] = package_out.get(package, "") + event.get("Output", "")
            if name and "/" not in name:
                test_out[(package, name)] = test_out.get((package, name), "") + event.get("Output", "")
        elif action in ("pass", "fail", "skip"):
            if name is None:
                packages[package] = (action, event.get("Elapsed", 0.0))
            elif "/" not in name:
                tests[(package, name)] = (action, event.get("Elapsed", 0.0))
        elif action == "build-fail" or (action is None and event.get("ImportPath")):
            package_out[event.get("ImportPath")] = package_out.get(event.get("ImportPath"), "") + line + "\n"
    return {"tests": tests, "packages": packages, "output": test_out, "package_output": package_out}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--partition", default=os.environ.get("TEST_PARTITION"), required=False)
    parser.add_argument("--check-workflow", type=Path, help="validate this workflow's test layout and exit")
    args = parser.parse_args()
    if args.check_workflow:
        layout = check_layout(workflow_rows(args.check_workflow.read_text()))
        print(f"{args.check_workflow}: test layout covers every group on {sorted(layout)}")
        return
    if not args.partition:
        parser.error("set --partition or TEST_PARTITION")
    items = parse_partition(args.partition)
    windows = os.environ.get("TEST_WINDOWS", "false") == "true"
    race_setting = os.environ.get("TEST_RACE", "false" if windows else "true")
    if race_setting not in ("true", "false") or (windows and race_setting == "true"):
        parser.error("TEST_RACE must be true or false, and false on Windows")
    race = race_setting == "true"
    workers = int(os.environ.get("TEST_WORKERS") or os.cpu_count() or 1)
    module = capture(["go", "list", "-m"]).strip()
    packages = capture(["go", "list", "./..."]).splitlines()
    if not packages or len(packages) != len(set(packages)):
        raise ValueError("package census must be nonempty and unique")
    large = {group: module + suffix for group, suffix in GROUPS.items()}
    if not set(large.values()) <= set(packages):
        raise ValueError(f"missing large packages: {sorted(set(large.values()) - set(packages))}")
    # Short paths: tests bind Unix sockets under the worker's temp directory,
    # and macOS caps a socket path at 104 bytes.
    base = os.environ.get("RUNNER_TEMP") or ("/tmp" if os.path.isdir("/tmp") else tempfile.gettempdir())
    root = Path(base) / "gt"
    root.mkdir(parents=True, exist_ok=True)
    scratch = Path(tempfile.mkdtemp(prefix="j", dir=root))
    for name in ("logs", "coverage"):
        (scratch / name).mkdir()
    if windows:
        # Keep go's build scratch beside the tests', off the system drive.
        os.environ["GOTMPDIR"] = str(root)
    job = Job(race, windows, workers, scratch, time.monotonic() + TEST_BUDGET_SECONDS)
    print(f"partition {args.partition}: race={race} workers={workers} scratch={scratch}", flush=True)
    try:
        for group, shard, count in items:
            if group == REST:
                darwin_race = [module + suffix for suffix in DARWIN_RACE_PACKAGES] \
                    if sys.platform == "darwin" and not race else []
                rest = [package for package in packages
                        if package not in large.values() and package not in darwin_race]
                job.run_rest(rest, darwin_race)
            else:
                directory = capture(["go", "list", "-f", "{{.Dir}}", large[group]]).strip()
                job.run_package(group, large[group], shard, count, directory)
    finally:
        job.report()
        if race:
            merge_coverage(job.coverage, Path("coverage.out"))
        print(f"\nlogs: {scratch / 'logs'}", flush=True)
    if job.failures:
        sys.exit(1)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, subprocess.CalledProcessError) as error:
        print(f"CI test partition failed: {error}", file=sys.stderr)
        sys.exit(1)
