package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestIncrementalGoProducerWithdrawalAndRestoreRejectsNestedDecoy(t *testing.T) {
	root := t.TempDir()
	producerPath := filepath.Join(root, "producer", "producer.go")
	producerSource := "package producer\nfunc Produce() int { return 7 }\n"
	files := map[string]string{
		"go.mod":                     "module example.test/app\n\ngo 1.24\n",
		"consumer/consumer.go":       "package consumer\nimport \"example.test/app/producer\"\nfunc Consume() int { return producer.Produce() }\n",
		"producer/producer.go":       producerSource,
		"decoy/go.mod":               "module example.test/decoy\n\ngo 1.24\n",
		"decoy/producer/producer.go": "package producer\nfunc Produce() int { return 99 }\n",
	}
	for name, body := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700))
		writeFile(t, filepath.Join(root, name), body)
	}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.GlobalConfig{Repos: []config.RepoEntry{{Name: "fixture", Path: root}}}
	cfg.SetConfigPath(configPath)
	require.NoError(t, cfg.Save())
	manager, err := config.NewConfigManager(configPath)
	require.NoError(t, err)
	index := func() (*MultiIndexer, graph.Store) {
		_, store := newSQLiteIndexer(t)
		mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
		t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
		mi.SetContractCoreRuntime(ContractCoreRuntimeHooks{})
		results, err := mi.IndexAll()
		require.NoError(t, err)
		require.Empty(t, results["fixture"].FailedFiles)
		return mi, store
	}
	mi, store := index()
	caller := "fixture/consumer/consumer.go::Consume"
	producer := "fixture/producer/producer.go::Produce"
	decoy := "fixture/decoy/producer/producer.go::Produce"
	project := func(g graph.Store) []string {
		var rows []string
		for _, edge := range g.GetOutEdges(caller) {
			if edge.Kind == graph.EdgeCalls {
				rows = append(rows, fmt.Sprintf("call %s %s %s %g", edge.To, edge.Origin, edge.Tier, edge.Confidence))
			}
		}
		for _, edge := range g.GetInEdges(caller) {
			if edge.Kind == graph.EdgeValueFlow {
				rows = append(rows, fmt.Sprintf("flow %s %s %s %g", edge.From, edge.Origin, edge.Tier, edge.Confidence))
			}
		}
		slices.Sort(rows)
		return rows
	}
	require.Equal(t, producer, callTargetFrom(t, store, caller))
	require.NotNil(t, store.GetNode(decoy), "the misleading same-named definition must actually exist")
	before := project(store)
	require.Len(t, before, 2, "a positive call and actual returned-value flow make parity nonvacuous")
	writeFile(t, producerPath, "package producer\nfunc Withdrawn() int { return 7 }\n")
	result, err := mi.IncrementalReindexRepo("fixture", []string{producerPath})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.Nil(t, store.GetNode(producer))
	call := callEdgeFrom(t, store, caller)
	require.True(t, graph.IsUnresolvedTarget(call.To), "the cross-repo tail must preserve the per-repo package refusal")
	require.Empty(t, call.Origin)
	require.Zero(t, call.Confidence)
	for _, edge := range store.GetInEdges(caller) {
		if edge.Kind == graph.EdgeValueFlow {
			require.NotEqual(t, decoy, edge.From, "a rejected call must not synthesize a decoy value-flow producer")
		}
	}
	writeFile(t, producerPath, producerSource)
	result, err = mi.IncrementalReindexRepo("fixture", []string{producerPath})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.Equal(t, producer, callTargetFrom(t, store, caller))
	_, cold := index()
	require.Equal(t, before, project(store), "restoring exact source restores the original call and value-flow identities")
	require.Equal(t, project(cold), project(store), "independent cold parse must agree after withdrawal/restoration")
}
