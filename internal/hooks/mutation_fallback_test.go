package hooks

import (
	"bytes"
	"encoding/json"
	"github.com/zzet/gortex/internal/daemon"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

const legacyStaleRefusal = "view_read_only: checkout mutation was not admitted; no files were changed: indexer: checkout mutation view is stale: checkout disk changed"

func mutationRootFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return mutationCheckoutRoot(root, "", true)
}

func mutationPostFixture(root, session string, failed bool, text string) postHookInput {
	return postHookInput{
		HookEventName: "PostToolUse", CWD: root, SessionID: session, ToolName: "mcp__gortex__edit",
		ToolInput:    map[string]any{"operation": "file", "target": map[string]any{"file": "a.go"}},
		ToolResponse: map[string]any{"isError": failed, "content": []any{map[string]any{"type": "text", "text": text}}},
	}
}

func TestReadOnlyFallbackScopesTTLAndSuccessfulMutation(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	root, sibling := mutationRootFixture(t), mutationRootFixture(t)
	at := time.Unix(1600, 0)
	post := mutationPostFixture(root, "one", true, legacyStaleRefusal)
	if !observeMutationFallback(post, at) {
		t.Fatal("actual failed mutation not observed")
	}
	st := loadSessionState("one")
	for _, tc := range []struct {
		now  time.Time
		live bool
	}{
		{at.Add(-time.Nanosecond), false}, {at, true},
		{at.Add(10*time.Minute - time.Nanosecond), true}, {at.Add(10 * time.Minute), false},
	} {
		if live := mutationFallbackLive(st, root, tc.now); live != tc.live {
			t.Fatalf("at %v: live=%v", tc.now, live)
		}
	}
	if mutationFallbackLive(st, sibling, at) || mutationFallbackLive(loadSessionState("two"), root, at) {
		t.Fatal("refusal escaped checkout/session scope")
	}
	// A success containing the exact refusal words clears, rather than arms,
	// state. Only the MCP result flag establishes the outcome.
	post.ToolResponse = mutationPostFixture(root, "one", false, legacyStaleRefusal).ToolResponse
	observeMutationFallback(post, at.Add(time.Minute))
	if mutationFallbackLive(loadSessionState("one"), root, at.Add(time.Minute)) {
		t.Fatal("successful mutation retained refusal")
	}
}

func TestReadOnlyFallbackNeverArmsFromUnprovenOrOtherToolFailures(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	root := mutationRootFixture(t)
	at := time.Unix(1600, 0)
	for name, change := range map[string]func(*postHookInput){
		"plain_text":            func(p *postHookInput) { p.ToolResponse = legacyStaleRefusal },
		"serialized_error_text": func(p *postHookInput) { data, _ := json.Marshal(p.ToolResponse); p.ToolResponse = string(data) },
		"success":               func(p *postHookInput) { p.ToolResponse.(map[string]any)["isError"] = false },
		"read_tool":             func(p *postHookInput) { p.ToolName = "mcp__gortex__read" },
		"other_server":          func(p *postHookInput) { p.ToolName = "mcp__other__edit_file" },
		"preview":               func(p *postHookInput) { p.ToolInput["operation"] = "preview" },
		"refactor":              func(p *postHookInput) { p.ToolName = "mcp__gortex__refactor" },
		"generic_readonly": func(p *postHookInput) {
			p.ToolResponse = mutationPostFixture(root, "s", true, "view_read_only: save buffers first").ToolResponse
		},
		"busy": func(p *postHookInput) {
			p.ToolResponse = mutationPostFixture(root, "s", true, legacyStaleRefusal+"; indexer: checkout mutation lane is busy; retry").ToolResponse
		},
		"pending": func(p *postHookInput) {
			p.ToolResponse = mutationPostFixture(root, "s", true, legacyStaleRefusal+"; indexer: checkout mutation refresh is pending").ToolResponse
		},
		"id_only_selector": func(p *postHookInput) {
			p.ToolInput["view"] = map[string]any{"kind": "worktree", "checkout_id": "unresolved"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			post := mutationPostFixture(root, name, true, legacyStaleRefusal)
			change(&post)
			observeMutationFallback(post, at)
			if mutationFallbackLive(loadSessionState(name), root, at) {
				t.Fatal("unproven failed source mutation armed fallback")
			}
		})
	}
}

func TestReadOnlyFallbackModernReadyAndUnreachedDoNotRelaxPolicy(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	root := mutationRootFixture(t)
	observeMutationFallback(mutationPostFixture(root, "s", true, legacyStaleRefusal), time.Now())
	before, err := os.ReadFile(sessionStatePath("s"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		status   fileIndexStatus
		readOnly bool
	}{
		{"legacy_indexed", fileIndexStatus{Indexed: true, Count: 3, ProbeOK: true, Tracked: true}, true},
		{"modern_ready", fileIndexStatus{Indexed: true, Count: 3, ProbeOK: true, Tracked: true, MutationKnown: true}, false},
		{"modern_refused", fileIndexStatus{Indexed: true, Count: 3, ProbeOK: true, Tracked: true, MutationKnown: true, ReadOnly: true}, true},
		{"unreached", fileIndexStatus{Unreached: true}, false},
		{"abstention", fileIndexStatus{Tracked: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes := 0
			stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus { probes++; return tc.status })
			st := sessionFileScopeProbe("s")(root, "a.go")
			if st.ReadOnly != tc.readOnly || probes != 1 {
				t.Fatalf("state=%+v probes=%d", st, probes)
			}
			if !tc.readOnly && !reflect.DeepEqual(st, tc.status) {
				t.Fatalf("healthy/unreached/unknown policy changed: %+v -> %+v", tc.status, st)
			}
		})
	}
	after, err := os.ReadFile(sessionStatePath("s"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("PreToolUse probe rewrote the whole session record")
	}
}

func TestReadOnlyFallbackNativeDoorsAndCodexRewriteUseOneProbe(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	root := mutationRootFixture(t)
	observeMutationFallback(mutationPostFixture(root, "s", true, legacyStaleRefusal), time.Now())
	probes := 0
	stubFileIndexScopeBy(t, func(_, _ string) fileIndexStatus {
		probes++
		return fileIndexStatus{Indexed: true, Count: 3, ProbeOK: true, Tracked: true}
	})
	for _, door := range []func(fileScopeProbe) enrichResult{
		func(p fileScopeProbe) enrichResult {
			return enrichReadWithProbe(map[string]any{"file_path": "a.go"}, root, p)
		},
		func(p fileScopeProbe) enrichResult {
			return enrichBashWithProbe(map[string]any{"command": "cat a.go"}, root, p)
		},
		func(p fileScopeProbe) enrichResult {
			return enrichEditWithProbe(map[string]any{"file_path": "a.go"}, root, p)
		},
		func(p fileScopeProbe) enrichResult {
			return enrichWriteWithProbe(map[string]any{"file_path": "a.go"}, root, p)
		},
	} {
		before := probes
		if result := door(sessionFileScopeProbe("s")); result.deny || !result.readOnlyAllowance || probes != before+1 {
			t.Fatalf("native fallback/probe count: %+v probes=%d", result, probes-before)
		}
	}
	// Exercise the actual Codex rewrite handler: a refused cat must stay a
	// native cat with advisory, without the rewrite attempt probing twice.
	oldRepos := hookTrackedReposFn
	hookTrackedReposFn = func() []daemon.TrackedRepoStatus { return []daemon.TrackedRepoStatus{{Path: root, Prefix: "fixture"}} }
	t.Cleanup(func() { hookTrackedReposFn = oldRepos })
	data, err := json.Marshal(HookInput{HookEventName: "PreToolUse", ToolName: "Bash", SessionID: "s", CWD: root, ToolInput: map[string]any{"command": "cat a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	before := probes
	output := captureHookStdout(t, func() { runCodexBashRewrite(data, 0) })
	var result HookOutput
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output=%q: %v", output, err)
	}
	if result.HookSpecificOutput == nil || result.HookSpecificOutput.PermissionDecision == "deny" || result.HookSpecificOutput.UpdatedInput != nil || probes != before+1 {
		t.Fatalf("rewrite closed native path or reprobed: %+v probes=%d", result, probes-before)
	}
}

func TestReadOnlyFallbackCheckoutBoundaryAndSymlinkIsolation(t *testing.T) {
	t.Setenv(hookSessionDirEnvVar, t.TempDir())
	root, other := mutationRootFixture(t), mutationRootFixture(t)
	post := mutationPostFixture(root, "s", true, legacyStaleRefusal)
	post.ToolInput["view"] = map[string]any{"kind": "worktree", "path": other}
	observeMutationFallback(post, time.Now())
	st := loadSessionState("s")
	if mutationFallbackLive(st, root, time.Now()) || !mutationFallbackLive(st, other, time.Now()) {
		t.Fatal("explicit selected checkout inherited caller CWD")
	}
	link := filepath.Join(root, "elsewhere")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if got := mutationCheckoutRoot(root, filepath.Join(link, "new.go"), false); got != other {
		t.Fatalf("new file through symlink borrowed wrong checkout: %s", got)
	}
}
