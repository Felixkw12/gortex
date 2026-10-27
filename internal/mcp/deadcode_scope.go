package mcp

import (
	"context"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

func (s *Server) deadCodeRepoPrefixes(ctx context.Context) []string {
	narrow := repoAllowFromContext(ctx)
	opts, active := s.scopeOptionsWithRepoNarrow(ctx, narrow)
	if !active {
		return nil
	}
	if strings.HasPrefix(opts.WorkspaceID, unresolvedWorkspacePrefix) {
		return []string{}
	}
	if len(opts.RepoAllow) == 0 {
		// A workspace slug is not a repository prefix. Without a resolved
		// repo set, retain the metadata-filtered fallback rather than guess
		// ownership from a registry that may be mid-untrack/reconciliation.
		return nil
	}
	prefixes := make([]string, 0, len(opts.RepoAllow)+1)
	for prefix, allowed := range opts.RepoAllow {
		if allowed {
			prefixes = append(prefixes, prefix)
		}
	}
	// Unlike outline's NodeScope, dead_code's explicit analyze repo narrow
	// excludes the unowned bucket. A session ceiling alone still admits it.
	if len(narrow) == 0 && !opts.RepoAllow[""] {
		prefixes = append(prefixes, "")
	}
	sort.Strings(prefixes)
	return prefixes
}

func (s *Server) deadCodeProcesses(ctx context.Context, reader graph.Reader) *analysis.ProcessResult {
	processes := s.getProcesses()
	if processes == nil || !s.scopeFiltersActive(ctx) {
		return processes
	}
	ids := make([]string, 0, len(processes.Processes))
	for _, process := range processes.Processes {
		ids = append(ids, process.EntryPoint)
	}
	nodes := reader.GetNodesByIDs(ids)
	filtered := *processes
	filtered.Processes = nil
	for _, process := range processes.Processes {
		if s.analyzeNodeVisible(ctx, nodes[process.EntryPoint]) {
			filtered.Processes = append(filtered.Processes, process)
		}
	}
	return &filtered
}
