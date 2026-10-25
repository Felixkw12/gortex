package storetest

import (
	"reflect"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// RunScopedOutline compares a backend's scoped aggregates against the graph
// reference, including ownership fallback, false allow entries and externals.
func RunScopedOutline(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	ag, ok := s.(graph.ScopedOutlineAggregator)
	if !ok {
		t.Skip("backend does not implement graph.ScopedOutlineAggregator")
	}
	nodes := []*graph.Node{
		{ID: "a/main", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "a/main.go"},
		{ID: "a/lib", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "a/lib.go"},
		{ID: "b/main", RepoPrefix: "b", WorkspaceID: "ws", ProjectID: "q", Language: "rust", FilePath: "b/main.rs"},
		{ID: "c/main", RepoPrefix: "c", WorkspaceID: "other", ProjectID: "p", Language: "python", FilePath: "c/main.py"},
		{ID: "legacy/main", RepoPrefix: "legacy", Language: "go", FilePath: "legacy/main.go"},
		{ID: "unowned", WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "source.go"},
		{ID: "global", Language: "go", FilePath: "external::go:fmt"},
		{ID: "global-call", ProjectID: "ignored", FilePath: "external-call::symbol"},
		{ID: "empty-path"},
		{ID: "case-sensitive", FilePath: "External::source"},
		{ID: "external-with-workspace", WorkspaceID: "other", FilePath: "external::not-global"},
		{ID: "source-unowned", FilePath: "source.go"},
		{ID: "module-source", FilePath: "module::source"},
		{ID: "no-file", RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "go"},
	}
	edges := []*graph.Edge{
		{From: "a/main", To: "a/lib", Kind: graph.EdgeImports},
		{From: "b/main", To: "a/lib", Kind: graph.EdgeImports}, // target-only scope
		{From: "c/main", To: "a/lib", Kind: graph.EdgeImports},
		{From: "a/main", To: "global", Kind: graph.EdgeCalls},
		{From: "b/main", To: "global", Kind: graph.EdgeImports},
		{From: "a/main", To: "unowned", Kind: graph.EdgeImports},
		{From: "a/main", To: "b/main", Kind: graph.EdgeCalls},
		{From: "a/main", To: "no-file", Kind: graph.EdgeImports},
	}
	s.AddBatch(nodes, edges)
	ref := graph.New()
	ref.AddBatch(nodes, edges)
	for name, scope := range map[string]graph.NodeScope{
		"unbound":           {},
		"repo":              {RepoAllow: map[string]bool{"a": true}},
		"workspace":         {WorkspaceID: "ws", RepoAllow: map[string]bool{"a": true, "b": true}},
		"workspace-only":    {WorkspaceID: "ws"},
		"project":           {WorkspaceID: "ws", ProjectID: "p", RepoAllow: map[string]bool{"a": true, "b": true}},
		"project-with-repo": {ProjectID: "p", RepoAllow: map[string]bool{"a": true}},
		"project-alone":     {ProjectID: "p"},
		"fallback":          {WorkspaceID: "legacy", ProjectID: "legacy", RepoAllow: map[string]bool{"legacy": true}},
		"unknown":           {WorkspaceID: "unknown", RepoAllow: map[string]bool{"unknown": true}},
		"false-repo":        {RepoAllow: map[string]bool{"a": false}},
		"mixed-allow":       {RepoAllow: map[string]bool{"a": true, "b": false}},
	} {
		t.Run(name, func(t *testing.T) {
			got, want := ag.ScopedOutlineCounts(scope), ref.ScopedOutlineCounts(scope)
			sort.Slice(got.FileImports, func(i, j int) bool { return got.FileImports[i].FilePath < got.FileImports[j].FilePath })
			sort.Slice(want.FileImports, func(i, j int) bool { return want.FileImports[i].FilePath < want.FileImports[j].FilePath })
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("scoped outline = %+v, want %+v", got, want)
			}
			if name == "project" && (got.TotalNodes != 6 || got.TotalEdges != 4) {
				t.Fatalf("project counts = %+v, want 6 nodes, 4 edges", got)
			}
			if name == "project" {
				for _, row := range got.FileImports {
					if row.FilePath == "a/lib.go" && row.Count != 3 {
						t.Fatalf("imports must count all sources: %+v", row)
					}
				}
			}
		})
	}
}
