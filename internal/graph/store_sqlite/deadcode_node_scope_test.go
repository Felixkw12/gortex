package store_sqlite

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestDeadCodeNodeScopePreservesMetadataAndGlobalUsage(t *testing.T) {
	s, _ := openTempStore(t)
	nodes := []*graph.Node{
		{ID: "own", Kind: graph.KindFunction, RepoPrefix: "stale-owner", WorkspaceID: "ws"},
		{ID: "live", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws"},
		{ID: "foreign", Kind: graph.KindFunction, RepoPrefix: "foreign", WorkspaceID: "other"},
		{ID: "unowned", Kind: graph.KindFunction, WorkspaceID: "ws"},
		{ID: "external", Kind: graph.KindFunction, FilePath: "external::dependency"},
		{ID: "legacy", Kind: graph.KindFunction, RepoPrefix: "ws"},
		{ID: "ownfile", Kind: graph.KindFile, RepoPrefix: "stale-owner", WorkspaceID: "ws", Meta: map[string]any{"entry_point": true}},
		{ID: "foreignfile", Kind: graph.KindFile, RepoPrefix: "foreign", WorkspaceID: "other"},
		{ID: "type", Kind: graph.KindType, RepoPrefix: "stale-owner", WorkspaceID: "ws"},
		{ID: "iface", Kind: graph.KindInterface, RepoPrefix: "foreign", WorkspaceID: "other", Meta: map[string]any{"methods": []string{"Work"}}},
	}
	require.NoError(t, s.AddBatchChecked(nodes, []*graph.Edge{{From: "foreign", To: "live", Kind: graph.EdgeCalls}, {From: "type", To: "iface", Kind: graph.EdgeImplements}}))
	scope := graph.NodeScope{WorkspaceID: "ws"}
	kinds := []graph.NodeKind{graph.KindFunction}
	incoming := map[graph.NodeKind][]graph.EdgeKind{graph.KindFunction: {graph.EdgeCalls}}
	for _, prefixes := range [][]string{nil, {"stale-owner", "", "ws"}, {}} {
		got, err := s.DeadCodeCandidatesInScopeFiltered(kinds, incoming, prefixes, scope, graph.OutlineEdgeFilter{})
		require.NoError(t, err)
		var want []*graph.Node
		for _, n := range s.DeadCodeCandidatesInRepos(kinds, incoming, prefixes) {
			if scope.Allows(n) {
				want = append(want, n)
			}
		}
		require.Equal(t, want, got)
	}
	got, err := s.DeadCodeCandidatesInScopeFiltered(kinds, incoming, nil, scope, graph.OutlineEdgeFilter{})
	require.NoError(t, err)
	ids := make([]string, 0, len(got))
	for _, n := range got {
		ids = append(ids, n.ID)
	}
	require.ElementsMatch(t, []string{"own", "unowned", "external", "legacy"}, ids)
	require.Len(t, slices.Collect(s.NodesByKindInScope(graph.KindFile, nil, scope)), 1)
	rows, err := s.IfaceImplementsRowsInScopeFiltered(nil, scope, graph.OutlineEdgeFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "iface", rows[0].IfaceID)
}

// This benchmark exposes residual node-scan growth separately from expensive
// usage probes. It makes no claim that workspace predicates have a new index.
func BenchmarkDeadCodeWorkspaceForeignGrowth(b *testing.B) {
	for _, foreign := range []int{0, 10000} {
		b.Run(fmt.Sprint(foreign), func(b *testing.B) {
			s, err := Open(b.TempDir() + "/scope.db")
			require.NoError(b, err)
			b.Cleanup(func() { _ = s.Close() })
			nodes := []*graph.Node{{ID: "own", Kind: graph.KindFunction, WorkspaceID: "ws"}}
			for i := range foreign {
				nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("foreign/%08d", i), Kind: graph.KindMethod, WorkspaceID: "other"})
			}
			require.NoError(b, s.AddBatchChecked(nodes, nil))
			b.ResetTimer()
			for range b.N {
				got, err := s.DeadCodeCandidatesInScopeFiltered([]graph.NodeKind{graph.KindFunction, graph.KindMethod}, nil, nil, graph.NodeScope{WorkspaceID: "ws"}, graph.OutlineEdgeFilter{})
				if err != nil || len(got) != 1 {
					b.Fatal(len(got), err)
				}
			}
		})
	}
}
