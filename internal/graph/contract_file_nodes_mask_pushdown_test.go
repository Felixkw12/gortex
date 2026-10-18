package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type contractFileNodeRequestReader struct {
	Reader
	requests [][]string
	err      error
}

func (r *contractFileNodeRequestReader) LayerContractFileNodesContext(ctx context.Context, _ string, paths []string) (map[string][]*Node, error) {
	r.requests = append(r.requests, append([]string{}, paths...))
	if r.err != nil {
		return nil, r.err
	}
	return memoryContractFileNodes(ctx, paths, r.GetFileNodes)
}

func TestContractFileNodesPrunesOnlyWholeFileMasksBeforeBaseRead(t *testing.T) {
	base := New()
	old := &Node{ID: "a.go::F", Kind: KindFunction, FilePath: "a.go", Meta: map[string]any{"signature": "old"}}
	gone := &Node{ID: "gone.go::F", Kind: KindFunction, FilePath: "gone.go"}
	kept := &Node{ID: "keep.go::F", Name: "F", Kind: KindFunction, FilePath: "keep.go", Meta: map[string]any{"nested": map[string]any{"full": "kept"}}}
	base.AddBatch([]*Node{old, gone, kept}, nil)
	checked := &contractFileNodeRequestReader{Reader: base}
	layer := NewOverlayLayer()
	current := *old
	current.Meta = map[string]any{"signature": "current"}
	layer.MarkFile("a.go", false)
	layer.AddNode("a.go", &current)
	layer.MarkFile("gone.go", true)
	view := NewOverlaidViewWithLayer(checked, layer)
	rows, err := ContractFileNodesContext(t.Context(), view, "", []string{"a.go", "gone.go", "keep.go"})
	require.NoError(t, err)
	require.Equal(t, []*Node{&current}, rows["a.go"])
	require.Empty(t, rows["gone.go"])
	require.Equal(t, []*Node{kept}, rows["keep.go"])
	require.Equal(t, [][]string{{"keep.go"}}, checked.requests, "hidden historical files must not be hydrated then discarded")
	// Identity-only removal does not claim a whole file and must keep the base
	// read: other nodes in that same file remain visible.
	checked.requests = nil
	removed := NewOverlayLayer()
	removed.MarkRemoved(kept.Name, kept.ID)
	rows, err = ContractFileNodesContext(t.Context(), NewOverlaidViewWithLayer(checked, removed), "", []string{"keep.go"})
	require.NoError(t, err)
	require.Empty(t, rows["keep.go"])
	require.Equal(t, [][]string{{"keep.go"}}, checked.requests)
}

func TestContractFileNodesMaskedBaseStillChecksEmptyRead(t *testing.T) {
	checked := &contractFileNodeRequestReader{Reader: New()}
	layer := NewOverlayLayer()
	layer.MarkFile("a.go", true)
	view := NewOverlaidViewWithLayer(checked, layer)
	rows, err := ContractFileNodesContext(t.Context(), view, "", []string{"a.go"})
	require.NoError(t, err)
	require.Empty(t, rows["a.go"])
	require.Equal(t, [][]string{{}}, checked.requests, "a fully masked base still checks connection and revision lifetime")
	cause := errors.New("base checked read failed")
	checked.err = cause
	rows, err = ContractFileNodesContext(t.Context(), view, "", []string{"a.go"})
	require.ErrorIs(t, err, cause)
	require.Nil(t, rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := len(checked.requests)
	rows, err = ContractFileNodesContext(ctx, view, "", []string{"a.go"})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	require.Len(t, checked.requests, before)
}
