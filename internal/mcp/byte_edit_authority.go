package mcp

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

// byteEditRequest is deliberately narrower than generic edit dispatch. Unknown
// shapes retain graph admission. base_sha guards the target; the lifecycle's
// whole-checkout sample, not this SHA, establishes checkout source authority.
func (s *Server) byteEditRequest(req *mcp.CallToolRequest, freshness requestFreshness, caps capabilityRequest) (*mcp.CallToolRequest, bool) {
	if s == nil || s.lifecycle == nil || req == nil || freshness.requireFresh || freshness.hasDeadline || caps.requireComplete {
		return nil, false
	}
	for _, cap := range append(append([]graphview.CapabilityID{}, caps.required...), caps.optional...) {
		if cap != graphview.CapSourceSnapshot {
			return nil, false
		}
	}
	lowered := *req
	if isFacadeToolName(req.Params.Name) {
		spec, ok := s.viewFacadeOperation(req)
		if !ok || spec.Legacy != "edit_file" {
			return nil, false
		}
		lowered.Params.Name = spec.Legacy
		lowered.Params.Arguments = normalizeFacadeArguments(spec, req.GetArguments())
	} else if req.Params.Name != "edit_file" {
		return nil, false
	}
	path := lowered.GetString("path", "")
	old := lowered.GetString("old_string", "")
	next := lowered.GetString("new_string", "")
	sha := normalizeExpectedSHA(lowered.GetString("base_sha", ""))
	raw, err := hex.DecodeString(sha)
	if err != nil || len(raw) != 20 || len(sha) != 40 || path == "" || len(path) > 4096 || strings.ContainsAny(path, "\x00*?[]{}\n\r") || strings.Contains(path, "::") || old == "" || old == next || lowered.GetInt("expected_occurrences", 0) <= 0 || lowered.GetBool("dry_run", false) {
		return nil, false
	}
	return &lowered, true
}

// prepareByteEditAuthority runs after all tool/session/repository-scope gates,
// before awaitMutationRoute or graph materialization. Its view is explicitly a
// source-only authority with graph=pending, never an anchored graph reader.
func (s *Server) prepareByteEditAuthority(ctx context.Context, req *mcp.CallToolRequest, selector graphview.Selector, freshness requestFreshness, caps capabilityRequest) (context.Context, *requestView, func(), *mcp.CallToolResult) {
	noop := func() {}
	if checkoutMutationFromContext(ctx) != nil {
		return ctx, nil, noop, nil
	}
	lowered, ok := s.byteEditRequest(req, freshness, caps)
	if !ok || (selector.Kind != graphview.SelectorAuto && selector.Kind != graphview.SelectorWorktree) {
		return ctx, nil, noop, nil
	}
	if OverlayViewFromContext(ctx) != nil {
		return ctx, nil, noop, nil
	}
	snapshot, err := s.snapshotOverlayRequestForCtx(ctx)
	if err != nil || (snapshot != nil && len(snapshot.files) != 0) {
		return ctx, nil, noop, nil
	}
	control, err := s.resolveCheckoutControlScope(ctx, selector, lowered)
	if err != nil {
		return ctx, nil, noop, mcp.NewToolResultError(err.Error())
	}
	if control == nil || !control.CheckoutScoped {
		return ctx, nil, noop, nil
	}
	if _, err := s.familyPrimary(ctx, control.Checkout.FamilyID); err != nil {
		return ctx, nil, noop, mcp.NewToolResultError(err.Error())
	}
	mutation, err := s.lifecycle.BeginCheckoutByteMutation(ctx, control.Checkout.CheckoutID, control.Checkout.RootPath, control.Checkout.Incarnation)
	if errors.Is(err, indexer.ErrCheckoutByteAnchorUnavailable) {
		return ctx, nil, noop, nil
	}
	if err != nil {
		return ctx, nil, noop, mcp.NewToolResultError(fmt.Sprintf("%s: byte mutation admission refused; no files were changed: %v", graphview.CodeViewReadOnly, err))
	}
	root := control.Checkout.RootPath
	info, err := indexer.SourceRootFileInfo(root)
	if err != nil {
		mutation.Close()
		return ctx, nil, noop, mcp.NewToolResultError(err.Error())
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		mutation.Close()
		return ctx, nil, noop, mcp.NewToolResultError(err.Error())
	}
	rider := graphview.NewViewRider(selector)
	rider.MarkExact(control.Selector.String())
	rider.CheckoutID, rider.GraphID = control.Checkout.CheckoutID, control.GraphID
	view := &requestView{kind: requestViewKindWorktree, rider: rider, viewRoot: root, sourceScope: "file", sourceRootInfo: info, sourceResolvedRoot: resolved, sourceCheckoutIncarnation: control.Checkout.Incarnation, sourceRepoPrefix: control.RepoPrefix, sourceGraphPending: true, declared: graphview.Completeness{graphview.CapSourceSnapshot: graphview.StateComplete}}
	ctx = withRequestView(ctx, view)
	ctx = withCheckoutMutation(ctx, mutation, root)
	ctx = withCheckoutControl(ctx, control)
	if snapshot != nil {
		ctx = withOverlayRequestSnapshot(ctx, snapshot)
	}
	abs, _, err := s.resolveFilePath(ctx, lowered.GetString("path", ""))
	if err == nil {
		err = guardCheckoutMutationPath(ctx, abs)
	}
	if err != nil {
		mutation.Close()
		view.close()
		return ctx, nil, noop, mcp.NewToolResultError(err.Error())
	}
	return ctx, view, mutation.Close, nil
}
