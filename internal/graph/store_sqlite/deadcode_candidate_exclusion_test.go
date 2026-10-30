package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestDeadCodeCandidateExclusionsPreserveRawScopeAndLegacyEvidence(t *testing.T) {
	s, _ := openTempStore(t)
	ids := []string{"own", "live", "promoted", "legacy", "stdlib::x", "repo::module::x", "repo::MODULE::x", "repo::nested::module::x", "global"}
	var nodes []*graph.Node
	for _, id := range ids {
		n := &graph.Node{ID: id, Kind: graph.KindFunction, WorkspaceID: "ws"}
		if id == "promoted" || id == "legacy" {
			n.Meta = map[string]any{"external": true}
		}
		if id == "global" {
			n.WorkspaceID = ""
			n.FilePath = "external::shared"
		}
		nodes = append(nodes, n)
	}
	require.NoError(t, s.AddBatchChecked(nodes, []*graph.Edge{{From: "promoted", To: "live", Kind: graph.EdgeCalls}}))
	// A legacy blob-only marker cannot be rejected on promoted column proof.
	blob, err := encodeMeta(map[string]any{"external": true})
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`UPDATE nodes SET external=NULL, meta=? WHERE id='legacy'`, blob)
	require.NoError(t, err)
	query := func(exclude graph.DeadCodeCandidateExclusions, filter graph.OutlineEdgeFilter) ([]string, []*graph.Node) {
		got, queryErr := s.DeadCodeCandidatesInScopeFiltered([]graph.NodeKind{graph.KindFunction}, map[graph.NodeKind][]graph.EdgeKind{graph.KindFunction: {graph.EdgeCalls}}, nil, graph.NodeScope{WorkspaceID: "ws"}, filter, exclude)
		require.NoError(t, queryErr)
		var out []string
		for _, n := range got {
			out = append(out, n.ID)
		}
		return out, got
	}
	raw, _ := query(graph.DeadCodeCandidateExclusions{}, graph.OutlineEdgeFilter{})
	require.ElementsMatch(t, []string{"own", "promoted", "legacy", "stdlib::x", "repo::module::x", "repo::MODULE::x", "repo::nested::module::x", "global"}, raw)
	exclude := graph.DeadCodeCandidateExclusions{Stubs: true, External: true}
	eligible, got := query(exclude, graph.OutlineEdgeFilter{})
	require.ElementsMatch(t, []string{"own", "legacy", "repo::MODULE::x", "repo::nested::module::x", "global"}, eligible)
	for _, n := range got {
		if n.ID == "legacy" {
			require.Equal(t, true, n.Meta["external"])
			require.False(t, exclude.Allows(n), "decoded final eligibility still rejects legacy blob-only markers")
		}
	}
	// The candidate-only exclusion must not remove the flagged caller's
	// incoming evidence. An explicit adjacency mask can remove that evidence.
	masked, _ := query(exclude, graph.OutlineEdgeFilter{ExcludeNodeIDs: map[string]bool{"promoted": true}})
	require.Contains(t, masked, "live")
}
