package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Test-only accessor: read version counts under the same lock order as the
// registry rather than exposing mutable maps to concurrent lifecycle tests.
func pendingSourceVersionCount(s *Server, checkoutID string) int {
	p := &s.pendingSourceFiles
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for _, record := range p.versions {
		record.mu.RLock()
		if record.checkoutID == checkoutID {
			count++
		}
		record.mu.RUnlock()
	}
	return count
}

func TestPendingSourceRegistryCanonicalTerminalRetirement(t *testing.T) {
	for name, settle := range map[string]func(*mutationCommitRecord){
		"fresh":       func(r *mutationCommitRecord) { r.recordGraph(mutationReindexOutcome{Reindexed: true}) },
		"failed":      func(r *mutationCommitRecord) { r.recordGraph(mutationReindexOutcome{Err: context.DeadlineExceeded}) },
		"not applied": func(r *mutationCommitRecord) { r.markNotApplied(context.Canceled) },
		"disk failed": func(r *mutationCommitRecord) { r.markFailed(context.DeadlineExceeded) },
	} {
		t.Run(name, func(t *testing.T) {
			s := &Server{}
			record := s.beginMutationCommit(context.Background(), "edit", "", "", "file.go", "/fixture/file.go")
			require.Equal(t, 1, pendingSourceVersionCount(s, ""))
			record.markCommitted("bytes", 5)
			settle(record)
			require.Zero(t, pendingSourceVersionCount(s, ""), "retirement must not wait for a source read or another completion")
		})
	}
}

func TestPendingSourceRegistryCanonicalFreshRetainsCommitOrderEvidence(t *testing.T) {
	s := &Server{}
	old := s.beginMutationCommit(context.Background(), "edit", "", "", "file.go", "/fixture/file.go")
	old.markCommitted("old", 3)
	newer := s.beginMutationCommit(context.Background(), "edit", "", "", "file.go", "/fixture/file.go")
	newer.markCommitted("new", 3)
	newer.recordGraph(mutationReindexOutcome{Reindexed: true})
	require.Zero(t, pendingSourceVersionCount(s, ""))
	require.Equal(t, mutationGraphStale, old.snapshot().GraphStatus, "retirement does not rewrite historical publication evidence")
	old.recordGraph(mutationReindexOutcome{Err: context.DeadlineExceeded})
	require.Zero(t, pendingSourceVersionCount(s, ""), "a late failure cannot resurrect a retired version")
}
