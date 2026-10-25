package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Embedding Reader hides optional capabilities to exercise the original
// outline implementation as an independent oracle for the golden results.
type legacyOutlineReader struct{ graph.Reader }

type outlineCountingReader struct {
	*store_sqlite.Store
	allNodes, allNodesLight, allEdges, aggregates int
	lookedUp                                      []string
}

func (r *outlineCountingReader) AllNodes() []*graph.Node { r.allNodes++; return r.Store.AllNodes() }
func (r *outlineCountingReader) AllNodesLight() []*graph.Node {
	r.allNodesLight++
	return r.Store.AllNodesLight()
}
func (r *outlineCountingReader) AllEdges() []*graph.Edge { r.allEdges++; return r.Store.AllEdges() }
func (r *outlineCountingReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	r.lookedUp = append(r.lookedUp, ids...)
	return r.Store.GetNodesByIDs(ids)
}
func (r *outlineCountingReader) ScopedOutlineCounts(scope graph.NodeScope) graph.OutlineCounts {
	r.aggregates++
	return r.Store.ScopedOutlineCounts(scope)
}

func (r *outlineCountingReader) ScopedOutlineCountsFiltered(scope graph.NodeScope, filter graph.OutlineEdgeFilter) graph.OutlineCounts {
	r.aggregates++
	return r.Store.ScopedOutlineCountsFiltered(scope, filter)
}

func outlineFixture(t *testing.T) (*Server, *store_sqlite.Store) {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "outline.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	nodes := []*graph.Node{
		{ID: "a-main", Name: "main", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "a/main.go"},
		{ID: "a-lib", Name: "Library", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "a/lib.go"},
		{ID: "b-main", Name: "main", Kind: graph.KindFunction, RepoPrefix: "b", WorkspaceID: "ws", ProjectID: "q", Language: "rust", FilePath: "b/main.rs"},
		{ID: "outside-main", Name: "main", Kind: graph.KindFunction, RepoPrefix: "c", WorkspaceID: "other", ProjectID: "p", Language: "python", FilePath: "c/main.py"},
		{ID: "legacy-main", Name: "main", Kind: graph.KindFunction, RepoPrefix: "legacy", Language: "go", FilePath: "legacy/main.go"},
		{ID: "unowned-main", Name: "main", Kind: graph.KindFunction, WorkspaceID: "ws", ProjectID: "p", Language: "go", FilePath: "source.go"},
		{ID: "global-main", Name: "main", Kind: graph.KindFunction, Language: "go", FilePath: "external::go:fmt"},
		{ID: "global-call", Name: "Call", Kind: graph.KindFunction, ProjectID: "ignored", FilePath: "external-call::symbol"},
	}
	edges := []*graph.Edge{
		{From: "a-main", To: "a-lib", Kind: graph.EdgeImports},
		{From: "b-main", To: "a-lib", Kind: graph.EdgeImports},
		{From: "outside-main", To: "a-lib", Kind: graph.EdgeImports},
		{From: "legacy-main", To: "a-lib", Kind: graph.EdgeImports},
		{From: "a-main", To: "global-main", Kind: graph.EdgeCalls},
		{From: "b-main", To: "global-main", Kind: graph.EdgeImports},
		{From: "outside-main", To: "global-main", Kind: graph.EdgeImports},
		{From: "a-main", To: "unowned-main", Kind: graph.EdgeImports},
		{From: "a-main", To: "b-main", Kind: graph.EdgeCalls},
		{From: "a-main", To: "global-call", Kind: graph.EdgeCalls},
	}
	store.AddBatch(nodes, edges)
	s := &Server{graph: store, session: newSessionState(), sessions: newSessionMap(), logger: zap.NewNop()}
	installCommunitiesForTest(s, &analysis.CommunityResult{Modularity: .75, Communities: []analysis.Community{
		{ID: "a", Label: "A", Size: 3, Cohesion: .5, Members: []string{"missing", "a-lib"}},
		{ID: "b", Label: "B", Size: 2, Cohesion: .6, Members: []string{"b-main"}},
		{ID: "global", Label: "Global", Size: 1, Cohesion: .7, Members: []string{"global-main"}},
		{ID: "stale", Label: "Stale", Size: 0, Members: []string{"missing"}},
	}})
	for _, id := range []string{"outside-main", "a-lib", "b-main", "global-main", "unowned-main", "legacy-main"} {
		n := store.GetNode(id)
		s.hotspots = append(s.hotspots, analysis.HotspotEntry{ID: id, Name: n.Name, Kind: string(n.Kind), FilePath: n.FilePath, FanIn: 2, FanOut: 1, ComplexityScore: 3})
	}
	s.hotspotsReady = true
	return s, store
}

func outlineBoundContext(s *Server, id, workspace string, repos map[string]bool) context.Context {
	ctx := WithSessionID(context.Background(), id)
	ss := s.sessionFor(ctx)
	ss.scopeResolved = true
	ss.scopeBound = true
	ss.scopeWorkspaceID = workspace
	ss.scopeRepoAllow = repos
	return ctx
}

func outlineJSON(t *testing.T, s *Server, ctx context.Context, reader graph.Reader) []byte {
	t.Helper()
	if reader != nil {
		ctx = withRequestView(ctx, &requestView{reader: reader})
	}
	result, err := s.handleGetRepoOutline(ctx, makeReq("get_repo_outline", map[string]any{"format": "json"}))
	require.NoError(t, err)
	require.False(t, result.IsError)
	return []byte(toolResultText(result))
}

func TestOutlineGolden(t *testing.T) {
	s, store := outlineFixture(t)
	for _, tc := range []struct {
		name, workspace string
		repos, narrow   map[string]bool
		unbound         bool
	}{
		{name: "repo", repos: map[string]bool{"a": true}},
		{name: "workspace", workspace: "ws", repos: map[string]bool{"a": true, "b": true}},
		{name: "project", workspace: "ws", repos: map[string]bool{"a": true, "b": true}, narrow: map[string]bool{"a": true}},
		{name: "unbound", unbound: true},
		{name: "global_unowned", workspace: "unknown", repos: map[string]bool{"a": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if !tc.unbound {
				ctx = outlineBoundContext(s, tc.name, tc.workspace, tc.repos)
			}
			if tc.narrow != nil {
				ctx = withRepoAllow(ctx, tc.narrow)
			}
			legacy := outlineJSON(t, s, ctx, legacyOutlineReader{Reader: store})
			got := outlineJSON(t, s, ctx, store)
			golden, err := os.ReadFile(filepath.Join("testdata", "outline", tc.name+".json"))
			require.NoError(t, err)
			require.JSONEq(t, string(golden), string(legacy), "legacy output changed")
			require.JSONEq(t, string(golden), string(got), "aggregate output changed")
		})
	}
}

func TestOutlineBoundSessionNeverScansAllEdges(t *testing.T) {
	s, store := outlineFixture(t)
	reader := &outlineCountingReader{Store: store}
	ctx := outlineBoundContext(s, "counting", "ws", map[string]bool{"a": true})
	outlineJSON(t, s, ctx, reader)
	require.Zero(t, reader.allEdges)
	require.Zero(t, reader.allNodes)
	require.Zero(t, reader.allNodesLight)
	require.Equal(t, 1, reader.aggregates)
	require.NotEmpty(t, reader.lookedUp, "analyzer membership must use candidate lookups")
	for _, id := range reader.lookedUp {
		require.Contains(t, []string{"missing", "a-lib", "b-main", "global-main", "outside-main", "unowned-main", "legacy-main", "a-main"}, id)
	}
}

func TestOutlineUnboundKeepsExistingSummaryPath(t *testing.T) {
	s, store := outlineFixture(t)
	reader := &outlineCountingReader{Store: store}
	outlineJSON(t, s, context.Background(), reader)
	require.Zero(t, reader.aggregates)
	require.Zero(t, reader.allEdges)
	require.Zero(t, reader.allNodes)
	require.Zero(t, reader.allNodesLight)
}

func TestOutlineCommunityStopsAfterMatchingBatch(t *testing.T) {
	s, store := outlineFixture(t)
	members := []string{"a-main"}
	for i := 0; i < 1024; i++ {
		members = append(members, fmt.Sprintf("stale-%d", i))
	}
	installCommunitiesForTest(s, &analysis.CommunityResult{Communities: []analysis.Community{{ID: "large", Size: len(members), Members: members}}})
	s.hotspotsReady = true
	reader := &outlineCountingReader{Store: store}
	outlineJSON(t, s, outlineBoundContext(s, "community", "ws", map[string]bool{"a": true}), reader)
	require.LessOrEqual(t, len(reader.lookedUp), 128+6, "stop after the first matching community batch, plus main candidates")
}

func TestOutlineOverlayReflectsUnsavedBuffers(t *testing.T) {
	s, store := outlineFixture(t)
	ctx := outlineBoundContext(s, "overlay", "ws", map[string]bool{"a": true})
	layer := graph.NewOverlayLayer()
	layer.MarkFile("a/main.go", false)
	layer.AddNode("a/main.go", &graph.Node{ID: "unsaved-main", Name: "main", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "typescript", FilePath: "a/main.go"})
	layer.AddNode("a/main.go", &graph.Node{ID: "unsaved-helper", Name: "Helper", Kind: graph.KindFunction, RepoPrefix: "a", WorkspaceID: "ws", ProjectID: "p", Language: "typescript", FilePath: "a/main.go"})
	layer.AddEdge(&graph.Edge{From: "unsaved-main", To: "a-lib", Kind: graph.EdgeImports, FilePath: "a/main.go"})
	view := graph.NewOverlaidView(store, layer)
	_, aggregates := any(view).(graph.ScopedOutlineAggregator)
	require.False(t, aggregates)
	got := outlineJSON(t, s, WithOverlayView(ctx, view), nil)
	require.Contains(t, string(got), "unsaved-main")
	require.NotContains(t, string(got), `"id":"a-main"`)
	var decoded struct {
		Summary struct {
			TotalNodes int `json:"total_nodes"`
			Languages  []struct {
				Name  string `json:"name"`
				Nodes int    `json:"nodes"`
			} `json:"languages"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(got, &decoded))
	require.Equal(t, 6, decoded.Summary.TotalNodes)
	require.Contains(t, string(got), "typescript")
	require.NotNil(t, store.GetNode("a-main"))
	require.Nil(t, store.GetNode("unsaved-main"))
}

func TestOutlineContractRuntimeNeverScansAndPreservesFilters(t *testing.T) {
	s, store := outlineFixture(t)
	store.AddBatch([]*graph.Node{
		{ID: "contract", Kind: graph.KindContract, RepoPrefix: "a", WorkspaceID: "ws", FilePath: "a/api.yaml"},
		{ID: "config", Kind: graph.KindConfigKey, RepoPrefix: "a", WorkspaceID: "ws", FilePath: "a/config.yaml"},
		{ID: "table", Kind: graph.KindTable, RepoPrefix: "a", WorkspaceID: "ws", FilePath: "a/schema.sql"},
	}, []*graph.Edge{
		{From: "contract", To: "a-lib", Kind: graph.EdgeImports},
		{From: "a-main", To: "config", Kind: graph.EdgeReadsConfig},
		{From: "a-main", To: "a-lib", Kind: graph.EdgeCalls, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "outside-main", To: "a-lib", Kind: graph.EdgeImports, Line: 7, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "a-main", To: "a-lib", Kind: graph.EdgeMatches},
		{From: "a-main", To: "table", Kind: graph.EdgeProvides},
	})
	installContractCoreKindTestRuntime(t, s)
	ctx := outlineBoundContext(s, "runtime", "ws", map[string]bool{"a": true})
	reader := &outlineCountingReader{Store: store}
	got := outlineJSON(t, s, ctx, reader)
	require.Zero(t, reader.allEdges)
	require.Zero(t, reader.allNodes)
	require.Zero(t, reader.allNodesLight)
	require.Equal(t, 1, reader.aggregates, "the production wrapper must use filtered SQL aggregation")
	legacy := outlineJSON(t, s, ctx, legacyOutlineReader{Reader: store})
	require.JSONEq(t, string(legacy), string(got))
	var decoded struct {
		Summary struct {
			TotalEdges int `json:"total_edges"`
		} `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(got, &decoded))
	require.Equal(t, 5, decoded.Summary.TotalEdges, "table provides remains core; contract/kind/metadata edges are excluded")
	// Explicit ownership sets replace kind classification, even when empty.
	for _, ids := range []map[string]bool{nil, {}, {"a-lib": true}} {
		wrapped := newContractCoreEdges(store, ctx, ids)
		aggregated, ok := scopedOutlineCountsFor(wrapped, graph.NodeScope{WorkspaceID: "ws", RepoAllow: map[string]bool{"a": true}})
		require.True(t, ok)
		var count int
		for _, edge := range wrapped.AllEdges() {
			if (graph.NodeScope{WorkspaceID: "ws", RepoAllow: map[string]bool{"a": true}}).Allows(wrapped.GetNode(edge.From)) && (graph.NodeScope{WorkspaceID: "ws", RepoAllow: map[string]bool{"a": true}}).Allows(wrapped.GetNode(edge.To)) {
				count++
			}
		}
		require.Equal(t, count, aggregated.TotalEdges)
	}
}

func TestOutlineHotspotsStopAfterVisibleRankedHead(t *testing.T) {
	s, store := outlineFixture(t)
	for i := 0; i < 1024; i++ {
		s.hotspots = append(s.hotspots, analysis.HotspotEntry{ID: fmt.Sprintf("hot-stale-%d", i)})
	}
	reader := &outlineCountingReader{Store: store}
	outlineJSON(t, s, outlineBoundContext(s, "hotspot-head", "", map[string]bool{"a": true, "b": true, "c": true, "legacy": true}), reader)
	require.NotContains(t, reader.lookedUp, "hot-stale-128")
	require.LessOrEqual(t, len(reader.lookedUp), 128+10, "only the first hotspot head and analyzer/main candidates are needed")
}
