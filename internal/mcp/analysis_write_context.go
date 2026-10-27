package mcp

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type analysisWriteContextBinder interface {
	BindAnalysisWriteContext(context.Context) graph.AnalysisGenerationStore
}

func bindAnalysisWriteContext(writer graph.AnalysisGenerationStore, ctx context.Context) graph.AnalysisGenerationStore {
	if binder, ok := writer.(analysisWriteContextBinder); ok {
		return binder.BindAnalysisWriteContext(ctx)
	}
	return writer // Legacy writer remains actually owned, without a bound claim.
}

// This is an admission/interruptible-SQL budget, not a whole rollback bound.
// The actual physical cleanup remains part of the caller's completion.
const analysisAbortCleanupBudget = 2 * time.Second

func abortAnalysisGeneration(ctx context.Context, writer graph.AnalysisGenerationStore, id int64) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), analysisAbortCleanupBudget)
	defer cancel()
	return bindAnalysisWriteContext(writer, cleanupCtx).AbortAnalysisGeneration(id)
}
