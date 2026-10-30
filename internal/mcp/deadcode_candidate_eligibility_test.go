package mcp

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type eligibilityCandidateSpy struct {
	*store_sqlite.Store
	rows *int
}

func (r *eligibilityCandidateSpy) BindReadContext(ctx context.Context) graph.Reader {
	return &eligibilityCandidateSpy{Store: r.WithReadContext(ctx), rows: r.rows}
}
func (r *eligibilityCandidateSpy) DeadCodeCandidatesInScopeFiltered(k []graph.NodeKind, in map[graph.NodeKind][]graph.EdgeKind, repos []string, scope graph.NodeScope, filter graph.OutlineEdgeFilter) ([]*graph.Node, error) {
	nodes, err := r.Store.DeadCodeCandidatesInScopeFiltered(k, in, repos, scope, filter)
	*r.rows += len(nodes)
	return nodes, err
}
func TestDeadCodeKnownIneligibleGlobalsDoNotGrowCandidateHydration(t *testing.T) {
	s, store := outlineFixture(t)
	store.AddBatch([]*graph.Node{
		{ID: "own-private", Kind: graph.KindFunction, Name: "_unused_own", Language: "python", FilePath: "own/f.py", RepoPrefix: "own", WorkspaceID: "ws"},
		{ID: "own-live", Kind: graph.KindFunction, Name: "_live_own", Language: "python", FilePath: "own/f.py", RepoPrefix: "own", WorkspaceID: "ws"},
		{ID: "own::module::stub", Kind: graph.KindFunction, Name: "stub", Language: "python", FilePath: "own/f.py", RepoPrefix: "own", WorkspaceID: "ws"},
		{ID: "own::MODULE::source", Kind: graph.KindFunction, Name: "_case_sensitive", Language: "python", FilePath: "own/f.py", RepoPrefix: "own", WorkspaceID: "ws"},
		{ID: "global-unmarked", Kind: graph.KindFunction, Name: "_global_own", Language: "python", FilePath: "external::shared"},
		{ID: "ext::caller", Kind: graph.KindFunction, Name: "caller", Language: "python", FilePath: "external::caller", Meta: map[string]any{"external": true}},
	}, []*graph.Edge{{From: "ext::caller", To: "own-live", Kind: graph.EdgeCalls}})
	run := func() (string, int) {
		rows := 0
		ctx := outlineBoundContext(s, "eligibility", "ws", nil)
		reader := newContractCoreEdges(&eligibilityCandidateSpy{Store: store, rows: &rows}, ctx, nil)
		ctx = withRequestView(ctx, &requestView{reader: reader})
		result, err := s.handleFindDeadCode(ctx, makeReq("find_dead_code", map[string]any{"format": "json"}))
		require.NoError(t, err)
		body := toolResultText(result)
		require.Contains(t, body, "own-private")
		require.Contains(t, body, "own::MODULE::source", "identity checks are case sensitive")
		require.Contains(t, body, "global-unmarked", "global visibility is not an ownership-based exclusion")
		require.NotContains(t, body, "own-live", "a flagged foreign caller remains valid usage evidence")
		return body, rows
	}
	before, rowsBefore := run()
	nodes := make([]*graph.Node, 1000)
	for i := range nodes {
		nodes[i] = &graph.Node{ID: fmt.Sprintf("ext::dependency::%04d", i), Kind: graph.KindFunction, Name: "dependency", FilePath: "external::dependency", Meta: map[string]any{"external": true}}
	}
	store.AddBatch(nodes, nil)
	after, rowsAfter := run()
	require.JSONEq(t, before, after)
	require.Equal(t, rowsBefore, rowsAfter, "already rejected globals must not reach candidate hydration/usage work")
}
