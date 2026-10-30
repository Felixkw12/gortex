package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestDeadCodeProvenWorkspaceAvoidsPrimaryNodeSeek(t *testing.T) {
	s, _ := openTempStore(t)
	// The effective-workspace seek itself proves this nonempty workspace.
	// EXPLAIN's COVERING label alone is insufficient for expression indexes:
	// inspect executable bytecode for the residual primary-key verification.
	q, args := deadCodeNodeScopeVerdictPageSQL(0, false, true, nil, graph.OutlineEdgeFilter{}, graph.DeadCodeCandidateExclusions{External: true})
	values := append([]any{int64(0), "ws"}, append(args, "", "function", 512)...)
	rows, err := s.db.Query("EXPLAIN "+q, values...)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var addr, p1, p2, p3, p5 int
		var opcode string
		var p4, comment any
		require.NoError(t, rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment))
		require.NotEqual(t, "NotFound", opcode, "candidate-only workspace key still visits primary nodes")
	}
	require.NoError(t, rows.Err())
}

func TestDeadCodeSourceMaskUsesCoveringKindIdentity(t *testing.T) {
	s, _ := openTempStore(t)
	filter := graph.OutlineEdgeFilter{ExcludeNodeKinds: []graph.NodeKind{graph.KindContract, graph.KindContract}}
	q, args := deadCodeNodeScopeVerdictPageSQL(0, false, false, nil, filter)
	plan := generationLookupPlan(t, s.db, q, append(args, int64(0), "", "function", 512)...)
	require.Contains(t, plan, "COVERING INDEX nodes_by_kind")
	require.NotContains(t, plan, "SEARCH source USING PRIMARY KEY")
}
