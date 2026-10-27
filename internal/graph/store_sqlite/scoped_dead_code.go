package store_sqlite

import (
	"context"
	"iter"
	"slices"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ScopedDeadCodeReader = (*Store)(nil)
var _ graph.FilteredScopedDeadCodeReader = (*Store)(nil)

func sortedDeadCodeRepos(prefixes []string) []string {
	if prefixes == nil {
		return nil
	}
	out := slices.Clone(prefixes)
	slices.Sort(out)
	return slices.Compact(out)
}

// nodes is WITHOUT ROWID: nodes_by_repo(repo_prefix, view_gen) also carries
// the remaining primary-key id. Repo/generation equality makes id a keyset
// seek and supplies ORDER BY without repeatedly sorting the ownership bucket.
func deadCodeScopedVerdictPageSQL(allowed int) string {
	q, _ := deadCodeFilteredVerdictPageSQL(allowed, true, graph.OutlineEdgeFilter{})
	return q
}

func deadCodeFilteredVerdictPageSQL(allowed int, scoped bool, filter graph.OutlineEdgeFilter) (string, []any) {
	probe := `NOT EXISTS (SELECT 1 FROM edges e `
	source := ""
	if len(filter.ExcludeNodeKinds) > 0 {
		source = "source"
		probe += `LEFT JOIN nodes source ON source.id = e.from_id AND source.view_gen = e.view_gen `
	}
	probe += `WHERE e.to_id = n.id AND e.view_gen = n.view_gen`
	if allowed > 0 {
		probe += ` AND e.kind IN (` + inPlaceholders(allowed) + `)`
	}
	edgeSQL, edgeArgs := outlineEdgePredicate(filter, source, "n")
	viaSQL, viaArgs := outlineViaPredicate(filter.ExcludeVia)
	probe += edgeSQL + viaSQL + `)`
	q := `SELECT n.id, ` + probe + ` FROM nodes AS n`
	if scoped {
		q += ` INDEXED BY nodes_by_repo WHERE n.view_gen = ? AND n.repo_prefix = ?`
	} else {
		q += ` WHERE n.view_gen = ?`
	}
	q += ` AND n.id > ? AND n.kind = ? ORDER BY n.id LIMIT ?`
	return q, append(edgeArgs, viaArgs...)
}

const deadCodeScopedFilesPageSQL = `SELECT ` + lookupNodeCols + ` FROM nodes INDEXED BY nodes_by_repo
WHERE view_gen = ? AND repo_prefix = ? AND id > ? AND kind = ? ORDER BY id LIMIT ?`

func (s *Store) DeadCodeCandidatesInRepos(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string) []*graph.Node {
	out, _ := s.DeadCodeCandidatesInReposContext(s.readContext(), kinds, incoming, prefixes)
	return out
}

// DeadCodeCandidatesInReposContext seeks only the requested ownership buckets.
// The same-generation incoming-usage probe deliberately has no repo predicate:
// a caller in another repo keeps the candidate alive.
func (s *Store) DeadCodeCandidatesInReposContext(ctx context.Context, kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string) ([]*graph.Node, error) {
	if prefixes == nil {
		return s.DeadCodeCandidatesContext(ctx, kinds, incoming)
	}
	return s.deadCodeCandidatesInReposFiltered(ctx, kinds, incoming, prefixes, graph.OutlineEdgeFilter{})
}

func (s *Store) DeadCodeCandidatesInReposFiltered(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string, filter graph.OutlineEdgeFilter) ([]*graph.Node, error) {
	return s.deadCodeCandidatesInReposFiltered(s.readContext(), kinds, incoming, prefixes, filter)
}

func (s *Store) deadCodeCandidatesInReposFiltered(ctx context.Context, kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, prefixes []string, filter graph.OutlineEdgeFilter) ([]*graph.Node, error) {
	repos := sortedDeadCodeRepos(prefixes)
	if prefixes == nil {
		repos = []string{""} // One global pass; no ownership predicate or argument.
	}

	var out []*graph.Node
	for _, kind := range kinds {
		var candidates []*graph.Node
		allowed := anaDedupeEdgeKinds(incoming[kind])
		q, filterArgs := deadCodeFilteredVerdictPageSQL(len(allowed), prefixes != nil, filter)
		for _, prefix := range repos {
			after := ""
			for {
				if err := ctx.Err(); err != nil {
					return append(out, candidates...), err
				}
				args := make([]any, 0, len(allowed)+5)
				for _, edgeKind := range allowed {
					args = append(args, string(edgeKind))
				}
				args = append(args, filterArgs...)
				args = append(args, s.viewGen)
				if prefixes != nil {
					args = append(args, prefix)
				}
				args = append(args, after, string(kind), deadCodePageSize)
				dead, scanned, lastID, err := s.scanDeadCodeVerdictPage(ctx, q, args...)
				if err != nil {
					return append(out, candidates...), err
				}
				if len(dead) > 0 {
					nodes, err := s.nodesByIDsOrdered(ctx, dead)
					candidates = append(candidates, nodes...)
					if err != nil {
						return append(out, candidates...), err
					}
				}
				if scanned < deadCodePageSize {
					break
				}
				after = lastID
			}
		}
		// Preserve the unscoped kind/id order even when prefix spelling and
		// node ids do not share an ordering convention.
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
		out = append(out, candidates...)
	}
	return out, nil
}

func (s *Store) IfaceImplementsRowsInRepos(prefixes []string) []graph.IfaceImplementsRow {
	out, _ := s.IfaceImplementsRowsInReposContext(s.readContext(), prefixes)
	return out
}

func (s *Store) IfaceImplementsRowsInReposContext(ctx context.Context, prefixes []string) ([]graph.IfaceImplementsRow, error) {
	return s.ifaceImplementsRowsInReposFiltered(ctx, prefixes, graph.OutlineEdgeFilter{})
}

func (s *Store) IfaceImplementsRowsInReposFiltered(prefixes []string, filter graph.OutlineEdgeFilter) ([]graph.IfaceImplementsRow, error) {
	return s.ifaceImplementsRowsInReposFiltered(s.readContext(), prefixes, filter)
}

func (s *Store) ifaceImplementsRowsInReposFiltered(ctx context.Context, prefixes []string, filter graph.OutlineEdgeFilter) ([]graph.IfaceImplementsRow, error) {
	repos := sortedDeadCodeRepos(prefixes)
	if prefixes == nil {
		repos = []string{""}
	}
	var out []graph.IfaceImplementsRow
	for _, prefix := range repos {
		var q string
		var args []any
		if prefixes == nil {
			// Preserve legacy dangling implementing-type tuples in the global view.
			q = `SELECT e.from_id, n.id, n.meta FROM edges e
LEFT JOIN nodes implementing ON implementing.id = e.from_id AND implementing.view_gen = e.view_gen
JOIN nodes n ON n.id = e.to_id AND n.view_gen = e.view_gen
WHERE e.kind = ? AND n.kind = ? AND n.meta IS NOT NULL AND e.view_gen = ?`
			args = []any{string(graph.EdgeImplements), string(graph.KindInterface), s.viewGen}
		} else {
			q = `SELECT e.from_id, n.id, n.meta
FROM nodes AS implementing INDEXED BY nodes_by_repo
CROSS JOIN edges e INDEXED BY edges_by_from ON e.from_id = implementing.id AND e.view_gen = implementing.view_gen AND e.kind = ?
JOIN nodes n ON n.id = e.to_id AND n.view_gen = e.view_gen
WHERE implementing.view_gen = ? AND implementing.repo_prefix = ? AND n.kind = ? AND n.meta IS NOT NULL`
			args = []any{string(graph.EdgeImplements), s.viewGen, prefix, string(graph.KindInterface)}
		}
		edgeSQL, edgeArgs := outlineEdgePredicate(filter, "implementing", "n")
		viaSQL, viaArgs := outlineViaPredicate(filter.ExcludeVia)
		args = append(args, edgeArgs...)
		args = append(args, viaArgs...)
		rows, err := outlineRows(ctx, s.db, outlineQuery{sql: q + edgeSQL + viaSQL, args: args})
		if err != nil {
			return nil, err
		}
		part, err := scanIfaceImplementsRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (s *Store) NodesByKindInRepos(kind graph.NodeKind, prefixes []string) iter.Seq[*graph.Node] {
	if prefixes == nil {
		return s.NodesByKind(kind)
	}
	return func(yield func(*graph.Node) bool) {
		ctx := s.readContext()
		for _, prefix := range sortedDeadCodeRepos(prefixes) {
			after := ""
			for {
				page, scanned, lastID, err := s.scanNodePage(ctx, deadCodeScopedFilesPageSQL, s.viewGen, prefix, after, string(kind), deadCodePageSize)
				if isNoSuchIndexErr(err) {
					page, scanned, lastID, err = s.scanNodePage(ctx, strings.ReplaceAll(deadCodeScopedFilesPageSQL, " INDEXED BY nodes_by_repo", ""), s.viewGen, prefix, after, string(kind), deadCodePageSize)
				}
				if err != nil {
					return
				}
				for _, node := range page {
					if !yield(node) {
						return
					}
				}
				if scanned < deadCodePageSize {
					break
				}
				after = lastID
			}
		}
	}
}
