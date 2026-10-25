package graph

// NodeScope is the workspace, project and repository boundary shared by
// query traversal and backend outline aggregation. A project-only scope is
// intentionally inactive; empty repository ownership passes a repo narrow.
type NodeScope struct {
	WorkspaceID string
	ProjectID   string
	RepoAllow   map[string]bool
}

// Allows reports whether n passes the boundary, including the legacy ownership
// fallbacks and the synthetic global-external carve-out.
func (s NodeScope) Allows(n *Node) bool {
	if n == nil {
		return true
	}
	// A synthetic global external (the type checker's `ext::go:fmt::Errorf`
	// at `external::go:fmt`: no repository, no workspace, a path that is no
	// repository source) is visible from every scope by construction, as the
	// repo narrow below already treats it. An unowned node at a source path
	// stays subject to the workspace check.
	if s.WorkspaceID != "" && !isGlobalExternalNode(n) {
		ws := n.WorkspaceID
		if ws == "" {
			ws = n.RepoPrefix
		}
		if ws != s.WorkspaceID {
			return false
		}
	}
	// A repository narrow disambiguates a project without pretending that
	// the repository is its own workspace. Unqualified project-only options
	// keep their existing behavior; synthetic global externals stay visible.
	if s.ProjectID != "" && (s.WorkspaceID != "" || len(s.RepoAllow) > 0) && !isGlobalExternalNode(n) {
		proj := n.ProjectID
		if proj == "" {
			proj = n.RepoPrefix
		}
		if proj != s.ProjectID {
			return false
		}
	}
	// A node with an empty RepoPrefix is not owned by any repository, so a
	// repo narrow can only ever be satisfied vacuously — the RepoAllow keys
	// are registry prefixes, which such nodes never carry. Admit it: the
	// workspace / project checks above still bound it for scoped sessions.
	//
	// Two populations land here. Synthetic global externals (dep::,
	// external::, non-Go module::) model third-party symbols owned by no
	// repo and are visible from every scope by construction — that is the
	// durable reason. Nodes minted in single-repo (unprefixed) mode are the
	// historical one, and go away once prefixing is unconditional.
	//
	// This is the same carve-out the MCP layer's filterNodes / field-query /
	// API-impact filters apply. Every repo-narrow predicate in the codebase
	// must agree on it: one that rejects instead of admitting turns a scoped
	// query into an empty result rather than a narrower one.
	if len(s.RepoAllow) > 0 && n.RepoPrefix != "" && !s.RepoAllow[n.RepoPrefix] {
		return false
	}
	return true
}

func isGlobalExternalNode(n *Node) bool {
	return n.RepoPrefix == "" && n.WorkspaceID == "" && n.FilePath != "" &&
		!IsAuditableRepoSourcePath(n.FilePath)
}

// OutlineCounts contains the scalar summary and target-file import counts.
// Imports are scoped only at the target, including imports from outside scope.
type OutlineCounts struct {
	TotalNodes  int
	ByLanguage  map[string]int
	TotalEdges  int
	FileImports []FileImportCountRow
}

// ScopedOutlineAggregator avoids materializing every node and edge to produce
// an outline. Overlay views deliberately omit it so unsaved edits use the
// reader-based fallback.
type ScopedOutlineAggregator interface {
	ScopedOutlineCounts(NodeScope) OutlineCounts
}

// ScopedOutlineCounts is the in-memory reference implementation.
func (g *Graph) ScopedOutlineCounts(scope NodeScope) OutlineCounts {
	out := OutlineCounts{ByLanguage: make(map[string]int)}
	allowed := make(map[string]bool)
	for _, n := range g.AllNodes() {
		if scope.Allows(n) {
			allowed[n.ID] = true
			out.TotalNodes++
			if n.Language != "" {
				out.ByLanguage[n.Language]++
			}
		}
	}
	imports := make(map[string]int)
	for _, e := range g.AllEdges() {
		if allowed[e.From] && allowed[e.To] {
			out.TotalEdges++
		}
		if e.Kind != EdgeImports || !allowed[e.To] {
			continue
		}
		n := g.GetNode(e.To)
		path := n.FilePath
		if path == "" {
			path = n.ID
		}
		imports[path]++
	}
	for path, count := range imports {
		out.FileImports = append(out.FileImports, FileImportCountRow{FilePath: path, Count: count})
	}
	return out
}

var _ ScopedOutlineAggregator = (*Graph)(nil)
