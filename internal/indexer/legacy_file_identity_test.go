package indexer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// These are real native-Windows reindex regressions, not path simulation on
// POSIX. The old extraction input matches v0.63.8's filepath.Rel graph key.
func TestWindowsReindexHealsLegacyOrdinaryIdentities(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("requires native Windows path semantics")
	}
	for _, language := range []string{"python", "javascript"} {
		for _, backend := range []string{"memory", "sqlite"} {
			for _, changed := range []bool{false, true} {
				name := language + "/" + backend + "/unchanged"
				if changed {
					name = language + "/" + backend + "/changed"
				}
				t.Run(name, func(t *testing.T) { testWindowsLegacyReindex(t, language, backend, changed) })
			}
		}
	}
}

func testWindowsLegacyReindex(t *testing.T, language, backend string, changed bool) {
	ext, source, callerSource, rootSource, extractor := legacyIdentityFixture(language)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	rel := "sub/sample" + ext
	path := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "caller"+ext), []byte(callerSource), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "root"+ext), []byte(rootSource), 0o644))
	var store graph.Store = graph.New()
	if backend == "sqlite" {
		disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, disk.Close()) })
		store = disk
	}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(store, registry, cfg.Index, zap.NewNop())
	idx.SetRepoPrefix("repo")
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	canonical := "repo/" + rel
	native := "repo/" + filepath.FromSlash(rel)
	require.NotEqual(t, canonical, native)
	var helper *graph.Node
	for _, node := range store.GetFileNodes(canonical) {
		if node.Name == "Helper" && node.Kind == graph.KindFunction {
			helper = node
		}
	}
	require.NotNil(t, helper)
	legacyResult, err := extractor.Extract(filepath.FromSlash(rel), []byte(source))
	require.NoError(t, err)
	defer legacyResult.ReleaseTree()
	idx.applyRepoPrefix(legacyResult.Nodes, legacyResult.Edges)
	store.AddBatch(legacyResult.Nodes, legacyResult.Edges)
	oldHelper := native + strings.TrimPrefix(helper.ID, canonical)
	require.NotNil(t, store.GetNode(oldHelper))
	// Move an actual caller's already resolved edge to the retained old ID.
	// Requiring that edge afterward prevents a mere duplicate-search fix from
	// silently losing foreign callers during endpoint eviction.
	var callerEdge *graph.Edge
	for _, edge := range store.GetInEdges(helper.ID) {
		if edge.Kind == graph.EdgeCalls && edge.FilePath == "repo/caller"+ext {
			callerEdge = edge
			break
		}
	}
	require.NotNil(t, callerEdge, "fixture must resolve the caller before corruption")
	moved := *callerEdge
	moved.To = oldHelper
	store.ReindexEdges([]graph.EdgeReindex{{Edge: &moved, OldTo: helper.ID}})
	for _, edge := range store.GetInEdges(helper.ID) {
		require.False(t, edge.Kind == graph.EdgeCalls && edge.From == moved.From, "canonical helper must have no surviving caller to mask a lost legacy edge")
	}
	foreign := "repo/caller" + ext
	targets := []*graph.Node{
		{ID: "repo/license::MIT", Kind: graph.KindLicense, FilePath: native},
		{ID: "repo/team::core", Kind: graph.KindTeam, FilePath: native},
		{ID: "repo/module::pkg", Kind: graph.KindModule, FilePath: native},
	}
	foreignEdges := []*graph.Edge{
		{From: foreign, To: targets[0].ID, Kind: graph.EdgeLicensedAs, FilePath: foreign},
		{From: foreign, To: targets[1].ID, Kind: graph.EdgeOwns, FilePath: foreign},
		{From: foreign, To: targets[2].ID, Kind: graph.EdgeImports, FilePath: foreign},
	}
	store.AddBatch(targets, foreignEdges)
	sibling := `repo/other\untouched` + ext
	store.AddBatch([]*graph.Node{{ID: sibling, Kind: graph.KindFile, FilePath: sibling}}, nil)
	if fts, ok := store.(graph.SymbolFTSBatchUpserter); ok {
		require.NoError(t, fts.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: oldHelper, Tokens: "Helper"}}))
	}
	rootIDs := fileIdentityIDs(store.GetFileNodes("repo/root" + ext))
	require.False(t, idx.IsStale(rel), "unchanged discovery must reach repair before mtime shortcut")
	if changed {
		require.NoError(t, os.WriteFile(path, []byte(strings.Replace(source, "return 1", "return 2", 1)), 0o644))
	}
	for attempt := 0; attempt < 3; attempt++ {
		_, err = idx.IncrementalReindexPaths(dir, []string{path})
		require.NoError(t, err)
		require.Nil(t, store.GetNode(native))
		require.Nil(t, store.GetNode(oldHelper))
		require.NotNil(t, store.GetNode(helper.ID))
		require.Equal(t, rootIDs, fileIdentityIDs(store.GetFileNodes("repo/root"+ext)))
		require.NotNil(t, store.GetNode(sibling), "scoped repair must leave another file alone")
		for _, target := range targets {
			require.Equal(t, native, store.GetNode(target.ID).FilePath)
			require.Len(t, store.GetInEdges(target.ID), 1, "foreign coverage edges must survive")
		}
		var calls int
		for _, edge := range store.GetOutEdges(moved.From) {
			if edge.Kind == graph.EdgeCalls && edge.To == helper.ID {
				calls++
			}
		}
		require.Equal(t, 1, calls, "foreign caller must resolve to exactly one canonical symbol")
		if searcher, ok := store.(graph.SymbolSearcher); ok {
			hits, err := searcher.SearchSymbols("Helper", 20)
			require.NoError(t, err)
			var symbols int
			for _, hit := range hits {
				require.NotEqual(t, oldHelper, hit.NodeID, "stale FTS document must be deleted")
				if hit.NodeID == helper.ID {
					symbols++
				}
			}
			require.Equal(t, 1, symbols)
		}
	}
}

func legacyIdentityFixture(language string) (string, string, string, string, parser.Extractor) {
	if language == "python" {
		return ".py", "def Helper():\n    return 1\n", "from sub.sample import Helper\n\ndef caller():\n    return Helper()\n", "def RootOnly():\n    return 3\n", languages.NewPythonExtractor()
	}
	return ".js", "export function Helper() { return 1; }\n", "import { Helper } from './sub/sample.js';\nexport function caller() { return Helper(); }\n", "export function RootOnly() { return 3; }\n", languages.NewJavaScriptExtractor()
}

func fileIdentityIDs(nodes []*graph.Node) map[string]bool {
	ids := make(map[string]bool)
	for _, node := range nodes {
		ids[node.ID] = true
	}
	return ids
}

func TestPOSIXReindexPreservesLiteralBackslashFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is a path separator on Windows")
	}
	for _, language := range []string{"python", "javascript"} {
		t.Run(language, func(t *testing.T) {
			ext, source, _, _, extractor := legacyIdentityFixture(language)
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
			canonical, native := "sub/sample"+ext, `sub\sample`+ext
			for _, rel := range []string{canonical, native} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, rel), []byte(source), 0o644))
			}
			store := graph.New()
			registry := parser.NewRegistry()
			registry.Register(extractor)
			idx := New(store, registry, config.Default().Index, zap.NewNop())
			idx.SetRootPath(dir)
			_, err := idx.Index(dir)
			require.NoError(t, err)
			literalIDs := fileIdentityIDs(store.GetFileNodes(native))
			require.NotEmpty(t, literalIDs)
			for attempt := 0; attempt < 2; attempt++ {
				require.NoError(t, idx.IndexFile(filepath.Join(dir, canonical)))
				require.Equal(t, literalIDs, fileIdentityIDs(store.GetFileNodes(native)))
				require.NotNil(t, store.GetNode(canonical))
				require.NotNil(t, store.GetNode(native))
			}
		})
	}
}
