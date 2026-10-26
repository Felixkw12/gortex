package scripts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

type ciNeed struct {
	Result  string            `json:"result"`
	Outputs map[string]string `json:"outputs,omitempty"`
}

// This is one concrete, valid affected-PR receipt. Other scenarios alter the
// receipt and invoke the production gate, rather than reimplement its rules.
func affectedPRNeeds() map[string]ciNeed {
	return map[string]ciNeed{
		"plan": {Result: "success", Outputs: map[string]string{
			"mode": "affected", "has_tests": "true", "empty_ok": "false",
		}},
		"test":               {Result: "success"},
		"smoke":              {Result: "success"},
		"build-native":       {Result: "success"},
		"lint":               {Result: "success"},
		"build-linux-static": {Result: "success"},
		"build-onnx":         {Result: "success"},
		"benchmark":          {Result: "success"},
		"parser-stress":      {Result: "skipped"},
	}
}

func gatePython(t *testing.T) string {
	t.Helper()
	// Windows runners provide python; Unix runners normally provide python3.
	// Resolve argv directly and require Python 3, without a shell or aliases.
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = []string{"python", "python3"}
	}
	for _, name := range names {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		err = exec.CommandContext(ctx, path, "-c", "import sys; sys.exit(sys.version_info.major != 3)").Run()
		cancel()
		if err == nil {
			return path
		}
	}
	t.Fatal("CI gate tests require Python 3 (python3 or python), as the production gate does")
	return ""
}

func runCIGate(t *testing.T, python, event string, needs map[string]ciNeed, wantError string) {
	t.Helper()
	payload, err := json.Marshal(needs)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "ci_test_gate.py")
	cmd.Env = append(os.Environ(), "CI_NEEDS="+string(payload), "GITHUB_EVENT_NAME="+event)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("gate timed out: %v\n%s", ctx.Err(), output)
	}
	if wantError == "" {
		if err != nil || !strings.Contains(string(output), "All required CI checks passed") {
			t.Fatalf("gate rejected a valid receipt: %v\n%s", err, output)
		}
		return
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 0 {
		t.Fatalf("gate must reject the receipt with a nonzero exit: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "ValueError: "+wantError) {
		t.Fatalf("gate rejected for the wrong reason; want %q:\n%s", wantError, output)
	}
}

func TestCIResultGateAcceptsValidReceipts(t *testing.T) {
	python := gatePython(t)
	for _, tc := range []struct {
		name, event string
		outputs     map[string]string
		results     map[string]string
	}{
		{name: "affected PR", event: "pull_request"},
		{name: "explicit empty PR", event: "pull_request",
			outputs: map[string]string{"has_tests": "false", "empty_ok": "true"},
			results: map[string]string{"test": "skipped"}},
		{name: "full PR", event: "pull_request", outputs: map[string]string{"mode": "full"},
			results: map[string]string{"smoke": "skipped"}},
		{name: "full main", event: "push", outputs: map[string]string{"mode": "full"},
			results: map[string]string{"smoke": "skipped", "benchmark": "skipped"}},
		{name: "full manual", event: "workflow_dispatch", outputs: map[string]string{"mode": "full"},
			results: map[string]string{"smoke": "skipped", "benchmark": "skipped", "parser-stress": "success"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needs := affectedPRNeeds()
			for key, value := range tc.outputs {
				needs["plan"].Outputs[key] = value
			}
			for job, result := range tc.results {
				needs[job] = ciNeed{Result: result}
			}
			runCIGate(t, python, tc.event, needs, "")
		})
	}
}

func TestCIResultGateRejectsFailedCancelledOrUnexpectedlySkippedJobs(t *testing.T) {
	python := gatePython(t)
	for job, receipt := range affectedPRNeeds() {
		for _, result := range []string{"failure", "cancelled", "skipped"} {
			if result == receipt.Result {
				continue // parser-stress is legitimately skipped for PRs.
			}
			t.Run(fmt.Sprintf("%s/%s", job, result), func(t *testing.T) {
				needs := affectedPRNeeds()
				changed := needs[job]
				changed.Result = result
				needs[job] = changed
				runCIGate(t, python, "pull_request", needs, job+" must be ")
			})
		}
	}
}

func TestCIResultGateRejectsInvalidPlanningOutputs(t *testing.T) {
	python := gatePython(t)
	for _, tc := range []struct {
		name, wantError string
		outputs         map[string]string
	}{
		{name: "unvalidated empty", wantError: "empty test selection was not explicitly validated",
			outputs: map[string]string{"mode": "affected", "has_tests": "false", "empty_ok": "false"}},
		{name: "missing outputs", wantError: "planning outputs are missing or invalid", outputs: map[string]string{}},
		{name: "invalid has_tests", wantError: "planning outputs are missing or invalid",
			outputs: map[string]string{"mode": "affected", "has_tests": "oops"}},
		{name: "empty full suite", wantError: "empty test selection was not explicitly validated",
			outputs: map[string]string{"mode": "full", "has_tests": "false", "empty_ok": "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			needs := affectedPRNeeds()
			needs["plan"] = ciNeed{Result: "success", Outputs: tc.outputs}
			if tc.outputs["has_tests"] == "false" {
				needs["test"] = ciNeed{Result: "skipped"}
			}
			runCIGate(t, python, "pull_request", needs, tc.wantError)
		})
	}
}

func TestCIWorkflowWiresSelectionBuildsCoverageAndStableGate(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	lf := strings.ReplaceAll(string(data), "\r\n", "\n")
	for _, tc := range []struct{ name, text string }{
		{"LF", lf},
		{"CRLF", strings.ReplaceAll(lf, "\n", "\r\n")},
	} {
		t.Run(tc.name, func(t *testing.T) { assertCIWorkflowWiring(t, tc.text) })
	}
}

func assertCIWorkflowWiring(t *testing.T, input string) {
	t.Helper()
	// Git may check the workflow out with core.autocrlf on Windows. Match
	// Python read_text's universal-newline behavior on both checkout forms.
	text := strings.ReplaceAll(input, "\r\n", "\n")
	for _, required := range []string{
		"matrix: ${{ fromJSON(needs.plan.outputs.matrix) }}",
		"CI_FULL_TESTS: ${{ vars.CI_FULL_TESTS }}",
		"CI_BASE_SHA: ${{ github.event.pull_request.base.sha }}",
		"fetch-depth: 0", "--selection test-selection.json",
		"needs.plan.outputs.mode == 'full'", "name: affected-coverage-",
		"  build-native:\n", "  lint:\n", "  build-linux-static:\n", "  build-onnx:\n", "  ci-result:\n",
		"if: always()", "go test -count=1 ./scripts", "run: python3 scripts/ci_test_gate.py",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("workflow is missing required wiring %q", required)
		}
	}
	events := regexp.MustCompile(`(?m)^    types: \[([^\]]+)\]`).FindStringSubmatch(text)
	if len(events) != 2 {
		t.Fatal("workflow must specify its pull request event types")
	}
	got := make(map[string]bool)
	for _, event := range strings.Split(events[1], ", ") {
		got[event] = true
	}
	want := map[string]bool{"opened": true, "synchronize": true, "reopened": true, "labeled": true, "unlabeled": true}
	if !maps.Equal(got, want) {
		t.Errorf("pull request events = %v, want %v", got, want)
	}
}
