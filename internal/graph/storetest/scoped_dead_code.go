package storetest

import (
	"fmt"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func testScopedDeadCodeReader(t *testing.T, factory Factory) {
	s := factory(t)
	reader, ok := s.(graph.ScopedDeadCodeReader)
	if !ok {
		t.Skip("backend does not implement graph.ScopedDeadCodeReader")
	}
	for _, node := range []*graph.Node{
		{ID: "a-dead", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a-live", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a-weak", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a-missing-caller", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "a-type", Kind: graph.KindType, RepoPrefix: "a"},
		{ID: "a-iface", Kind: graph.KindInterface, RepoPrefix: "a", Meta: map[string]any{"methods": []string{"Work"}}},
		{ID: "a-file", Kind: graph.KindFile, RepoPrefix: "a"},
		{ID: "b-caller", Kind: graph.KindFunction, RepoPrefix: "b"},
		{ID: "b-type", Kind: graph.KindType, RepoPrefix: "b"},
		{ID: "b-iface", Kind: graph.KindInterface, RepoPrefix: "b", Meta: map[string]any{"methods": []string{"Work"}}},
		{ID: "b-file", Kind: graph.KindFile, RepoPrefix: "b"},
		{ID: "unowned-dead", Kind: graph.KindFunction},
		{ID: "unowned-file", Kind: graph.KindFile},
	} {
		s.AddNode(node)
	}
	for _, edge := range []*graph.Edge{
		{From: "b-caller", To: "a-live", Kind: graph.EdgeCalls},
		{From: "b-caller", To: "a-weak", Kind: graph.EdgeReads},
		{From: "missing-caller", To: "a-missing-caller", Kind: graph.EdgeCalls},
		{From: "a-type", To: "b-iface", Kind: graph.EdgeImplements},
		{From: "b-type", To: "a-iface", Kind: graph.EdgeImplements},
		{From: "missing-type", To: "a-iface", Kind: graph.EdgeImplements},
	} {
		s.AddEdge(edge)
	}
	kinds := []graph.NodeKind{graph.KindFunction}
	incoming := map[graph.NodeKind][]graph.EdgeKind{graph.KindFunction: {graph.EdgeCalls}}
	for _, tc := range []struct {
		name       string
		prefixes   []string
		candidates []string
		files      []string
		implements []string
	}{
		{"one repo", []string{"a"}, []string{"a-dead", "a-weak"}, []string{"a-file"}, []string{"a-type:b-iface"}},
		{"other repo", []string{"b"}, []string{"b-caller"}, []string{"b-file"}, []string{"b-type:a-iface"}},
		{"union deduplicates prefixes", []string{"b", "a", "a"}, []string{"a-dead", "a-weak", "b-caller"}, []string{"a-file", "b-file"}, []string{"a-type:b-iface", "b-type:a-iface"}},
		{"empty matches nothing", []string{}, nil, nil, nil},
		{"unowned is explicit", []string{""}, []string{"unowned-dead"}, []string{"unowned-file"}, nil},
		{"nil preserves unscoped", nil, []string{"a-dead", "a-weak", "b-caller", "unowned-dead"}, []string{"a-file", "b-file", "unowned-file"}, []string{"a-type:b-iface", "b-type:a-iface", "missing-type:a-iface"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sortNodeIDs(reader.DeadCodeCandidatesInRepos(kinds, incoming, tc.prefixes)); !slices.Equal(got, tc.candidates) {
				t.Fatalf("candidates = %v, want %v (cross-repo/dangling incoming usage must survive)", got, tc.candidates)
			}
			if got := sortNodeIDs(slices.Collect(reader.NodesByKindInRepos(graph.KindFile, tc.prefixes))); !slices.Equal(got, tc.files) {
				t.Fatalf("files = %v, want %v", got, tc.files)
			}
			var rows []string
			for _, row := range reader.IfaceImplementsRowsInRepos(tc.prefixes) {
				rows = append(rows, row.TypeID+":"+row.IfaceID)
				if fmt.Sprint(row.IfaceMeta["methods"]) != "[Work]" {
					t.Fatalf("interface metadata lost: %#v", row)
				}
			}
			slices.Sort(rows)
			if !slices.Equal(rows, tc.implements) {
				t.Fatalf("implements = %v, want %v (interface targets may be outside scope)", rows, tc.implements)
			}
		})
	}
}
