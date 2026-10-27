package graph

import "iter"

// ScopedDeadCodeReader restricts candidate ownership, never evidence of use:
// incoming edges and implemented interfaces can belong to any repository.
// Nil prefixes mean unscoped; a non-nil empty set matches no nodes. An empty
// ownership prefix is included only when explicitly present in the set.
// Composed readers must preserve their masks instead of forwarding this
// capability to a physical base store.
type ScopedDeadCodeReader interface {
	DeadCodeCandidatesInRepos(kinds []NodeKind, incoming map[NodeKind][]EdgeKind, prefixes []string) []*Node
	IfaceImplementsRowsInRepos(prefixes []string) []IfaceImplementsRow
	NodesByKindInRepos(kind NodeKind, prefixes []string) iter.Seq[*Node]
}

var _ ScopedDeadCodeReader = (*Graph)(nil)

func repoPrefixSet(prefixes []string) map[string]bool {
	if prefixes == nil {
		return nil
	}
	allowed := make(map[string]bool, len(prefixes))
	for _, prefix := range prefixes {
		allowed[prefix] = true
	}
	return allowed
}

func (g *Graph) NodesByKindInRepos(kind NodeKind, prefixes []string) iter.Seq[*Node] {
	allowed := repoPrefixSet(prefixes)
	return func(yield func(*Node) bool) {
		for n := range g.NodesByKind(kind) {
			if n != nil && (allowed == nil || allowed[n.RepoPrefix]) && !yield(n) {
				return
			}
		}
	}
}

func (g *Graph) DeadCodeCandidatesInRepos(kinds []NodeKind, incoming map[NodeKind][]EdgeKind, prefixes []string) []*Node {
	return g.deadCodeCandidates(kinds, incoming, prefixes)
}

func (g *Graph) IfaceImplementsRowsInRepos(prefixes []string) []IfaceImplementsRow {
	if prefixes == nil {
		return g.IfaceImplementsRows()
	}
	allowed := repoPrefixSet(prefixes)
	var out []IfaceImplementsRow
	for edge := range g.EdgesByKind(EdgeImplements) {
		if edge == nil {
			continue
		}
		typ := g.GetNode(edge.From)
		if typ == nil || !allowed[typ.RepoPrefix] {
			continue
		}
		iface := g.GetNode(edge.To)
		if iface == nil || iface.Kind != KindInterface || iface.Meta == nil {
			continue
		}
		if _, ok := iface.Meta["methods"]; ok {
			out = append(out, IfaceImplementsRow{TypeID: typ.ID, IfaceID: iface.ID, IfaceMeta: iface.Meta})
		}
	}
	return out
}
