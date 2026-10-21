package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graphview"
)

func TestPlainByteEditUsesSourceAuthorityAcrossPendingPublication(t *testing.T) {
	f, blocked := newBlockedLifecycleFixture(t)
	old := f.edit(t, f.worktree, map[string]any{"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func LifecycleBlocked() {}", "format": "json"})
	require.False(t, old.IsError, viewResultText(t, old))
	select {
	case <-blocked.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("old publication never entered parser")
	}
	target := filepath.Join(f.worktree, "byte.go")
	before := []byte("package repo\nfunc ByteBefore() {}\n")
	require.NoError(t, os.WriteFile(target, before, 0600))
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = map[string]any{"path": "repo/byte.go", "old_string": "ByteBefore", "new_string": "ByteAfter", "base_sha": gitBlobSHA(before), "expected_occurrences": 1, "format": "json"}
	entered := make(chan struct{})
	result := make(chan *mcplib.CallToolResult, 1)
	ctx := WithSessionCWD(WithSessionID(t.Context(), "plain-byte-handoff"), f.worktree)
	go func() {
		r, err := f.srv.wrapToolHandler(func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			view := requestViewFromContext(ctx)
			if view == nil || view.sourceScope != "file" || view.materialized != nil || !view.sourceGraphPending || checkoutMutationFromContext(ctx) == nil {
				return mcplib.NewToolResultError("missing distinct source-byte authority"), nil
			}
			close(entered)
			return f.srv.handleEditFile(ctx, req)
		})(ctx, req)
		if err != nil {
			result <- mcplib.NewToolResultError(err.Error())
			return
		}
		result <- r
	}()
	select {
	case <-entered:
		t.Fatal("edit entered before old parser drained")
	case <-result:
		t.Fatal("edit returned before old parser drained")
	default:
	}
	disk, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, before, disk)
	blocked.unblock()
	var r *mcplib.CallToolResult
	select {
	case r = <-result:
	case <-time.After(20 * time.Second):
		t.Fatal("byte edit did not finish after drain")
	}
	require.False(t, r.IsError, viewResultText(t, r))
	select {
	case <-entered:
	default:
		t.Fatal("handler never received byte authority")
	}
	payload := decodeFileOpsResult(t, r)
	require.Equal(t, "applied", payload["status"])
	fresh := resultFreshness(t, r)
	require.Equal(t, "file", fresh["freshness_scope"])
	require.Equal(t, "pending", fresh["graph_freshness"])
	disk, err = os.ReadFile(target)
	require.NoError(t, err)
	require.Contains(t, string(disk), "ByteAfter")
	prior, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(prior), "LifecycleBlocked")
	f.awaitMutation(t, f.worktree, r)
}

func TestByteEditAuthorityKeepsTargetGuardsAndExcludedShapes(t *testing.T) {
	f := newRealCheckoutMutationFixture(t)
	original, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	args := func() map[string]any {
		return map[string]any{"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func EditedByte() {}", "base_sha": gitBlobSHA(original), "expected_occurrences": 1, "format": "json"}
	}
	for _, kind := range []string{"sha", "occurrence", "parse", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			a := args()
			switch kind {
			case "sha":
				a["base_sha"] = "0000000000000000000000000000000000000000"
			case "occurrence":
				a["expected_occurrences"] = 2
			case "parse":
				a["new_string"] = "func Broken("
			case "foreign":
				a["path"] = filepath.Join(f.primary, "edit.go")
			}
			r := f.edit(t, f.worktree, a)
			require.True(t, r.IsError, viewResultText(t, r))
			disk, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
			require.NoError(t, err)
			require.Equal(t, original, disk)
		})
	}
	for _, kind := range []string{"no_sha", "dry_run", "symbol", "batch", "write", "fresh", "capability"} {
		t.Run(kind, func(t *testing.T) {
			req := mcplib.CallToolRequest{}
			req.Params.Name = "edit_file"
			a := args()
			req.Params.Arguments = a
			freshness := requestFreshness{}
			caps := capabilityRequest{}
			switch kind {
			case "no_sha":
				delete(a, "base_sha")
			case "dry_run":
				a["dry_run"] = true
			case "symbol":
				req.Params.Name = "edit_symbol"
			case "batch":
				req.Params.Name = "batch_edit"
			case "write":
				req.Params.Name = "write_file"
			case "fresh":
				freshness.requireFresh = true
			case "capability":
				caps.required = []graphview.CapabilityID{graphview.CapSyntaxGraph}
			}
			_, eligible := f.srv.byteEditRequest(&req, freshness, caps)
			require.False(t, eligible)
		})
	}
}

func TestByteEditFacadeUsesNormalizedGuardAndSelectedDisk(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "facade-v1")
	f := newRealCheckoutMutationFixture(t)
	before, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	primary, err := os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	r := f.facade(t, f.primary, "edit", map[string]any{"operation": "file", "target": map[string]any{"file": "repo/edit.go"}, "match": "func New() {}", "replacement": "func ByteFacade() {}", "guard": map[string]any{"base_sha": gitBlobSHA(before), "expected_occurrences": 1}, "view": map[string]any{"kind": "worktree", "path": f.worktree}, "format": "json"})
	require.False(t, r.IsError, viewResultText(t, r))
	require.Equal(t, "file", resultFreshness(t, r)["freshness_scope"])
	disk, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(disk), "ByteFacade")
	disk, err = os.ReadFile(filepath.Join(f.primary, "edit.go"))
	require.NoError(t, err)
	require.Equal(t, primary, disk)
	f.awaitMutation(t, f.primary, r)
}

func TestRejectedByteEditResumesEarlierPendingPublication(t *testing.T) {
	f, blocked := newBlockedLifecycleFixture(t)
	prior := f.edit(t, f.worktree, map[string]any{"path": "repo/edit.go", "old_string": "func New() {}", "new_string": "func LifecycleBlocked() {}", "format": "json"})
	require.False(t, prior.IsError, viewResultText(t, prior))
	select {
	case <-blocked.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("earlier publication never reached barrier")
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit_file"
	req.Params.Arguments = map[string]any{"path": "repo/edit.go", "old_string": "LifecycleBlocked", "new_string": "MustNotWrite", "base_sha": "0000000000000000000000000000000000000000", "expected_occurrences": 1, "format": "json"}
	ctx := WithSessionCWD(WithSessionID(t.Context(), "rejected-byte-handoff"), f.worktree)
	result := make(chan *mcplib.CallToolResult, 1)
	go func() {
		r, err := f.srv.wrapToolHandler(func(ctx context.Context, r mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
			if sourceRequestView(ctx) == nil || checkoutMutationFromContext(ctx) == nil {
				return mcplib.NewToolResultError("missing byte authority"), nil
			}
			return f.srv.handleEditFile(ctx, r)
		})(ctx, req)
		if err != nil {
			result <- mcplib.NewToolResultError(err.Error())
			return
		}
		result <- r
	}()
	blocked.unblock()
	var r *mcplib.CallToolResult
	select {
	case r = <-result:
	case <-time.After(20 * time.Second):
		t.Fatal("rejected byte request did not close")
	}
	require.True(t, r.IsError)
	require.Contains(t, viewResultText(t, r), "base_sha mismatch")
	disk, err := os.ReadFile(filepath.Join(f.worktree, "edit.go"))
	require.NoError(t, err)
	require.Contains(t, string(disk), "LifecycleBlocked")
	require.NotContains(t, string(disk), "MustNotWrite")
	f.awaitMutation(t, f.worktree, prior)
}
