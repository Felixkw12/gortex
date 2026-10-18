package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestFoldBaseAdjacencyMemoKeepsRenderedRowsAndSelectedMasks(t *testing.T) {
	base := graph.New()
	for _, id := range []string{"source.go::Caller", "target.go::Old", "target.go::Kept"} {
		base.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, FilePath: graph.IDFile(id), Meta: map[string]any{"full": "node"}})
	}
	base.AddEdge(&graph.Edge{From: "source.go::Caller", To: "target.go::Old", Kind: graph.EdgeCalls, FilePath: "foreign.go", Line: 8, Confidence: .37, Origin: "original", Meta: map[string]any{"large": strings.Repeat("proof", 100)}})
	base.AddEdge(&graph.Edge{From: "source.go::Caller", To: "target.go::Kept", Kind: graph.EdgeReferences, FilePath: "foreign.go", Line: 9, Confidence: .91, Origin: "foreign", Meta: map[string]any{"via": "kept"}})
	layer := graph.NewOverlayLayer()
	layer.MarkFile("target.go", false)
	layer.AddNode("target.go", &graph.Node{ID: "target.go::Kept", Kind: graph.KindFunction, FilePath: "target.go"})
	layer.AddEdge(&graph.Edge{From: "target.go::Kept", To: "source.go::Caller", Kind: graph.EdgeCalls, FilePath: "target.go", Line: 4, Confidence: .8, Origin: "own", Meta: map[string]any{"new": "full"}})
	paths := []string{"source.go", "target.go", "foreign.go"}
	ids := []string{"missing.go::Absent"}
	expected, err := renderFoldChunk(t.Context(), graph.NewOverlaidViewWithLayer(base, layer), paths, ids, 0, len(paths)+len(ids), map[string]struct{}{})
	require.NoError(t, err)
	probe := &foldQuantumProbe{Reader: base}
	memo := newFoldBaseAdjacency(commitLayerBase{Reader: probe, stack: []int64{1}})
	require.NotNil(t, memo)
	for range 2 {
		got, err := renderFoldChunk(t.Context(), graph.NewOverlaidViewWithLayer(memo, layer), paths, ids, 0, len(paths)+len(ids), map[string]struct{}{})
		require.NoError(t, err)
		require.Equal(t, expected, got, "all original render fields, recorded rows, tombstones and foreign adjacency remain exact")
	}
	require.Len(t, probe.out, 1, "both sides share the same immutable base outgoing projection")
	require.Len(t, probe.in, 1, "both sides share the same immutable base incoming projection")
	require.Positive(t, memo.used)
	require.LessOrEqual(t, memo.used, memo.budget)
	memo.reset()
	require.Zero(t, memo.used)
	require.Empty(t, memo.outgoing)
	require.Empty(t, memo.incoming)
	_, err = renderFoldChunk(t.Context(), graph.NewOverlaidViewWithLayer(memo, layer), paths, ids, 0, len(paths)+len(ids), map[string]struct{}{})
	require.NoError(t, err)
	require.Len(t, probe.out, 2, "next chunk obtains fresh base rows")
	require.Len(t, probe.in, 2)
}

func TestFoldBaseAdjacencyMemoBoundsWithoutChangingFallbackOrCancellation(t *testing.T) {
	base := graph.New()
	base.AddEdge(&graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "file", Line: 3, Confidence: .4, Origin: "origin", Meta: map[string]any{"full": "retained on first read"}})
	probe := &foldQuantumProbe{Reader: base}
	memo := newFoldBaseAdjacency(commitLayerBase{Reader: probe, stack: []int64{1, 2}})
	require.NotNil(t, memo)
	memo.budget = 1
	for range 2 {
		require.Equal(t, base.GetOutEdgesByNodeIDs([]string{"a"}), memo.GetOutEdgesByNodeIDs([]string{"a"}))
	}
	require.Len(t, probe.out, 2, "over-budget answer uses original reads without truncation")
	require.Zero(t, memo.used)
	require.Empty(t, memo.outgoing)
	memo.budget = foldBaseAdjacencyBytes
	_ = memo.GetOutEdgesByNodeIDs([]string{"missing"})
	_ = memo.GetOutEdgesByNodeIDs([]string{"missing"})
	require.Len(t, probe.out, 3, "one genuine immutable absence can be reused")
	memo.reset()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := renderFoldChunk(ctx, memo, nil, []string{"a"}, 0, 1, map[string]struct{}{})
	require.ErrorIs(t, err, context.Canceled)
	// A refusal from the unchanged backend remains authoritative.
	probe.cancelAt = "out"
	ctx, cancel = context.WithCancel(t.Context())
	probe.cancel = cancel
	_, err = renderFoldChunk(ctx, memo, nil, []string{"a"}, 0, 1, map[string]struct{}{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestFoldBaseAdjacencyMemoRequiresImmutablePositiveAncestry(t *testing.T) {
	base := graph.New()
	require.Nil(t, newFoldBaseAdjacency(base), "unknown reader has no immutable leased ancestry proof")
	require.Nil(t, newFoldBaseAdjacency(commitLayerBase{Reader: base}))
	require.Nil(t, newFoldBaseAdjacency(commitLayerBase{Reader: base, stack: []int64{0, 1}}))
	require.Nil(t, newFoldBaseAdjacency(commitLayerBase{Reader: base, stack: []int64{-1}}))
}
