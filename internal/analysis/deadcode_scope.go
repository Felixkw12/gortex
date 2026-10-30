package analysis

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

func deadCodeRepoSet(prefixes []string) map[string]bool {
	if prefixes == nil {
		return nil
	}
	allowed := make(map[string]bool, len(prefixes))
	for _, prefix := range prefixes {
		allowed[prefix] = true
	}
	return allowed
}

func deadCodeRepoAllows(node *graph.Node, allowed map[string]bool) bool {
	return node != nil && (allowed == nil || allowed[node.RepoPrefix])
}

func filterDeadCodeRepos(nodes []*graph.Node, prefixes []string) []*graph.Node {
	if prefixes == nil {
		return nodes
	}
	allowed := deadCodeRepoSet(prefixes)
	var out []*graph.Node
	for _, node := range nodes {
		if deadCodeRepoAllows(node, allowed) {
			out = append(out, node)
		}
	}
	return out
}

func filterDeadCodeNodeScope(nodes []*graph.Node, scope *graph.NodeScope) []*graph.Node {
	if scope == nil {
		return nodes
	}
	var out []*graph.Node
	for _, n := range nodes {
		if n != nil && scope.Allows(n) {
			out = append(out, n)
		}
	}
	return out
}

func deadCodeFileNodes(g graph.Reader, prefixes []string, scope *graph.NodeScope) iter.Seq[*graph.Node] {
	if scope != nil {
		if scoped, ok := g.(graph.NodeScopedDeadCodeReader); ok {
			return scoped.NodesByKindInScope(graph.KindFile, prefixes, *scope)
		}
	}
	if scoped, ok := g.(graph.ScopedDeadCodeReader); ok {
		return func(yield func(*graph.Node) bool) {
			for n := range scoped.NodesByKindInRepos(graph.KindFile, prefixes) {
				if n != nil && (scope == nil || scope.Allows(n)) && !yield(n) {
					return
				}
			}
		}
	}
	allowed := deadCodeRepoSet(prefixes)
	return func(yield func(*graph.Node) bool) {
		for node := range g.NodesByKind(graph.KindFile) {
			if deadCodeRepoAllows(node, allowed) && (scope == nil || scope.Allows(node)) && !yield(node) {
				return
			}
		}
	}
}

func scopedIfaceRequiredMethods(g graph.Reader, prefixes []string, scope *graph.NodeScope) map[string]map[string]bool {
	if scope != nil {
		if scoped, ok := g.(graph.NodeScopedDeadCodeReader); ok {
			return buildIfaceRequiredMethodsFromRows(scoped.IfaceImplementsRowsInScope(prefixes, *scope))
		}
	}
	if scoped, ok := g.(graph.ScopedDeadCodeReader); ok {
		return buildIfaceRequiredMethodsFromRows(scoped.IfaceImplementsRowsInRepos(prefixes))
	}
	// Legacy/composed readers retain the complete interface relation. Only
	// scoped candidates consume it, so foreign implementing types are inert.
	return buildIfaceRequiredMethods(g)
}
