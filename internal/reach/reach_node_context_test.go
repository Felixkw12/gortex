package reach

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type contextNodeStoreStub struct {
	graph.Store
	err    error
	called bool
}

func (s *contextNodeStoreStub) GetNodeContext(context.Context, string) (*graph.Node, error) {
	s.called = true
	return nil, s.err
}

func TestGetNodeContextUsesOptionalStoreMethod(t *testing.T) {
	s := &contextNodeStoreStub{err: context.Canceled}
	_, err := getNodeContext(context.Background(), s, "seed")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("getNodeContext error = %v, want context.Canceled", err)
	}
	if !s.called {
		t.Fatal("optional GetNodeContext was not called")
	}
}

type legacyNodeStoreStub struct {
	graph.Store
	called bool
}

func (s *legacyNodeStoreStub) GetNode(string) *graph.Node {
	s.called = true
	return nil
}

func TestGetNodeContextChecksCancellationBeforeLegacyFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &legacyNodeStoreStub{}

	_, err := getNodeContext(ctx, s, "seed")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("getNodeContext error = %v, want context.Canceled", err)
	}
	if s.called {
		t.Fatal("legacy GetNode called after cancellation")
	}
}

type contextNodesStoreStub struct {
	graph.Store
	err    error
	called bool
}

func (s *contextNodesStoreStub) GetNodesByIDsContext(context.Context, []string) (map[string]*graph.Node, error) {
	s.called = true
	return nil, s.err
}

func TestGetNodesContextUsesOptionalStoreMethod(t *testing.T) {
	s := &contextNodesStoreStub{err: context.Canceled}
	_, err := getNodesContext(context.Background(), s, []string{"seed"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("getNodesContext error = %v, want context.Canceled", err)
	}
	if !s.called {
		t.Fatal("optional GetNodesByIDsContext was not called")
	}
}

type blockingContextNodesStore struct {
	graph.Store
}

func (s *blockingContextNodesStore) GetNodesByIDsContext(ctx context.Context, _ []string) (map[string]*graph.Node, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestComputeCancellationBoundsNodeHydration(t *testing.T) {
	g, ids := newCallChain(t, 2)
	store := &blockingContextNodesStore{Store: g}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, truncated := compute(ctx, store, ids[1], nil)
	elapsed := time.Since(started)
	if !truncated {
		t.Fatal("compute must mark a cancelled node hydration as truncated")
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("compute returned after %s, want cancellation-bounded traversal", elapsed)
	}
}
