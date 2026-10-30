package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func seedIndexMigrationAnalysis(t *testing.T, version int) (string, graph.AnalysisGenerationHeader) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index-upgrade.db")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "kept-node", Kind: graph.KindFunction, Name: "kept", RepoPrefix: "repo", Meta: map[string]any{"doc": "persisted"}}}, nil))
	buildMinimalAnalysisGeneration(t, s, "kept", 2, true)
	header, found, err := s.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, s.Close())
	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`DROP INDEX nodes_by_effective_workspace`)
		require.NoError(t, err)
		require.NoError(t, setUserVersion(db, version))
	})
	return path, header
}

func requireKeptIndexMigrationAnalysis(t *testing.T, s *Store, want graph.AnalysisGenerationHeader) {
	t.Helper()
	header, found, err := s.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	require.True(t, found, "index-only upgrade must retain active analysis")
	require.Equal(t, want, header, "persisted revision/header must not be reset by an index-only upgrade")
	blob, found, err := s.LoadAnalysisBlob(header.GenerationID, graph.AnalysisBlobAdjacency)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, []byte("adjacency-kept"), blob)
	nodes, err := s.AnalysisNodeMetrics(header.GenerationID, []string{"kept-node"})
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Equal(t, "kept-community", nodes[0].CommunityID)
	require.Equal(t, "persisted", s.GetNode("kept-node").Meta["doc"])
	version, err := readUserVersion(s.db)
	require.NoError(t, err)
	require.Equal(t, 41, version)
	require.True(t, indexNames(t, s.db)["nodes_by_effective_workspace"])
}

func TestIndexOnlyMigrationPreservesActiveAnalysis(t *testing.T) {
	path, header := seedIndexMigrationAnalysis(t, 40)
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	requireKeptIndexMigrationAnalysis(t, s, header)
}
