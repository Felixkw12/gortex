package store_sqlite

import (
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
		JOIN nodes AS target ON target.id = e.to_id AND target.view_gen = e.view_gen
		WHERE ` + where + ` AND ` + targetPredicate, append(append([]any(nil), baseArgs...), targetArgs...)}
	imports = outlineQuery{`SELECT COALESCE(NULLIF(n.file_path, ''), n.id) AS path, COUNT(*)` + from + ` CROSS JOIN edges AS e INDEXED BY edges_by_to
		ON e.to_id = n.id AND e.view_gen = n.view_gen AND e.kind = ?
		WHERE ` + where + ` GROUP BY path`, append([]any{string(graph.EdgeImports)}, baseArgs...)}
	return languages, edges, imports
}

func (s *Store) ScopedOutlineCounts(scope graph.NodeScope) graph.OutlineCounts {
	languages, edges, imports := outlineQueries(scope, s.viewGen)
	out := graph.OutlineCounts{ByLanguage: make(map[string]int)}
	rows, err := s.outlineRows(languages)
	panicOnFatal(err)
	if rows == nil {
		return out
	}
	for rows.Next() {
		var language string
		var count int
		panicOnFatal(rows.Scan(&language, &count))
		out.TotalNodes += count
		if language != "" {
			out.ByLanguage[language] = count
		}
	}
	panicOnFatal(rows.Err())
	_ = rows.Close()
	rows, err = s.outlineRows(edges)
	panicOnFatal(err)
	if rows == nil {
		return out
	}
	if rows.Next() {
		panicOnFatal(rows.Scan(&out.TotalEdges))
	}
	panicOnFatal(rows.Err())
	_ = rows.Close()
	rows, err = s.outlineRows(imports)
	panicOnFatal(err)
	if rows == nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var row graph.FileImportCountRow
		panicOnFatal(rows.Scan(&row.FilePath, &row.Count))
		out.FileImports = append(out.FileImports, row)
	}
	panicOnFatal(rows.Err())
	return out
}

// Cold bulk loads may temporarily drop secondary indexes. Retain exact reads
// during that window; steady-state queries pin the repo/adjacency indexes so
// stale planner statistics cannot turn a scoped outline into a global scan.
func (s *Store) outlineRows(query outlineQuery) (*sql.Rows, error) {
	rows, err := s.db.Query(query.sql, query.args...)
	if isNoSuchIndexErr(err) {
		unhinted := strings.NewReplacer(
			" INDEXED BY nodes_by_repo", "",
			" INDEXED BY edges_by_from", "",
			" INDEXED BY edges_by_to", "",
		).Replace(query.sql)
		return s.db.Query(unhinted, query.args...)
	}
	return rows, err
}

var _ graph.ScopedOutlineAggregator = (*Store)(nil)
