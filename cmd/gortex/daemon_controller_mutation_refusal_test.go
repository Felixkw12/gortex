package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
)

func TestFileCoverageReadOnlyIsCheckoutScopedAndFreshRouteClears(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)
	route, found, err := f.catalog.GetCheckoutRoute(t.Context(), probeWorktreeID)
	if err != nil || !found {
		t.Fatalf("route: %v/%v", found, err)
	}
	advance := func() {
		t.Helper()
		if err := f.catalog.FlipCheckoutRoute(t.Context(), store_sqlite.FlipCheckoutRouteRequest{
			CheckoutID: route.CheckoutID, ExpectedRouteEpoch: route.RouteEpoch,
			GraphID: route.GraphID, CommitGenerationID: route.CommitGenerationID,
			DirtyGenerationID: route.DirtyGenerationID, State: store_sqlite.RouteActive,
		}); err != nil {
			t.Fatal(err)
		}
		route.RouteEpoch++
	}
	advance()
	// This controller fixture deliberately has a catalog route with no live
	// checkout directory. Exercise real stale admission, not a fabricated error
	// string; the persisted composed reader still reports indexed symbols.
	lease, refusal := f.controller.lifecycle.BeginCheckoutMutation(t.Context(), probeWorktreeID, f.worktreeRoot, route.RouteEpoch)
	if lease != nil {
		lease.Close()
	}
	if !errors.Is(refusal, indexer.ErrCheckoutMutationStale) {
		t.Fatalf("admission=%v", refusal)
	}
	f.controller.lifecycle.RecordCheckoutMutationRefusal(t.Context(), refusal)
	probe := func(root string) daemon.FileCoverageResult {
		t.Helper()
		coverage, err := f.controller.FileCoverage(t.Context(), daemon.FileCoverageParams{Path: filepath.Join(root, probeFile)})
		if err != nil {
			t.Fatal(err)
		}
		return coverage
	}
	readOnly := probe(f.worktreeRoot)
	if !readOnly.Covered || readOnly.MutationState != "read_only" || readOnly.MutationReason == "" {
		t.Fatalf("persisted coverage must also report mutation refusal: %+v", readOnly)
	}
	if primary := probe(f.primaryRoot); primary.MutationState != "" || primary.MutationReason != "" {
		t.Fatalf("worktree refusal affected primary: %+v", primary)
	}
	advance()
	f.controller.lifecycle.RecordCheckoutMutationRefusal(t.Context(), refusal)
	if fresh := probe(f.worktreeRoot); !fresh.Covered || fresh.MutationState != "" || fresh.MutationReason != "" {
		t.Fatalf("late refusal survived fresh route: %+v", fresh)
	}
	f.controller.lifecycle.RecordCheckoutMutationRefusal(t.Context(), indexer.ErrCheckoutMutationBusy)
	if busy := probe(f.worktreeRoot); busy.MutationState != "" {
		t.Fatal("Busy became read_only")
	}
}
