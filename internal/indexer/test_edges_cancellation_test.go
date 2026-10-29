package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type cancelTestProjectionStore struct {
	*graph.Graph
	cancel context.CancelFunc
	reads  int
}

func (g *cancelTestProjectionStore) NodesInFilesByKind(files []string, kinds []graph.NodeKind) []*graph.Node {
	g.reads++
	nodes := g.Graph.NodesInFilesByKind(files, kinds)
	if g.cancel != nil {
		g.cancel()
	}
	return nodes
}

func TestRetargetedTestProjectionCancellationKeepsCompletedGroupsRepairable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := &cancelTestProjectionStore{Graph: graph.New(), cancel: cancel}
	g.AddNode(&graph.Node{ID: "prod::Subject", Kind: graph.KindFunction, Name: "Subject", FilePath: "prod.go", Language: "go"})
	files := make([]string, testMetadataLookupBatchSize+1)
	for i := range files {
		file := fmt.Sprintf("pkg/f%04d_test.go", i)
		files[i] = file
		g.AddBatch([]*graph.Node{
			{ID: file, Kind: graph.KindFile, FilePath: file, Language: "go"},
			{ID: file + "::TestSubject", Kind: graph.KindFunction, Name: "TestSubject", FilePath: file, Language: "go"},
		}, []*graph.Edge{
			{From: file + "::TestSubject", To: "prod::Subject", Kind: graph.EdgeCalls, FilePath: file},
			{From: file + "::TestSubject", To: "old::Subject", Kind: graph.EdgeTests, FilePath: file},
		})
	}
	_, _, err := markTestSymbolsAndEmitEdgesForFilesContext(ctx, g, files)
	if !errors.Is(err, context.Canceled) || g.reads != 1 {
		t.Fatalf("cancellation = %v, file batches read = %d", err, g.reads)
	}
	first, last := files[0]+"::TestSubject", files[len(files)-1]+"::TestSubject"
	if findEdge(g, graph.EdgeTests, first, "prod::Subject") == nil || findEdge(g, graph.EdgeTests, first, "old::Subject") != nil {
		t.Fatal("cancellation split the completed group's eviction/replacement")
	}
	if findEdge(g, graph.EdgeTests, last, "prod::Subject") != nil || findEdge(g, graph.EdgeTests, last, "old::Subject") == nil {
		t.Fatal("cancellation started the remaining group's projection")
	}
	// Retry the same frontier normally, including already completed work.
	g.cancel = nil
	if _, _, err = markTestSymbolsAndEmitEdgesForFilesContext(context.Background(), g, files); err != nil {
		t.Fatal(err)
	}
	if findEdge(g, graph.EdgeTests, last, "prod::Subject") == nil || findEdge(g, graph.EdgeTests, last, "old::Subject") != nil {
		t.Fatal("normal repair did not replace the remaining stale projection")
	}
}

func TestRetargetedTestProjectionCanceledBeforeRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := &cancelTestProjectionStore{Graph: graph.New()}
	_, _, err := markTestSymbolsAndEmitEdgesForFilesContext(ctx, g, []string{"pkg/subject_test.go"})
	if !errors.Is(err, context.Canceled) || g.reads != 0 {
		t.Fatalf("cancellation = %v, graph reads = %d", err, g.reads)
	}
}
