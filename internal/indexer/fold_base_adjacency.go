package indexer

import (
	"unsafe"

	"github.com/zzet/gortex/internal/graph"
)

// This memo belongs to one verification chunk. Both rendered sides share the
// same leased immutable base. It is never used for mutable corpus generation
// zero, and reset drops every entry before the next chunk. Misses and oversized
// answers keep the original full adjacency read; no read limit is changed.
const foldBaseAdjacencyBytes = 4 << 20

type foldBaseAdjacencyEntry struct {
	rows    []*graph.Edge
	present bool
}

type foldBaseAdjacency struct {
	graph.Reader
	outgoing, incoming    map[string]foldBaseAdjacencyEntry
	used, entries, budget int
}

func newFoldBaseAdjacency(base LayerBase) *foldBaseAdjacency {
	b, ok := base.(commitLayerBase)
	if !ok || b.cloneCorpusBase != nil || len(b.stack) == 0 {
		return nil
	}
	for _, generation := range b.stack {
		if generation <= 0 {
			return nil
		}
	}
	return &foldBaseAdjacency{Reader: base, budget: foldBaseAdjacencyBytes}
}

func (b *foldBaseAdjacency) Unwrap() graph.Reader { return b.Reader }

func (b *foldBaseAdjacency) reset() {
	b.outgoing, b.incoming = nil, nil
	b.used, b.entries = 0, 0
}

func (b *foldBaseAdjacency) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return b.read(ids, false)
}
func (b *foldBaseAdjacency) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return b.read(ids, true)
}

func (b *foldBaseAdjacency) read(ids []string, incoming bool) map[string][]*graph.Edge {
	cache := b.outgoing
	if incoming {
		cache = b.incoming
	}
	var missing []string
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := cache[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(seen) == 0 {
		if incoming {
			return b.Reader.GetInEdgesByNodeIDs(ids)
		}
		return b.Reader.GetOutEdgesByNodeIDs(ids)
	}
	var fresh map[string][]*graph.Edge
	if len(missing) > 0 {
		if incoming {
			fresh = b.Reader.GetInEdgesByNodeIDs(missing)
		} else {
			fresh = b.Reader.GetOutEdgesByNodeIDs(missing)
		}
	}
	out := make(map[string][]*graph.Edge, len(seen))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if entry, ok := cache[id]; ok {
			if entry.present {
				out[id] = entry.rows
			}
			continue
		}
		rows, present := fresh[id]
		if present {
			out[id] = rows
		}
		if !seen[id] {
			continue
		}
		delete(seen, id)
		// Retain only the fields used by baseEdgeVisible and renderFoldEdge.
		// The first full read still decodes/validates all original rows. No caller
		// outside this verification receives this query-local scalar projection.
		cost := len(id) + 64
		for _, edge := range rows {
			cost += int(unsafe.Sizeof(graph.Edge{}))
			if edge != nil {
				cost += len(edge.From) + len(edge.To) + len(edge.Kind) + len(edge.FilePath) + len(edge.Origin)
			}
			if cost > b.budget-b.used {
				break
			}
		}
		if cost > b.budget-b.used || b.entries >= graph.ContractProjectionRowLimit {
			continue
		}
		entry := foldBaseAdjacencyEntry{present: present}
		if rows != nil {
			entry.rows = make([]*graph.Edge, 0, len(rows))
			for _, edge := range rows {
				if edge == nil {
					entry.rows = append(entry.rows, nil)
					continue
				}
				entry.rows = append(entry.rows, &graph.Edge{From: edge.From, To: edge.To, Kind: edge.Kind, FilePath: edge.FilePath, Line: edge.Line, Confidence: edge.Confidence, Origin: edge.Origin})
			}
		}
		if cache == nil {
			cache = map[string]foldBaseAdjacencyEntry{}
		}
		cache[id] = entry
		b.used += cost
		b.entries++
	}
	if incoming {
		b.incoming = cache
	} else {
		b.outgoing = cache
	}
	return out
}
