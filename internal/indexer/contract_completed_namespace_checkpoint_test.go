package indexer

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

func TestContractCompletedNamespaceCheckpointKeepsHistoricalWorkDelta(t *testing.T) {
	files := map[string]string{"routes.go": jobRoutes, "consts.go": jobConsts, "helper.go": jobHelper}
	f := newContractJobFixture(t, files)
	w := newContractLinkedWorktree(t, f, files, "routes.go")
	factory := NewContractLinkedInputCapture(f.options.Materializer)
	actor := NewContractActor(contractLinkedCheckout, []string{"fixture"})
	var cache *contractDeltaCache
	var barriers []graph.ContractWork
	run := func() ContractFollowupReport {
		f.layers++
		inputs, source, err := factory(t.Context(), actor, "fixture")
		require.NoError(t, err)
		defer source.Release()
		snapshot, targets, err := NewContractFollowupSourceCapture(f.options)(t.Context(), source, inputs, "fixture", contractLinkedCheckout)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		snapshot.Key, snapshot.Work = targets[0].Key, targets[0].Work
		var current []graph.ContractWork
		for _, row := range snapshot.Work {
			if row.FilePath == row.RepoPrefix+"/" {
				current = append(current, row)
			}
		}
		require.NotEmpty(t, current, "real immutable ancestry retains its full-census debt")
		if barriers == nil {
			barriers = current
		} else {
			require.Equal(t, barriers, current, "historical debt is not erased or rewritten")
		}
		_, payload, err := f.store.BeginPayloadGeneration(t.Context(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "completed-namespace", LayerID: fmt.Sprintf("job-%d", f.layers), GenerationKind: "contract_analysis", CreatedAt: time.Now().UnixNano()})
		require.NoError(t, err)
		session, err := newContractDeltaSession(cache, snapshot, f.options.Config)
		require.NoError(t, err)
		report, err := RunContractFollowup(t.Context(), ContractFollowupRequest{RebuildReason: ContractFollowupUnknownInterval, Snapshot: snapshot, Payload: payload, Catalog: targets[0].Catalog, Registry: f.options.Registry, Config: f.options.Config, RepoConfigs: snapshot.RepoConfigs, Leases: f.options.Materializer.Leases, Yield: f.options.Yield, Logger: zap.NewNop(), delta: session})
		cache = session.cache
		require.NoError(t, err)
		require.True(t, report.Published)
		require.NotNil(t, cache)
		require.True(t, cache.complete)
		return report
	}
	first := run()
	require.NotNil(t, cache.continuation)
	require.True(t, cache.continuation.resumesBarrier(cache.identity, barriers))
	replaced := append([]graph.ContractWork(nil), barriers...)
	replaced[0].Token += "-new-census"
	require.False(t, cache.continuation.resumesBarrier(cache.identity, replaced), "a new namespace cannot inherit the old census proof")
	require.Equal(t, viewmetrics.ContractModeFill, first.Mode)
	parent := w.commit
	for i, path := range []string{"/after", "/again"} {
		view := w.view(t)
		files["routes.go"] = strings.ReplaceAll(jobRoutes, "RoutePath", `"`+path+`"`)
		writeFile(t, filepath.Join(w.root, "routes.go"), files["routes.go"])
		target, err := source.NewFilesystemSource(w.root)
		require.NoError(t, err)
		dirty, _, err := w.builder.Build(t.Context(), BuildRequest{
			Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GraphID: w.graphID, LayerID: dirtyLayerID(contractLinkedCheckout), CheckoutID: contractLinkedCheckout, GenerationKind: DirtyLayerGenerationKind, BaseGenerationID: parent, TreeOID: "tree-linked", ProvenanceCommitOID: "commit-linked", CreatedAt: time.Now().UnixNano()},
			Base:     commitLayerBase{Reader: view.Reader, cloneSources: view.GenerationSources(), cloneCorpusBase: f.store.AtGeneration(0)}, Target: target, Changes: []LayerPathChange{{Path: "routes.go", Kind: LayerPathModified}}, RootPath: w.root, RepoPrefix: "fixture", WorkspaceID: "fixture", ProjectID: "fixture",
		})
		require.NoError(t, target.Close())
		require.NoError(t, err)
		view.Close()
		w.route(t, w.commit, dirty)
		parent = dirty
		report := run()
		require.Equal(t, viewmetrics.ContractModeDelta, report.Mode)
		require.Equal(t, 1, report.Files)
		require.Equal(t, []string{"fixture/routes.go"}, report.ChangedFiles)
		require.Equal(t, i+1, cache.deltas)
		current := w.view(t)
		require.Equal(t, contractMeta(coldContracts(t, files)), contractMeta(w.contracts(t, current)))
		current.Close()
	}
}
