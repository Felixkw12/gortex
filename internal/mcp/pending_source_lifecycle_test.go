package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

// Test-only accessor: read version counts under the same lock order as the
// registry rather than exposing mutable maps to concurrent lifecycle tests.
func pendingSourceVersionCount(s *Server, checkoutID string) int {
	p := &s.pendingSourceFiles
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, record := range p.versions {
		record.mu.RLock()
		if record.checkoutID == checkoutID {
			count++
		}
		record.mu.RUnlock()
	}
	return count
}

func TestPendingSourceRegistryDropsDeadCheckoutIncarnation(t *testing.T) {
	for _, scenario := range []string{"removed", "reincarnated", "grace", "catalog error"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			store, err := store_sqlite.Open(filepath.Join(root, "store.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			catalog := store.Catalog()
			ctx := context.Background()
			require.NoError(t, catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
				FamilyID: "family", CommonDirIdentity: filepath.Join(root, ".git"), State: reconcile.FamilyStateReady,
			}))
			checkout := store_sqlite.Checkout{
				CheckoutID: "checkout", Incarnation: "old", FamilyID: "family", RootPath: root,
				GitDir: filepath.Join(root, ".git"), AdminName: "primary", State: store_sqlite.CheckoutStateReady,
				DesiredMode: store_sqlite.CheckoutModeDedicated, EffectiveMode: store_sqlite.CheckoutModeDedicated,
			}
			require.NoError(t, catalog.UpsertCheckout(ctx, checkout))
			s := &Server{graph: store}
			for i := range 8 {
				record := checkoutSourceVersion(s, "checkout", "old", filepath.Join(root, fmt.Sprintf("file%d.go", i)))
				record.markCommitted("bytes", 5)
				record.recordGraph(checkoutOutcome(record, mutationReindexOutcome{Err: context.DeadlineExceeded}))
			}
			require.Equal(t, 8, pendingSourceVersionCount(s, "checkout"))
			switch scenario {
			case "removed":
				require.NoError(t, catalog.DeleteCheckout(ctx, "checkout"))
			case "reincarnated":
				checkout.Incarnation = "new"
				require.NoError(t, catalog.UpsertCheckout(ctx, checkout))
				current := checkoutSourceVersion(s, "checkout", "new", filepath.Join(root, "current.go"))
				current.markCommitted("new bytes", 9)
				current.recordGraph(checkoutOutcome(current, mutationReindexOutcome{Err: context.DeadlineExceeded}))
			case "grace":
				checkout.State = store_sqlite.CheckoutStateRemovalGrace
				require.NoError(t, catalog.UpsertCheckout(ctx, checkout))
			case "catalog error":
				require.NoError(t, store.Close())
			}
			s.InvalidateSessionScopes()
			switch scenario {
			case "removed":
				require.Zero(t, pendingSourceVersionCount(s, "checkout"), "lifecycle notification retires versions immediately")
			case "reincarnated":
				require.Equal(t, 1, pendingSourceVersionCount(s, "checkout"), "the new incarnation's failed version remains pending")
				require.NoError(t, catalog.DeleteCheckout(ctx, "checkout"))
				require.Empty(t, s.pendingSourcePaths("checkout", "new"), "source reads consume authoritative retirement too")
				require.Zero(t, pendingSourceVersionCount(s, "checkout"))
			default:
				require.Equal(t, 8, pendingSourceVersionCount(s, "checkout"), "grace/errors are not evidence of death")
			}
		})
	}
}

func TestPendingSourceRegistryCanonicalTerminalRetirement(t *testing.T) {
	for name, settle := range map[string]func(*mutationCommitRecord){
		"fresh":       func(r *mutationCommitRecord) { r.recordGraph(mutationReindexOutcome{Reindexed: true}) },
		"failed":      func(r *mutationCommitRecord) { r.recordGraph(mutationReindexOutcome{Err: context.DeadlineExceeded}) },
		"not applied": func(r *mutationCommitRecord) { r.markNotApplied(context.Canceled) },
		"disk failed": func(r *mutationCommitRecord) { r.markFailed(context.DeadlineExceeded) },
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{}
			record := s.beginMutationCommit(context.Background(), "edit", "", "", "file.go", "/fixture/file.go")
			require.Equal(t, 1, pendingSourceVersionCount(s, ""))
			record.markCommitted("bytes", 5)
			settle(record)
			require.Zero(t, pendingSourceVersionCount(s, ""), "retirement must not wait for a source read or another completion")
		})
	}
}

func TestPendingSourceRegistryCanonicalFreshRetainsCommitOrderEvidence(t *testing.T) {
	s := &Server{}
	old := s.beginMutationCommit(context.Background(), "edit", "", "", "file.go", "/fixture/file.go")
	old.markCommitted("old", 3)
	newer := s.beginMutationCommit(context.Background(), "edit", "", "", "file.go", "/fixture/file.go")
	newer.markCommitted("new", 3)
	newer.recordGraph(mutationReindexOutcome{Reindexed: true})
	require.Zero(t, pendingSourceVersionCount(s, ""))
	require.Equal(t, mutationGraphStale, old.snapshot().GraphStatus, "retirement does not rewrite historical publication evidence")
	old.recordGraph(mutationReindexOutcome{Err: context.DeadlineExceeded})
	require.Zero(t, pendingSourceVersionCount(s, ""), "a late failure cannot resurrect a retired version")
}
