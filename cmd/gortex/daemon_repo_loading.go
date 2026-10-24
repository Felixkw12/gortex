package main

import (
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

// announceConfiguredRepos makes every configured repository read as loading.
//
// The warmup goroutine calls it first, ahead of checkout seeding rather than
// when warmup reaches its job loop: seeding takes seconds on a large worktree
// farm, and for that whole window the admission gate otherwise told sessions
// their configured repository was untracked. It is not called before Listen:
// it stats every configured root, and a hung mount must not keep the socket
// from coming up.
func announceConfiguredRepos(state *daemonState) {
	if state == nil || state.multiIndexer == nil || state.configManager == nil {
		return
	}
	if gc := state.configManager.Global(); gc != nil {
		state.multiIndexer.ExpectRepos(gc.Repos)
	}
}

// installRepoLoadTracking wires what has to happen when a configured
// repository becomes ready.
func installRepoLoadTracking(state *daemonState, disp *mcpDispatcher) {
	if state == nil || state.multiIndexer == nil {
		return
	}
	state.multiIndexer.OnRepoRegistered(func(rec indexer.RepoLoad) {
		if state.mcpServer != nil {
			// A session that resolved its scope while the repository was not
			// registered — through the router arm, or a resources/read — holds
			// a scope that sees nothing. Drop every cached scope so the next
			// call binds to the repository, with no reconnect.
			state.mcpServer.InvalidateSessionScopes()
			state.mcpServer.PublishRepoReadiness(rec)
		}
		disp.notifyRepoReady(rec)
		if state.lifecycle != nil {
			state.lifecycle.PrimaryRegistered(rec.Prefix)
		}
	})
}

// publicRepoState is the wire spelling of a load state: warmup's queued and
// running both read as loading to a client.
func publicRepoState(state indexer.RepoLoadState) string {
	switch {
	case state.Loading():
		return daemon.RepoStateLoading
	case state.Ready():
		return daemon.RepoStateReady
	case state == indexer.RepoLoadMissing:
		return daemon.RepoStateMissing
	case state == indexer.RepoLoadFailed:
		return daemon.RepoStateFailed
	}
	return ""
}

// loadingRepoForPath names the configured repository abs belongs to when that
// repository is still loading, for the coverage replies the hooks turn into a
// deny. Lock-free; reads no store.
func (c *realController) loadingRepoForPath(abs string) (repo string, loading bool) {
	if c == nil || c.multiIndexer == nil {
		return "", false
	}
	rec, pending := c.multiIndexer.PendingRepoForPath(abs)
	if !pending || !rec.State.Loading() {
		return "", false
	}
	return rec.Label(), true
}
