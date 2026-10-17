package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
)

func TestWatcherAgentConfigIsExcluded(t *testing.T) {
	root := t.TempDir()
	idx := &Indexer{rootPath: root}
	idx.config.Exclude = []string{"indexer-only.go"}
	patterns := append(append([]string{}, excludes.Builtin...), "custom.go")
	w, err := NewWatcher(idx, config.WatchConfig{Exclude: patterns}, zap.NewNop())
	require.NoError(t, err)
	writeExcludeFixture(t, filepath.Join(root, "sub/.gortexignore"), "ignored.go\n")
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"claude_mcp", ".claude/mcp.json", false},
		{"kiro_mcp", ".kiro/mcp.json", false},
		{"agent_state", ".claude/state.unknown", true},
		{"per_folder_ignore", "sub/ignored.go", true},
		{"watcher_own_flat_list", "custom.go", true},
		{"indexer_only_flat_list", "indexer-only.go", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.path)
			writeExcludeFixture(t, path, "fixture\n")
			require.Equal(t, tc.want, w.isExcluded(path))
		})
	}
	t.Run("escaping_symlink", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside.go")
		writeFile(t, outside, "package outside\n")
		path := filepath.Join(root, "escape.go")
		if err := os.Symlink(outside, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		require.True(t, w.isExcluded(path))
	})
}

func TestWatcherAgentConfigBackendFilter(t *testing.T) {
	root := t.TempDir()
	patterns := append(append([]string{}, excludes.Builtin...), "gen", "!gen/keep.go")
	w, err := NewWatcher(&Indexer{rootPath: root}, config.WatchConfig{Exclude: patterns}, zap.NewNop())
	require.NoError(t, err)
	filter := &watcherExcludeFilter{w: w}
	// These paths need not exist: the backend filter must make no filesystem reads.
	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"probe_first", ".git/" + probeMarker + "check", true},
		{"claude_mcp", ".claude/mcp.json", true},
		{"claude_subdirectory", ".claude/sub", true},
		{"agent_state_deferred", ".claude/state.unknown", true},
		{"negated_child_folder", "gen", true},
		{"ordinary_file", "main.go", true},
		{"ignored_tree", "node_modules/pkg/main.js", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, filter.ShouldInclude(filepath.Join(root, tc.path)))
		})
	}
	t.Run("rootless_match_rel", func(t *testing.T) {
		w, err := NewWatcher(&Indexer{}, config.WatchConfig{Exclude: []string{"gen", "!gen/keep.go"}}, zap.NewNop())
		require.NoError(t, err)
		filter := &watcherExcludeFilter{w: w}
		require.False(t, filter.ShouldInclude("gen"))
		require.True(t, filter.ShouldInclude("gen/keep.go"))
		require.True(t, filter.ShouldInclude("main.go"))
		require.True(t, w.isExcluded("gen"))
		require.False(t, w.isExcluded("main.go"))
	})
}

func TestWatcherAgentConfigMCPEdit(t *testing.T) {
	root := t.TempDir()
	reg := parser.NewRegistry()
	reg.Register(languages.NewMCPConfigExtractor())
	idx := New(graph.New(), reg, config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.SetRootPath(root)
	path := filepath.Join(root, ".claude/mcp.json")
	writeExcludeFixture(t, path, `{"mcpServers":{"edited":{"command":"npx"}}}`)
	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)
	// No backend loop was started; signal it stopped before joining owned timers.
	t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
	scheduled := make(chan string, 1)
	w.pointMutationPatch = func(path string, kind ChangeKind, generation uint64) error {
		scheduled <- path
		return nil
	}
	w.handleEvent(fswatcher.WatchEvent{Path: path, Types: []fswatcher.EventType{fswatcher.EventMod}})
	select {
	case got := <-scheduled:
		require.Equal(t, path, got, "MCP edit must reach the file reindex queue")
	case <-time.After(2 * time.Second):
		t.Fatal("MCP edit did not reach the file reindex queue")
	}
}

func TestWatcherAgentConfigNewDirectoryScan(t *testing.T) {
	for _, tc := range []struct {
		name  string
		dir   string
		file  string
		body  string
		extra []string
	}{
		{"reincluded_child", "gen", "keep.go", "package gen\nfunc Keep() {}\n", []string{"gen", "!gen/keep.go"}},
		{"claude_directory", ".claude", "mcp.json", `{"mcpServers":{"newserver":{"command":"npx"}}}`, nil},
		{"claude_subdirectory", ".claude/sub", "mcp.json", `{"mcpServers":{"newserver":{"command":"npx"}}}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			reg.Register(languages.NewMCPConfigExtractor())
			patterns := append(append([]string{}, excludes.Builtin...), tc.extra...)
			idx := New(graph.New(), reg, config.IndexConfig{Workers: 1, Exclude: patterns}, zap.NewNop())
			idx.SetRootPath(root)
			w, err := NewWatcher(idx, config.WatchConfig{Exclude: patterns}, zap.NewNop())
			require.NoError(t, err)
			t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
			dir := filepath.Join(root, tc.dir)
			path := filepath.Join(dir, tc.file)
			writeExcludeFixture(t, path, tc.body)
			rel := tc.dir + "/" + tc.file
			require.Empty(t, idx.graph.GetFileNodes(rel))

			type scanResult struct {
				paths []string
				err   error
			}
			scanned := make(chan scanResult, 1)
			// Observe the scheduled scope, then run the real discovery pipeline.
			w.discoverReindex = func(paths []string) (*IndexResult, error) {
				result, err := idx.incrementalDiscoverWatcherPaths(root, paths)
				scanned <- scanResult{paths: paths, err: err}
				return result, err
			}
			w.handleEvent(fswatcher.WatchEvent{Path: dir, Types: []fswatcher.EventType{fswatcher.EventCreate}})
			select {
			case got := <-scanned:
				require.NoError(t, got.err)
				require.Equal(t, []string{dir}, got.paths, "enqueueDirScan must schedule the new folder")
				require.NotEmpty(t, idx.graph.GetFileNodes(rel), "the scan must reindex the child file")
				require.Contains(t, idx.FileMtimes(), rel)
			case <-time.After(2 * time.Second):
				t.Fatal("new-directory create did not enqueue a discovery scan")
			}
		})
	}
}
