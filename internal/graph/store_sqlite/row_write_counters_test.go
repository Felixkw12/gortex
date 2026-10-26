package store_sqlite

import (
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"testing"
)

func TestWriteMarkCountsLogicalBatchRowsAndTransactions(t *testing.T) {
	s, _ := openWALReclaimStore(t)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	// Warm the variable-limit probe and ensure the writer's TLS is registered.
	s.AddBatch([]*graph.Node{{ID: "warm", Kind: graph.KindFunction, Name: "warm"}}, nil)
	before := WriteMark()
	s.AddBatch([]*graph.Node{{ID: "one", Kind: graph.KindFunction, Name: "one"}, {ID: "two", Kind: graph.KindFunction, Name: "two"}, {ID: "three", Kind: graph.KindFunction, Name: "three"}}, []*graph.Edge{{From: "one", To: "two", Kind: graph.EdgeCalls}, {From: "one", To: "three", Kind: graph.EdgeCalls}})
	d := WriteMark().Since(before)
	require.Equal(t, int64(3), d.Rows.Nodes, "generation census triggers must not inflate direct row writes")
	require.Equal(t, int64(2), d.Rows.Edges)
	require.Equal(t, int64(5), d.Rows.Total())
	require.Equal(t, int64(1), d.Transactions)
	require.Positive(t, d.WriterWALWriteBytes)
	before = WriteMark()
	s.AddBatch([]*graph.Node{{ID: "one", Kind: graph.KindFunction, Name: "one"}}, []*graph.Edge{{From: "one", To: "two", Kind: graph.EdgeCalls}})
	require.Zero(t, WriteMark().Since(before).Rows.Total(), "idempotent statements do not change rows")
}

func TestWriteMarkReturningAndFTSRows(t *testing.T) {
	s, _ := openWALReclaimStore(t)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	s.writeMu.Lock()
	tx, err := s.beginWrite()
	require.NoError(t, err)
	before := WriteMark()
	nodes := []*graph.Node{{ID: "repo::a", Name: "a", Kind: graph.KindFunction, RepoPrefix: "repo"}, {ID: "repo::b", Name: "b", Kind: graph.KindFunction, RepoPrefix: "repo"}}
	changed, _, ids, err := insertNodeChunksTx(tx, 0, nodes, true)
	require.NoError(t, err)
	require.Equal(t, 2, changed)
	require.Len(t, ids, 2)
	inserted, _, keys, err := insertEdgeChunksTx(tx, 0, []*graph.Edge{{From: "repo::a", To: "repo::b", Kind: graph.EdgeCalls}}, true)
	require.NoError(t, err)
	require.Equal(t, 1, inserted)
	require.Len(t, keys, 1)
	require.NoError(t, tx.Commit())
	s.writeMu.Unlock()
	d := WriteMark().Since(before)
	require.Equal(t, int64(2), d.Rows.Nodes)
	require.Equal(t, int64(1), d.Rows.Edges)
	before = WriteMark()
	require.NoError(t, s.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "repo::a", Tokens: "alpha"}, {NodeID: "repo::b", Tokens: "beta"}}))
	require.Equal(t, int64(2), WriteMark().Since(before).Rows.SymbolFTS)
	before = WriteMark()
	require.NoError(t, s.BatchDeleteSymbolFTS([]string{"repo::a", "repo::b"}))
	require.Equal(t, int64(2), WriteMark().Since(before).Rows.SymbolFTS)
	before = WriteMark()
	require.NoError(t, s.AppendContent("repo", []graph.ContentFTSItem{{NodeID: "repo::doc", FilePath: "doc.md", Ordinal: 0, Body: "alpha"}, {NodeID: "repo::doc2", FilePath: "doc.md", Ordinal: 1, Body: "beta"}}))
	require.Equal(t, int64(2), WriteMark().Since(before).Rows.ContentFTS)
	before = WriteMark()
	require.NoError(t, s.ReplaceContentFiles("repo", []graph.ContentFTSFileReplacement{{FilePath: "doc.md"}}))
	require.Equal(t, int64(2), WriteMark().Since(before).Rows.ContentFTS)
}
