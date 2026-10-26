package store_sqlite

import (
	"database/sql"
	"sync/atomic"
)

// RowWriteCounts counts directly changed rows in the four graph payload tables.
// Statement RowsAffected excludes trigger writes (including generation census
// triggers). Like VFS counters, these are process-wide activity, not committed
// cardinality: successful statements in a later rolled-back transaction count.
// INSERT ... RETURNING helpers add their returned-row census once per statement.
type RowWriteCounts struct {
	Nodes      int64 `json:"nodes"`
	Edges      int64 `json:"edges"`
	SymbolFTS  int64 `json:"symbol_fts"`
	ContentFTS int64 `json:"content_fts"`
}

func (r RowWriteCounts) Total() int64 { return r.Nodes + r.Edges + r.SymbolFTS + r.ContentFTS }
func (r RowWriteCounts) Since(p RowWriteCounts) RowWriteCounts {
	return RowWriteCounts{r.Nodes - p.Nodes, r.Edges - p.Edges, r.SymbolFTS - p.SymbolFTS, r.ContentFTS - p.ContentFTS}
}

var nodeRowsWritten, edgeRowsWritten, symbolFTSRowsWritten, contentFTSRowsWritten atomic.Int64

func rowWriteMark() RowWriteCounts {
	return RowWriteCounts{nodeRowsWritten.Load(), edgeRowsWritten.Load(), symbolFTSRowsWritten.Load(), contentFTSRowsWritten.Load()}
}
func countStatementRows(counter *atomic.Int64, result sql.Result, err error) {
	if err == nil && result != nil {
		if rows, rowsErr := result.RowsAffected(); rowsErr == nil {
			counter.Add(rows)
		}
	}
}
func countNodeWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&nodeRowsWritten, result, err)
	return result, err
}
func countEdgeWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&edgeRowsWritten, result, err)
	return result, err
}
func countSymbolFTSWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&symbolFTSRowsWritten, result, err)
	return result, err
}
func countContentFTSWrite(result sql.Result, err error) (sql.Result, error) {
	countStatementRows(&contentFTSRowsWritten, result, err)
	return result, err
}
