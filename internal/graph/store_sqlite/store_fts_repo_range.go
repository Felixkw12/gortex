package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// Keep the dense base-repository sort outside FTS5's internal rank stream:
// generation and repository rejection happens before scoring retained rows.
// BM25 still uses the shared corpus; rowid retains the rank stream's tie order.
const symbolRepoDenseSpanQuery = `SELECT symbol_fts.node_id, bm25(symbol_fts)
FROM symbol_fts
CROSS JOIN symbol_fts_rowid INDEXED BY symbol_fts_rowid_by_rowid
WHERE symbol_fts MATCH ?
  AND symbol_fts.rowid BETWEEN ? AND ?
  AND symbol_fts_rowid.fts_rowid = symbol_fts.rowid
  AND CASE WHEN symbol_fts_rowid.view_gen = ?
           THEN symbol_fts.repo_prefix IN ('', ?) ELSE 0 END = 1
  AND symbol_fts.rank MATCH 'bm25()'
ORDER BY bm25(symbol_fts), symbol_fts.rowid LIMIT ?`

func (s *Store) searchSymbolRepoSpanPlan(ctx context.Context, match, repo string, limit int) ([]graph.SymbolHit, bool, error) {
	return s.searchSymbolRepoSpanSnapshot(ctx, match, repo, limit, nil)
}

// Both the ownership extrema and the rank cursor use one read snapshot. Base
// rows are mutable: reusing bounds outside this transaction could omit a newly
// accepted document. afterProbe is used only by the snapshot regression test.
func (s *Store) searchSymbolRepoSpanSnapshot(ctx context.Context, match, repo string, limit int, afterProbe func(*sql.Tx) error) ([]graph.SymbolHit, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, true, err
	}
	defer func() { _ = tx.Rollback() }()
	span := symbolFTSSpan{generation: s.viewGen}
	// Indexed extrema establish complete bounds for both admitted ownership
	// buckets without enumerating their rows. The same snapshot ranks below.
	for _, owner := range []string{"", repo} {
		for _, order := range []string{"ASC", "DESC"} {
			var rowid int64
			err = tx.QueryRowContext(ctx, `SELECT fts_rowid FROM symbol_fts_rowid
WHERE view_gen = ? AND repo_prefix = ? ORDER BY fts_rowid `+order+` LIMIT 1`, s.viewGen, owner).Scan(&rowid)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return nil, true, err
			}
			if !span.present || rowid < span.lo {
				span.lo = rowid
			}
			if !span.present || rowid > span.hi {
				span.hi = rowid
			}
			span.present = true
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, true, err
	}
	if span.empty() {
		return nil, true, ctx.Err()
	}
	if err = tx.QueryRowContext(ctx, `SELECT fts_rowid FROM symbol_fts_rowid ORDER BY fts_rowid DESC LIMIT 1`).Scan(&span.tableMax); err != nil {
		return nil, true, err
	}
	query := symbolFTSSpanQuery(1, true)
	args := []any{match, span.lo, span.hi, span.generation, repo, limit}
	if !span.dense() {
		// Only the base single-repository door uses this plan. A complete
		// small ownership set can filter one MATCH cursor before it reads
		// foreign content; it must not become repeated rowid point queries.
		if s.viewGen != baseViewGeneration {
			return nil, false, nil
		}
		ids, complete, readErr := symbolRepoMembershipIDs(ctx, tx, s.viewGen, repo)
		if readErr != nil {
			return nil, true, readErr
		}
		if !complete {
			return nil, false, nil
		}
		if len(ids) == 0 {
			return nil, true, ctx.Err()
		}
		query = symbolRepoMembershipQuery(len(ids))
		args = []any{s.viewGen, match}
		for _, id := range ids {
			args = append(args, id)
		}
		args = append(args, repo, limit)
	} else if s.viewGen == baseViewGeneration {
		query = symbolRepoDenseSpanQuery
	}
	if afterProbe != nil {
		if err = afterProbe(tx); err != nil {
			return nil, true, err
		}
	}
	// Generation/repository ownership still decides membership in both plans,
	// and BM25 continues to use the complete shared table.
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	var hits []graph.SymbolHit
	for rows.Next() {
		var id string
		var score float64
		if err = rows.Scan(&id, &score); err != nil {
			return nil, true, err
		}
		if id != "" {
			hits = append(hits, graph.SymbolHit{NodeID: id, Score: -score})
		}
	}
	if err = rows.Err(); err != nil {
		return nil, true, err
	}
	if err = ctx.Err(); err != nil {
		return nil, true, err
	}
	return hits, true, nil
}
