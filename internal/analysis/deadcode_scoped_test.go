package analysis

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type deadCodeReadCounts struct{ candidates, implements, files, queries, allNodes int }
type countedDeadCodeReader struct {
	graph.Reader
	scoped graph.ScopedDeadCodeReader
	counts *deadCodeReadCounts
}

func (r *countedDeadCodeReader) AllNodes() []*graph.Node {
	r.counts.allNodes++
	return r.Reader.AllNodes()
}
func (r *countedDeadCodeReader) BindReadContext(ctx context.Context) graph.Reader {
	bound := graph.BindReadContext(r.Reader, ctx)
	return &countedDeadCodeReader{Reader: bound, scoped: bound.(graph.ScopedDeadCodeReader), counts: r.counts}
}
func (r *countedDeadCodeReader) DeadCodeCandidatesInRepos(k []graph.NodeKind, in map[graph.NodeKind][]graph.EdgeKind, repos []string) []*graph.Node {
	rows := r.scoped.DeadCodeCandidatesInRepos(k, in, repos)
	r.counts.queries++
	r.counts.candidates += len(rows)
	return rows
}
func (r *countedDeadCodeReader) IfaceImplementsRowsInRepos(repos []string) []graph.IfaceImplementsRow {
	rows := r.scoped.IfaceImplementsRowsInRepos(repos)
	r.counts.queries++
	r.counts.implements += len(rows)
	return rows
}
func (r *countedDeadCodeReader) NodesByKindInRepos(k graph.NodeKind, repos []string) iter.Seq[*graph.Node] {
	r.counts.queries++
	return func(yield func(*graph.Node) bool) {
		for node := range r.scoped.NodesByKindInRepos(k, repos) {
			r.counts.files++
			if !yield(node) {
				return
			}
		}
	}
}

func scopedDeadCodeStore(t testing.TB, sqlite bool) graph.Store {
	t.Helper()
	if !sqlite {
		return graph.New()
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "dead-code.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedScopedDeadCode(g graph.Store) {
	nodes := []*graph.Node{
		{ID: "a/file", Name: "lib.go", Kind: graph.KindFile, FilePath: "a/lib.go", RepoPrefix: "a", WorkspaceID: "ws-a"},
		{ID: "a/entry", Name: "entry.go", Kind: graph.KindFile, FilePath: "a/entry.go", RepoPrefix: "a", WorkspaceID: "ws-a", Meta: map[string]any{"entry_point": true}},
		{ID: "a/lib.go::unused", Name: "unused", Kind: graph.KindFunction, Language: "go", FilePath: "a/lib.go", StartLine: 2, RepoPrefix: "a", WorkspaceID: "ws-a", ProjectID: "project"},
		{ID: "a/lib.go::used", Name: "used", Kind: graph.KindFunction, Language: "go", FilePath: "a/lib.go", StartLine: 4, RepoPrefix: "a", WorkspaceID: "ws-a"},
		{ID: "a/lib.go::worker", Name: "worker", Kind: graph.KindType, Language: "go", FilePath: "a/lib.go", StartLine: 6, RepoPrefix: "a", WorkspaceID: "ws-a"},
		{ID: "a/lib.go::worker.work", Name: "work", Kind: graph.KindMethod, Language: "go", FilePath: "a/lib.go", StartLine: 8, RepoPrefix: "a", WorkspaceID: "ws-a"},
		{ID: "a/entry.go::boot", Name: "boot", Kind: graph.KindFunction, Language: "go", FilePath: "a/entry.go", RepoPrefix: "a", WorkspaceID: "ws-a"},
		{ID: "b/file", Name: "lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", WorkspaceID: "ws-b"},
		{ID: "b/lib.go::caller", Name: "caller", Kind: graph.KindFunction, Language: "go", FilePath: "b/lib.go", RepoPrefix: "b", WorkspaceID: "ws-b"},
		{ID: "b/lib.go::contract", Name: "contract", Kind: graph.KindInterface, Language: "go", FilePath: "b/lib.go", RepoPrefix: "b", WorkspaceID: "ws-b", Meta: map[string]any{"methods": []string{"work"}}},
	}
	g.AddBatch(nodes, []*graph.Edge{
		{From: "b/lib.go::caller", To: "a/lib.go::used", Kind: graph.EdgeCalls},
		{From: "a/lib.go::worker.work", To: "a/lib.go::worker", Kind: graph.EdgeMemberOf},
		{From: "a/lib.go::worker", To: "b/lib.go::contract", Kind: graph.EdgeImplements},
	})
}

func addUnrelatedDeadCode(g graph.Store, repo string, count int) {
	nodes := make([]*graph.Node, 0, count*4)
	edges := make([]*graph.Edge, 0, count)
	for i := range count {
		base := fmt.Sprintf("%s/file%d.go", repo, i)
		for _, spec := range []struct {
			suffix string
			kind   graph.NodeKind
		}{{"file", graph.KindFile}, {"unused", graph.KindFunction}, {"worker", graph.KindType}, {"contract", graph.KindInterface}} {
			node := &graph.Node{ID: base + "::" + spec.suffix, Name: spec.suffix, Kind: spec.kind, FilePath: base, Language: "go", RepoPrefix: repo, WorkspaceID: "other"}
			if spec.kind == graph.KindInterface {
				node.Meta = map[string]any{"methods": []string{"work"}}
			}
			nodes = append(nodes, node)
		}
		edges = append(edges, &graph.Edge{From: base + "::worker", To: base + "::contract", Kind: graph.EdgeImplements})
	}
	g.AddBatch(nodes, edges)
}

func TestDeadCodeScopedRowsIndependentOfOtherRepos(t *testing.T) {
	for _, sqlite := range []bool{false, true} {
		t.Run(fmt.Sprintf("sqlite=%t", sqlite), func(t *testing.T) {
			g := scopedDeadCodeStore(t, sqlite)
			seedScopedDeadCode(g)
			count := func() deadCodeReadCounts {
				counts := &deadCodeReadCounts{}
				reader := &countedDeadCodeReader{Reader: g, scoped: g.(graph.ScopedDeadCodeReader), counts: counts}
				entries := FindDeadCode(graph.BindReadContext(reader, t.Context()), nil, nil, FindDeadCodeOptions{RepoPrefixes: []string{"a"}})
				require.NotEmpty(t, entries)
				require.Equal(t, 3, counts.queries, "request binding must retain every scoped read capability")
				require.Zero(t, counts.allNodes)
				return *counts
			}
			before := count()
			require.Positive(t, before.candidates)
			require.Positive(t, before.implements)
			require.Positive(t, before.files)
			addUnrelatedDeadCode(g, "unrelated", 1000)
			require.Equal(t, before, count(), "a larger unrelated repo must not increase scoped rows (stronger than the 5% gate)")
		})
	}
}

func TestDeadCodeScopedProcessesUseScopedEntryPoints(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a/root", Name: "entry", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/lib.go", Language: "go"},
		{ID: "a/other", Name: "other", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/lib.go", Language: "go"},
		{ID: "b/root", Name: "entry", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/lib.go", Language: "go"},
	}, nil)
	processes := &ProcessResult{Processes: []Process{
		{EntryPoint: "a/root"},
		{EntryPoint: "b/root", Steps: []Step{{ID: "a/other"}}},
	}}
	entries := FindDeadCode(g, processes, nil, FindDeadCodeOptions{RepoPrefixes: []string{"a"}})
	require.Len(t, entries, 1)
	require.Equal(t, "a/other", entries[0].ID, "only processes whose entry point is in scope may rescue candidates")
}

// Hides optional capabilities to retain the old whole-reader algorithm as an
// independent oracle, and to exercise composed-reader fallbacks.
type legacyDeadCodeReader struct{ graph.Reader }

func TestDeadCodeScopedMatchesPostFiltered(t *testing.T) {
	for _, sqlite := range []bool{false, true} {
		t.Run(fmt.Sprintf("sqlite=%t", sqlite), func(t *testing.T) {
			g := scopedDeadCodeStore(t, sqlite)
			seedScopedDeadCode(g)
			addUnrelatedDeadCode(g, "unrelated", 20)
			var want []DeadCodeEntry
			for _, entry := range FindDeadCode(legacyDeadCodeReader{g}, nil, nil) {
				if g.GetNode(entry.ID).RepoPrefix == "a" {
					want = append(want, entry)
				}
			}
			require.NotEmpty(t, want)
			for _, reader := range []graph.Reader{g, legacyDeadCodeReader{g}} {
				require.Equal(t, want, FindDeadCode(reader, nil, nil, FindDeadCodeOptions{RepoPrefixes: []string{"a"}}))
			}
		})
	}
}

func TestDeadCodeCrossRepoCallerKeepsSymbolAlive(t *testing.T) {
	for _, sqlite := range []bool{false, true} {
		t.Run(fmt.Sprintf("sqlite=%t", sqlite), func(t *testing.T) {
			g := scopedDeadCodeStore(t, sqlite)
			seedScopedDeadCode(g)
			entries := FindDeadCode(g, nil, nil, FindDeadCodeOptions{RepoPrefixes: []string{"a"}})
			ids := make(map[string]bool)
			for _, entry := range entries {
				ids[entry.ID] = true
				if entry.ID == "a/lib.go::unused" {
					require.Equal(t, "a", entry.RepoPrefix)
					require.Equal(t, "ws-a", entry.WorkspaceID)
					data, err := json.Marshal(entry)
					require.NoError(t, err)
					require.Equal(t, `{"id":"a/lib.go::unused","name":"unused","kind":"function","file_path":"a/lib.go","start_line":2}`, string(data))
				}
			}
			require.True(t, ids["a/lib.go::unused"])
			require.False(t, ids["a/lib.go::used"], "its only caller belongs to repo b")
			require.False(t, ids["a/lib.go::worker.work"], "the implemented interface belongs to repo b")
			require.False(t, ids["a/entry.go::boot"], "the scoped file carries entry-point evidence")
			require.Empty(t, FindDeadCode(g, nil, nil, FindDeadCodeOptions{RepoPrefixes: []string{}}))
		})
	}
}

func BenchmarkDeadCodeScopedSixRepo(b *testing.B) {
	g := scopedDeadCodeStore(b, true)
	seedScopedDeadCode(g)
	for _, repo := range []string{"c", "d", "e", "f"} {
		addUnrelatedDeadCode(g, repo, 1000)
	}
	b.ResetTimer()
	for b.Loop() {
		FindDeadCode(g, nil, nil, FindDeadCodeOptions{RepoPrefixes: []string{"a"}})
	}
}
