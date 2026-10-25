package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// outlineScopePredicate is the SQL twin of graph.NodeScope.Allows. GLOB keeps
// the external path check case-sensitive, like strings.HasPrefix in Go.
func outlineScopePredicate(scope graph.NodeScope, alias string) (string, []any) {
	var terms []string
	var args []any
	global := `(` + alias + `.repo_prefix = '' AND ` + alias + `.workspace_id = '' AND (` + alias + `.file_path GLOB 'external::*' OR ` + alias + `.file_path GLOB 'external-call::*'))`
	if scope.WorkspaceID != "" {
		terms = append(terms, `(`+global+` OR COALESCE(NULLIF(`+alias+`.workspace_id, ''), `+alias+`.repo_prefix) = ?)`)
		args = append(args, scope.WorkspaceID)
	}
	if scope.ProjectID != "" && (scope.WorkspaceID != "" || len(scope.RepoAllow) > 0) {
		terms = append(terms, `(`+global+` OR COALESCE(NULLIF(`+alias+`.project_id, ''), `+alias+`.repo_prefix) = ?)`)
		args = append(args, scope.ProjectID)
	}
	if len(scope.RepoAllow) > 0 {
		terms = append(terms, alias+`.repo_prefix IN (SELECT CAST(value AS TEXT) FROM json_each(?))`)
		args = append(args, outlineReposJSON(scope))
	}
	if len(terms) == 0 {
		return "1", nil
	}
	return strings.Join(terms, " AND "), args
}

func outlineReposJSON(scope graph.NodeScope) string {
	// The unowned bucket is always eligible under a repo narrow. Workspace
	// and project checks still filter source nodes in that bucket.
	repos := []string{""}
	for repo, allowed := range scope.RepoAllow {
		if allowed && repo != "" {
			repos = append(repos, repo)
		}
	}
	sort.Strings(repos)
	data, _ := json.Marshal(repos)
	return string(data)
}

type outlineQuery struct {
	sql  string
	args []any
}

// outlineQueries drives edges from scoped nodes, preventing SQLite from
// choosing a whole-edge scan as the store grows. Only the selected repos and
// unowned bucket are visited through nodes_by_repo; endpoint checks use the
// node primary key in the same payload generation.
func outlineQueries(scope graph.NodeScope, generation int64) (languages, edges, imports outlineQuery) {
	return outlineFilteredQueries(scope, generation, graph.OutlineEdgeFilter{})
}

func outlineFilteredQueries(scope graph.NodeScope, generation int64, filter graph.OutlineEdgeFilter) (languages, edges, imports outlineQuery) {
	predicate, args := outlineScopePredicate(scope, "n")
	from := ` FROM nodes AS n`
	if len(scope.RepoAllow) > 0 {
		from += ` INDEXED BY nodes_by_repo`
	}
	where := `n.view_gen = ? AND ` + predicate
	baseArgs := append([]any{generation}, args...)
	languages = outlineQuery{`SELECT n.language, COUNT(*)` + from + ` WHERE ` + where + ` GROUP BY n.language`, baseArgs}

	targetPredicate, targetArgs := outlineScopePredicate(scope, "target")
	edges = outlineQuery{`SELECT COUNT(*)` + from + ` CROSS JOIN edges AS e INDEXED BY edges_by_from
		ON e.from_id = n.id AND e.view_gen = n.view_gen
		CROSS JOIN nodes AS target ON target.id = e.to_id AND target.view_gen = e.view_gen
		WHERE ` + where + ` AND ` + targetPredicate, append(append([]any(nil), baseArgs...), targetArgs...)}
	imports = outlineQuery{`SELECT COALESCE(NULLIF(n.file_path, ''), n.id) AS path, COUNT(*)` + from + ` CROSS JOIN edges AS e INDEXED BY edges_by_to
		ON e.to_id = n.id AND e.view_gen = n.view_gen AND e.kind = ?
		WHERE ` + where + ` GROUP BY path`, append([]any{string(graph.EdgeImports)}, baseArgs...)}
	edgeFilter, edgeArgs := outlineEdgePredicate(filter, "n", "target")
	edges.sql += edgeFilter
	edges.args = append(edges.args, edgeArgs...)
	importFilter, importArgs := outlineEdgePredicate(filter, "source", "n")
	if len(filter.ExcludeNodeKinds) > 0 {
		imports.sql = strings.Replace(imports.sql, "WHERE n.view_gen", "LEFT JOIN nodes AS source ON source.id = e.from_id AND source.view_gen = e.view_gen WHERE n.view_gen", 1)
	}
	imports.sql = strings.Replace(imports.sql, " GROUP BY path", importFilter+" GROUP BY path", 1)
	imports.args = append(imports.args, importArgs...)
	return languages, edges, imports
}

func outlineEdgePredicate(filter graph.OutlineEdgeFilter, from, to string) (string, []any) {
	var predicate string
	var args []any
	if len(filter.ExcludeKinds) > 0 {
		data, _ := json.Marshal(filter.ExcludeKinds)
		predicate += ` AND e.kind NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
		args = append(args, string(data))
	}
	if len(filter.ExcludeNodeKinds) > 0 {
		data, _ := json.Marshal(filter.ExcludeNodeKinds)
		for _, alias := range []string{from, to} {
			predicate += ` AND COALESCE(` + alias + `.kind, '') NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
			args = append(args, string(data))
		}
	}
	if len(filter.ExcludeNodeIDs) > 0 {
		ids := make([]string, 0, len(filter.ExcludeNodeIDs))
		for id, excluded := range filter.ExcludeNodeIDs {
			if excluded {
				ids = append(ids, id)
			}
		}
		data, _ := json.Marshal(ids)
		for _, endpoint := range []string{"from_id", "to_id"} {
			predicate += ` AND e.` + endpoint + ` NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
			args = append(args, string(data))
		}
	}
	return predicate, args
}

func (s *Store) ScopedOutlineCounts(scope graph.NodeScope) (graph.OutlineCounts, error) {
	return s.ScopedOutlineCountsFiltered(scope, graph.OutlineEdgeFilter{})
}

func (s *Store) ScopedOutlineCountsFiltered(scope graph.NodeScope, filter graph.OutlineEdgeFilter) (graph.OutlineCounts, error) {
	ctx := s.readContext()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.OutlineCounts{}, err
	}
	defer tx.Rollback()
	out, err := s.outlineCountsFrom(ctx, tx, scope, filter)
	if err != nil {
		return graph.OutlineCounts{}, err
	}
	if err := tx.Commit(); err != nil {
		return graph.OutlineCounts{}, err
	}
	return out, nil
}

type outlineQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// All totals and metadata corrections share one read snapshot. In particular,
// a concurrent writer cannot add excluded edges between counting and subtracting.
func (s *Store) outlineCountsFrom(ctx context.Context, tx outlineQueryer, scope graph.NodeScope, filter graph.OutlineEdgeFilter) (graph.OutlineCounts, error) {
	languages, edges, imports := outlineFilteredQueries(scope, s.viewGen, filter)
	out := graph.OutlineCounts{ByLanguage: make(map[string]int)}
	if err := scanOutlineQuery(ctx, tx, languages, func(rows *sql.Rows) error {
		var language string
		var count int
		if err := rows.Scan(&language, &count); err != nil {
			return err
		}
		out.TotalNodes += count
		if language != "" {
			out.ByLanguage[language] = count
		}
		return nil
	}); err != nil {
		return graph.OutlineCounts{}, err
	}
	if err := scanOutlineQuery(ctx, tx, edges, func(rows *sql.Rows) error { return rows.Scan(&out.TotalEdges) }); err != nil {
		return graph.OutlineCounts{}, err
	}
	if err := scanOutlineQuery(ctx, tx, imports, func(rows *sql.Rows) error {
		var row graph.FileImportCountRow
		if err := rows.Scan(&row.FilePath, &row.Count); err != nil {
			return err
		}
		out.FileImports = append(out.FileImports, row)
		return nil
	}); err != nil {
		return graph.OutlineCounts{}, err
	}
	if filter.ExcludeVia != "" {
		if err := subtractOutlineVia(ctx, tx, &out, edges, imports, filter.ExcludeVia); err != nil {
			return graph.OutlineCounts{}, err
		}
	}
	return out, nil
}

// Metadata is not a promoted edge column and may use flat, JSON or legacy gob
// encoding. Count ordinary edges in SQL, then decode only grouped candidates:
// flat strings contain their literal bytes; all older encodings are admitted
// conservatively (including escaped JSON). No whole-store edge read is needed.
func subtractOutlineVia(ctx context.Context, tx outlineQueryer, out *graph.OutlineCounts, edges, imports outlineQuery, via string) error {
	edges, imports = outlineViaQueries(edges, imports, via)
	counts := make(map[string]int, len(out.FileImports))
	for _, row := range out.FileImports {
		counts[row.FilePath] = row.Count
	}
	for i, query := range []outlineQuery{edges, imports} {
		if err := scanOutlineQuery(ctx, tx, query, func(rows *sql.Rows) error {
			var path string
			var meta []byte
			var count int
			var err error
			if i == 0 {
				err = rows.Scan(&meta, &count)
			} else {
				err = rows.Scan(&path, &meta, &count)
			}
			if err != nil {
				return err
			}
			excluded, err := outlineMetaVia(meta, via)
			if err != nil {
				return err
			}
			if excluded {
				if i == 0 {
					out.TotalEdges -= count
				} else {
					counts[path] -= count
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	out.FileImports = nil
	for path, count := range counts {
		if count > 0 {
			out.FileImports = append(out.FileImports, graph.FileImportCountRow{FilePath: path, Count: count})
		}
	}
	return nil
}

func outlineViaQueries(edges, imports outlineQuery, via string) (outlineQuery, outlineQuery) {
	candidate := ` AND e.meta IS NOT NULL AND length(e.meta) > 0 AND (substr(e.meta, 1, 2) <> ? OR instr(e.meta, ?) > 0)`
	args := []any{[]byte{metaFlatMagic0, metaFlatVersion}, []byte(via)}
	edges.sql = strings.Replace(edges.sql, `SELECT COUNT(*)`, `SELECT e.meta, COUNT(*)`, 1) + candidate + ` GROUP BY e.meta`
	edges.args = append(edges.args, args...)
	imports.sql = strings.Replace(imports.sql, `AS path, COUNT(*)`, `AS path, e.meta, COUNT(*)`, 1)
	imports.sql = strings.Replace(imports.sql, ` GROUP BY path`, candidate+` GROUP BY path, e.meta`, 1)
	imports.args = append(imports.args, args...)
	return edges, imports
}

func outlineMetaVia(blob []byte, via string) (bool, error) {
	if isFlatMeta(blob) {
		// The prefilter can match a different field or a longer string.
		// Read the actual top-level via value before subtracting anything.
		d := &metaDecoder{buf: blob[2:]}
		count, err := d.readCount()
		if err != nil {
			return false, err
		}
		for range count {
			key, err := d.readRawString()
			if err != nil {
				return false, err
			}
			if bytes.Equal(key, []byte("via")) {
				value, err := d.readValue()
				return value == via, err
			}
			if err := d.skipValue(); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	meta, err := decodeMeta(blob)
	return meta["via"] == via, err
}

// Cold bulk loads may temporarily drop secondary indexes. Retain exact reads
// during that window; steady-state queries pin the repo/adjacency indexes so
// stale planner statistics cannot turn a scoped outline into a global scan.
func outlineRows(ctx context.Context, tx outlineQueryer, query outlineQuery) (*sql.Rows, error) {
	rows, err := tx.QueryContext(ctx, query.sql, query.args...)
	if isNoSuchIndexErr(err) {
		unhinted := strings.NewReplacer(
			" INDEXED BY nodes_by_repo", "",
			" INDEXED BY edges_by_from", "",
			" INDEXED BY edges_by_to", "",
		).Replace(query.sql)
		return tx.QueryContext(ctx, unhinted, query.args...)
	}
	return rows, err
}

var _ graph.ScopedOutlineAggregator = (*Store)(nil)

var _ graph.FilteredScopedOutlineAggregator = (*Store)(nil)

func scanOutlineQuery(ctx context.Context, tx outlineQueryer, query outlineQuery, scan func(*sql.Rows) error) error {
	rows, err := outlineRows(ctx, tx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
