package mcp

import (
	"context"
	"maps"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// handleGetRepoOutline returns a single-call narrative overview of the
// indexed codebase: primary languages, top communities, load-bearing
// hotspots, most-imported files, and entry points. It's the "new to this
// repo" tool — everything a reader wants to know about the codebase in one
// response without having to assemble it from graph_stats + analyze + manual
// inspection.
//
// Output is compact on purpose (a handful of each list) so it stays under
// ~1k tokens even on large repos. For deeper exploration, the agent
// follows up with smart_context, find_usages, etc. on specific symbols
// surfaced here.
func (s *Server) handleGetRepoOutline(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	const (
		topCommunitiesN  = 5
		topHotspotsN     = 5
		topMostImportedN = 10
		topEntryPointsN  = 10
		topLanguagesN    = 5
	)

	// Bound sessions aggregate the request reader's scope in the backend.
	// Keep unbound sessions on the cached Stats / EdgeCount path.
	opts, bound := s.sessionScopeOptions(ctx)
	if bound {
		opts, _ = s.scopeOptionsWithRepoNarrow(ctx, repoAllowFromContext(ctx))
	}

	// Every node / edge read below goes through the request's reader, so
	// an overlay-active caller sees its own buffers. Hoisted once: the
	// outline touches the reader from several sections.
	reader := s.readerFor(ctx)

	var scoped []*graph.Node
	var inScope map[string]bool
	var aggregated *graph.OutlineCounts
	if bound {
		if counts, ok, err := scopedOutlineCountsFor(reader, graph.NodeScope{
			WorkspaceID: opts.WorkspaceID, ProjectID: opts.ProjectID, RepoAllow: opts.RepoAllow,
		}); err != nil {
			return nil, err
		} else if ok {
			aggregated = &counts
		} else {
			// Overlays deliberately take the existing reader-based path.
			scoped = s.scopedNodesLight(ctx)
			inScope = make(map[string]bool, len(scoped))
			for _, n := range scoped {
				inScope[n.ID] = true
			}
		}
	}
	// The aggregate path resolves only analyzer and entry-point candidates.
	// Cache negative lookups as well, and stop community lookups as soon as
	// one member matches instead of fetching every member of large groups.
	checked := make(map[string]bool)
	candidateScope := make(map[string]bool)
	allowsAny := func(ids []string) bool {
		if !bound {
			return true
		}
		if aggregated == nil {
			for _, id := range ids {
				if inScope[id] {
					return true
				}
			}
			return false
		}
		const batchSize = 128
		for start := 0; start < len(ids); start += batchSize {
			chunk := ids[start:min(start+batchSize, len(ids))]
			var missing []string
			for _, id := range chunk {
				if !checked[id] {
					missing = append(missing, id)
					checked[id] = true
				}
			}
			if len(missing) > 0 {
				for id, n := range reader.GetNodesByIDs(missing) {
					candidateScope[id] = opts.ScopeAllows(n)
				}
			}
			for _, id := range chunk {
				if candidateScope[id] {
					return true
				}
			}
		}
		return false
	}

	// Language breakdown — computed from the scoped node set so the
	// counts reflect only the session's workspace.
	type langEntry struct {
		Name  string `json:"name"`
		Nodes int    `json:"nodes"`
	}
	langCounts := make(map[string]int)
	totalScopedNodes := 0
	if aggregated != nil {
		maps.Copy(langCounts, aggregated.ByLanguage)
		totalScopedNodes = aggregated.TotalNodes
	} else if bound {
		for _, n := range scoped {
			if n.Language != "" {
				langCounts[n.Language]++
			}
		}
		totalScopedNodes = len(scoped)
	} else {
		// Unbound: Stats().ByLanguage already aggregates this server-
		// side; the cgo cost is one GROUP BY instead of one row per node.
		stats := reader.Stats()
		maps.Copy(langCounts, stats.ByLanguage)
		totalScopedNodes = stats.TotalNodes
	}
	var languages []langEntry
	for name, n := range langCounts {
		languages = append(languages, langEntry{Name: name, Nodes: n})
	}
	sort.Slice(languages, func(i, j int) bool {
		if languages[i].Nodes != languages[j].Nodes {
			return languages[i].Nodes > languages[j].Nodes
		}
		return languages[i].Name < languages[j].Name
	})
	primaryLang := ""
	if len(languages) > 0 {
		primaryLang = languages[0].Name
	}
	if len(languages) > topLanguagesN {
		languages = languages[:topLanguagesN]
	}

	// Edge count, bounded to edges whose endpoints are both in scope.
	// Unbound sessions never set inScope, so the count is exactly
	// the backend's EdgeCount() — an O(1) lookup that skips
	// materialising every edge over cgo.
	totalEdges := 0
	if aggregated != nil {
		totalEdges = aggregated.TotalEdges
	} else if inScope == nil {
		totalEdges = reader.EdgeCount()
	} else {
		for _, e := range reader.AllEdges() {
			if !inScope[e.From] || !inScope[e.To] {
				continue
			}
			totalEdges++
		}
	}

	summary := map[string]any{
		"total_nodes":      totalScopedNodes,
		"total_edges":      totalEdges,
		"primary_language": primaryLang,
		"languages":        languages,
	}

	// Communities — top N by member count, filtered to communities
	// with at least one member inside the session's workspace.
	communitiesSection := map[string]any{"count": 0}
	if comms := s.getCommunities(); comms != nil && len(comms.Communities) > 0 {
		sorted := make([]analysis.Community, 0, len(comms.Communities))
		for _, c := range comms.Communities {
			if allowsAny(c.Members) {
				sorted = append(sorted, c)
			}
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Size > sorted[j].Size
		})
		top := sorted
		if len(top) > topCommunitiesN {
			top = top[:topCommunitiesN]
		}
		communitiesSection = map[string]any{
			"count":      len(sorted),
			"modularity": comms.Modularity,
			"top":        topCommunitiesSummary(top),
		}
	}

	// Hotspots — load-bearing symbols by fan-in/out/crossings. Use a low
	// threshold to ensure we get the top N regardless of repo size.
	// Post-filtered to the session's workspace.
	hotspotsSection := []map[string]any{}
	hs := s.getHotspots()
	for i, h := range hs {
		if len(hotspotsSection) >= topHotspotsN {
			break
		}
		if aggregated != nil && i%128 == 0 {
			// Resolve one ranked head at a time; stop once the outline has
			// enough visible hotspots rather than hydrating the entire list.
			end := min(i+128, len(hs))
			ids := make([]string, 0, end-i)
			for _, candidate := range hs[i:end] {
				if !checked[candidate.ID] {
					ids = append(ids, candidate.ID)
					checked[candidate.ID] = true
				}
			}
			if len(ids) > 0 {
				for id, n := range reader.GetNodesByIDs(ids) {
					candidateScope[id] = opts.ScopeAllows(n)
				}
			}
		}
		if bound && !allowsAny([]string{h.ID}) {
			continue
		}
		hotspotsSection = append(hotspotsSection, map[string]any{
			"id":               h.ID,
			"name":             h.Name,
			"kind":             h.Kind,
			"file_path":        h.FilePath,
			"fan_in":           h.FanIn,
			"fan_out":          h.FanOut,
			"complexity_score": h.ComplexityScore,
		})
	}

	imported := []map[string]any(nil)
	var entries []map[string]any
	if aggregated != nil {
		counts := make(map[string]int, len(aggregated.FileImports))
		for _, row := range aggregated.FileImports {
			counts[row.FilePath] = row.Count
		}
		imported = rankImportedFiles(counts, topMostImportedN)
		mains := reader.FindNodesByName("main")
		ids := make([]string, 0, len(mains))
		for _, n := range mains {
			if n != nil {
				ids = append(ids, n.ID)
			}
		}
		for id, n := range reader.GetNodesByIDs(ids) {
			candidateScope[id] = opts.ScopeAllows(n)
		}
		entries = entryPointsForNodes(mains, candidateScope, topEntryPointsN)
	} else {
		imported = mostImportedFiles(reader, inScope, topMostImportedN)
		entries = entryPoints(reader, inScope, topEntryPointsN)
	}
	return s.respondJSONOrTOON(ctx, req, map[string]any{
		"summary":             summary,
		"communities":         communitiesSection,
		"hotspots":            hotspotsSection,
		"most_imported_files": imported,
		"entry_points":        entries,
	})
}

// topCommunitiesSummary shapes a subset of communities for the outline.
// Trimmed from the full Community struct (members can be thousands of IDs)
// to just label, size, and cohesion — enough for the reader to decide
// whether to drill into that subsystem.
func topCommunitiesSummary(comms []analysis.Community) []map[string]any {
	out := make([]map[string]any, 0, len(comms))
	for _, c := range comms {
		out = append(out, map[string]any{
			"id":       c.ID,
			"label":    c.Label,
			"size":     c.Size,
			"cohesion": c.Cohesion,
		})
	}
	return out
}

// mostImportedFiles ranks files by incoming `imports` edges. This surfaces
// the shared modules — packages everyone reaches for — which is a strong
// "here's where the gravity lives" signal for newcomers.
// inScope, when non-nil, bounds the ranking to imports whose target
// node is inside the session's workspace.
//
// Picks the FileImportAggregator capability when the backend
// implements it (one server-side aggregate ships back the per-file count
// instead of materialising every edge over cgo just to bucket).
// Falls back to the AllEdges-driven loop on backends that don't — which
// is what an overlay view takes, since it implements no aggregate.
func mostImportedFiles(g graph.Reader, inScope map[string]bool, topN int) []map[string]any {
	counts := make(map[string]int)
	if ag, ok := g.(graph.FileImportAggregator); ok {
		var scope []string
		if inScope != nil {
			scope = make([]string, 0, len(inScope))
			for id := range inScope {
				scope = append(scope, id)
			}
			// An empty inScope means "nothing matches" — the
			// aggregator contract maps that to nil so we never
			// fire a whole-graph scan on a bound session.
			if len(scope) == 0 {
				scope = []string{}
			}
		}
		for _, r := range ag.FileImportCounts(scope) {
			counts[r.FilePath] = r.Count
		}
	} else {
		for _, e := range g.AllEdges() {
			if e.Kind != graph.EdgeImports {
				continue
			}
			target := g.GetNode(e.To)
			if target == nil {
				continue
			}
			if inScope != nil && !inScope[target.ID] {
				continue
			}
			// Aggregate at the file level. For Import-kind nodes the node's
			// FilePath is the file being imported; for File-kind nodes the
			// ID is already the path.
			path := target.FilePath
			if path == "" {
				path = target.ID
			}
			counts[path]++
		}
	}

	return rankImportedFiles(counts, topN)
}

func rankImportedFiles(counts map[string]int, topN int) []map[string]any {
	type fileCount struct {
		path  string
		count int
	}
	var ranked []fileCount
	for p, c := range counts {
		ranked = append(ranked, fileCount{path: p, count: c})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count > ranked[j].count
		}
		return ranked[i].path < ranked[j].path
	})
	if len(ranked) > topN {
		ranked = ranked[:topN]
	}

	out := make([]map[string]any, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, map[string]any{
			"path":         r.path,
			"import_count": r.count,
		})
	}
	return out
}

// entryPoints finds likely program entry points: functions named `main`
// (the Go / Rust / C convention) and top-level functions with no callers
// in files named `main.*` or `cmd/**`. Good enough for the outline; a
// fuller process-based walk is what `get_processes` does separately.
//
// Lookup goes through FindNodesByName so the name index runs server-
// side on disk backends — the legacy nodes-slice walk pulled the whole
// node table just to keep the ~10 nodes literally named "main". When
// an inScope filter is supplied (bound session), it's applied after
// the name lookup so a bound session never sees mains from other
// workspaces.
func entryPoints(g graph.Reader, inScope map[string]bool, topN int) []map[string]any {
	return entryPointsForNodes(g.FindNodesByName("main"), inScope, topN)
}

func entryPointsForNodes(nodes []*graph.Node, inScope map[string]bool, topN int) []map[string]any {
	type ep struct {
		id       string
		name     string
		filePath string
	}
	var out []ep
	for _, n := range nodes {
		if n == nil {
			continue
		}
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if inScope != nil && !inScope[n.ID] {
			continue
		}
		out = append(out, ep{id: n.ID, name: n.Name, filePath: n.FilePath})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].filePath < out[j].filePath
	})
	if len(out) > topN {
		out = out[:topN]
	}

	shaped := make([]map[string]any, 0, len(out))
	for _, e := range out {
		shaped = append(shaped, map[string]any{
			"id":        e.id,
			"name":      e.name,
			"file_path": e.filePath,
		})
	}
	return shaped
}
