package agents

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

// PendingAutomaticCheckout reports whether cwd is inside a Git checkout whose
// family is already represented by one of trackedRoots, but the checkout
// itself has not reached the tracked checkout registry yet. Callers must first
// establish that cwd is not already covered; this helper only proves family
// identity.
//
// Initialize and lifecycle hooks use this distinction before recommending a
// tracking action. An unrelated repository can legitimately be explicitly
// tracked; any other checkout of an existing family is discovered
// automatically and must not be converted into a dedicated graph merely
// because reconciliation has not published its route yet. This includes both
// directions: a new linked worktree beside a tracked primary, and the primary
// checkout when a linked worktree is the family member currently tracked.
//
// The probe is read-only and filesystem-only. It walks up to the nearest .git
// entry, then reuses the indexer's canonical worktree resolver; it never runs
// Git, starts a daemon, or mutates configuration.
func PendingAutomaticCheckout(cwd string, trackedRoots []string) bool {
	_, represented := AutomaticCheckoutFamily(cwd, trackedRoots)
	return represented != ""
}

// AutomaticCheckoutFamily returns the nearest Git checkout and the first
// configured or registered root representing its family. A nonempty second
// result proves only family identity; callers still decide admission using
// the registry/catalog. No Git process or configuration write is involved.
func AutomaticCheckoutFamily(cwd string, knownRoots []string) (checkout, represented string) {
	root := nearestGitRoot(cwd)
	if root == "" {
		return "", ""
	}
	candidate := indexer.ResolveWorktree(root)
	if candidate.GitCommonDir == "" {
		return root, ""
	}
	candidateCommon := canonicalFamilyPath(candidate.GitCommonDir)
	for _, trackedRoot := range knownRoots {
		trackedRoot = strings.TrimSpace(trackedRoot)
		if trackedRoot == "" {
			continue
		}
		tracked := indexer.ResolveWorktree(trackedRoot)
		if tracked.GitCommonDir == "" {
			continue
		}
		if pathkey.EqualPaths(candidateCommon, canonicalFamilyPath(tracked.GitCommonDir)) {
			return root, trackedRoot
		}
	}
	return root, ""
}

func nearestGitRoot(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	current := filepath.Clean(abs)
	if info, statErr := os.Stat(current); statErr == nil && !info.IsDir() {
		current = filepath.Dir(current)
	}
	for {
		if _, err := os.Lstat(filepath.Join(current, ".git")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

func canonicalFamilyPath(path string) string {
	cleaned := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved)
	}
	return cleaned
}
