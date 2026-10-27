package store_sqlite

import (
	"context"
	"iter"
	"slices"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ScopedDeadCodeReader = (*Store)(nil)

func sortedDeadCodeRepos(prefixes []string) []string {
	if prefixes == nil {
		return nil
	}
	out := slices.Clone(prefixes)
	slices.Sort(out)
	return slices.Compact(out)
}

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
	var out []*graph.Node
	for _, kind := range kinds {
		var candidates []*graph.Node
		allowed := anaDedupeEdgeKinds(incoming[kind])
		q := `SELECT n.id, ` + deadCodeIncomingProbeSQL(len(allowed)) + `
FROM nodes AS n INDEXED BY nodes_by_repo
WHERE n.view_gen = ? AND n.repo_prefix = ? AND n.id > ? AND n.kind = ? ORDER BY n.id LIMIT ?`
		for _, prefix := range sortedDeadCodeRepos(prefixes) {
			after := ""
			for {
				if err := ctx.Err(); err != nil {
					return append(out, candidates...), err
				}
				args := make([]any, 0, len(allowed)+5)
				for _, edgeKind := range allowed {
					args = append(args, string(edgeKind))
				}
				args = append(args, s.viewGen, prefix, after, string(kind), deadCodePageSize)
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
	if prefixes == nil {
		q := `SELECT e.from_id, n.id, n.meta FROM edges e
JOIN nodes n ON n.id = e.to_id AND n.view_gen = e.view_gen
WHERE e.kind = ? AND n.kind = ? AND n.meta IS NOT NULL AND e.view_gen = ?`
		rows, err := s.db.QueryContext(ctx, q, string(graph.EdgeImplements), string(graph.KindInterface), s.viewGen)
		if err != nil {
			return nil, err
		}
		return scanIfaceImplementsRows(rows)
	}
	var out []graph.IfaceImplementsRow
	for _, prefix := range sortedDeadCodeRepos(prefixes) {
		q := `SELECT e.from_id, n.id, n.meta
FROM nodes AS implementing INDEXED BY nodes_by_repo
CROSS JOIN edges e INDEXED BY edges_by_from ON e.from_id = implementing.id AND e.view_gen = implementing.view_gen AND e.kind = ?
JOIN nodes n ON n.id = e.to_id AND n.view_gen = e.view_gen
WHERE implementing.view_gen = ? AND implementing.repo_prefix = ? AND n.kind = ? AND n.meta IS NOT NULL`
		rows, err := s.db.QueryContext(ctx, q, string(graph.EdgeImplements), s.viewGen, prefix, string(graph.KindInterface))
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
				q := `SELECT ` + lookupNodeCols + ` FROM nodes INDEXED BY nodes_by_repo
WHERE view_gen = ? AND repo_prefix = ? AND id > ? AND kind = ? ORDER BY id LIMIT ?`
				page, scanned, lastID, err := s.scanNodePage(ctx, q, s.viewGen, prefix, after, string(kind), deadCodePageSize)
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
