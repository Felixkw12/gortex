package indexer

import (
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/excludes"
)

// watcherExcludeFilter adapts the indexer's exclude matcher to the file
// watcher's PathFilter interface, so events under ignored trees are
// dropped inside the backend rather than crossing the event channel, the
// aggregator and the storm counter first.
//
// It is an optimisation, not a security barrier: handleEvent still runs
// the full isExcluded check, which additionally refuses symlinks that
// escape the repo root. This filter deliberately does no I/O so it stays
// cheap enough to run on every raw event.
type watcherExcludeFilter struct{ w *Watcher }

// ShouldInclude reports whether the backend should deliver an event for
// path. A watcher with no matcher includes everything.
func (f *watcherExcludeFilter) ShouldInclude(path string) bool {
	// Handshake probes are the watcher's own liveness signal, not repository
	// content, and the startup barrier writes them into <root>/.git so they
	// stay out of git status. Excluding them here would drop the very event
	// Start is waiting for and hang the barrier until it times out. Every
	// downstream consumer (mergeInitialReplayEvent, the barrier tail loop,
	// handleEvent) already discards probe paths, so admitting them costs one
	// channel send and never reaches the indexer.
	if isProbeMarkerPath(path) {
		return true
	}
	if f == nil || f.w == nil || f.w.excludes == nil {
		return true
	}
	root := f.w.indexer.rootPath
	if root == "" {
		return !f.w.excludes.MatchRel(path)
	}
	rel, err := filepath.Rel(root, path)
	// Defer agent-config files and potentially re-included subtrees to the
	// full check, without statting paths in this backend hot path.
	if err == nil && excludes.InAgentConfigDir(rel) {
		return true
	}
	if !f.w.excludes.MatchAbs(path, root) {
		return true
	}
	return err == nil && f.w.excludes.HasAnchoredNegatedDescendant(filepath.ToSlash(rel))
}

// isProbeMarkerPath reports whether path names a watcher handshake probe.
// Single definition so the backend filter and every event-loop consumer
// agree on what a probe looks like.
func isProbeMarkerPath(path string) bool {
	return strings.Contains(filepath.Base(path), probeMarker)
}
