package indexer

import "github.com/zzet/gortex/internal/graph"

// The fixed per-file extractor list in buildPerFileContractExtractors consumes
// source and nodes, not edges. Extraction retains only these DI/Spring edges
// for the unit's enrichment seeds. Full nodes and full retained edge payloads
// remain proof inputs; common file loads and generated-core proofs stay full.
func contractUnitSeedEdge(edge *graph.Edge) bool {
	if edge == nil {
		return false
	}
	if edge.Kind == graph.EdgeProvides || edge.Kind == graph.EdgeConsumes {
		_, di := diContractFromEdge(edge)
		return di
	}
	return edge.Kind == graph.EdgeCalls && edge.Meta["via"] == "spring.Bean"
}

// loadExtractionFile is used only by ordinary per-file extraction, whose
// returned inputs are exactly those recorded here. Enrichment uses loadFile.
func (e *contractFollowupEvidence) loadExtractionFile(path string) ([]*graph.Node, []*graph.Edge, error) {
	e.ledger.owner(path)
	e.ledger.key("path:", path)
	defer e.guard.enter()()
	nodes, edges, err := e.loadFileRows(path)
	if err != nil {
		return nil, nil, err
	}
	var seeds []*graph.Edge
	for _, edge := range edges {
		if contractUnitSeedEdge(edge) {
			seeds = append(seeds, edge)
		}
	}
	e.ledger.read(contractReadExtractionFile, []string{path}, contractFileAnswer{Nodes: nodes, Edges: seeds}, path)
	return nodes, seeds, nil
}
