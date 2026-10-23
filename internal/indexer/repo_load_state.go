package indexer

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
)

// RepoLoadState is where one configured repository stands on its way into the
// registry after a daemon start.
//
// The registry (MultiIndexer.repos) starts empty on every boot and a repository
// enters it only when its own warmup work finishes, which on a large store is
// minutes after the socket is serving. Without this state a configured
// repository read as untracked for that whole window: sessions were told to
// re-track it, the CLI printed "add it with gortex track", and agents stopped.
// Every configured repository now reads as loading until it reaches one of the
// three terminal states, so "configured, still warming" and "nobody tracks
// this" are told apart.
type RepoLoadState string

const (
	// RepoLoadQueued is a configured repository warmup has not started yet.
	RepoLoadQueued RepoLoadState = "queued"
	// RepoLoadRunning is a configured repository warmup is indexing or
	// reconciling right now.
	RepoLoadRunning RepoLoadState = "running"
	// RepoLoadReady is a registered repository: graph tools answer for it.
	RepoLoadReady RepoLoadState = "ready"
	// RepoLoadFailed is a repository whose warmup job ended without
	// registering it. Reason says why.
	RepoLoadFailed RepoLoadState = "failed"
	// RepoLoadMissing is a configured repository whose path does not exist.
	RepoLoadMissing RepoLoadState = "missing"
)

// Loading reports a state that has not reached a terminal answer yet.
func (s RepoLoadState) Loading() bool {
	return s == RepoLoadQueued || s == RepoLoadRunning
}

// Ready reports a registered repository.
func (s RepoLoadState) Ready() bool { return s == RepoLoadReady }

// Unavailable reports a terminal state that will not become ready without
// intervention.
func (s RepoLoadState) Unavailable() bool {
	return s == RepoLoadFailed || s == RepoLoadMissing
}

// RepoLoad is one configured repository's progress into the registry.
type RepoLoad struct {
	// Root is the repository root as configured, made absolute.
	Root string `json:"root"`
	// Prefix is the repository's prefix once registered; before that it is
	// the prefix its config entry resolves to.
	Prefix string        `json:"prefix,omitempty"`
	State  RepoLoadState `json:"state"`
	// Reason explains a failed or missing repository.
	Reason string `json:"reason,omitempty"`
	// Since is when the repository entered State.
	Since time.Time `json:"since"`

	// key and canonicalKey are the lexical and symlink-resolved identity keys
	// of Root, computed once when the record is created so a snapshot rebuild
	// never touches the filesystem.
	key          string
	canonicalKey string
	// commonDir is Root's shared git directory, read once when the record is
	// created, so a linked worktree of a loading primary is recognised without
	// spawning git on the admission path.
	commonDir string
}

// Label names the repository in user-facing text: its prefix, or its root
// when no prefix is known.
func (r RepoLoad) Label() string {
	if r.Prefix != "" {
		return r.Prefix
	}
	return r.Root
}

// repoLoadTable is copy-on-write: writers serialise on mu and publish a whole
// new snapshot; readers load the pointer and never block. The admission gate
// and the probe read it on every frame, so neither may wait behind a writer,
// and neither may take MultiIndexer.mu, which topology mutations hold.
type repoLoadTable struct {
	mu        sync.Mutex
	snap      atomic.Pointer[repoLoadSnapshot]
	listeners []func(RepoLoad) // guarded by mu
}

type repoLoadSnapshot struct {
	records []RepoLoad
	// index maps both identity keys of every record to its position.
	index map[string]int
	// loading counts records in a non-terminal state, so the worktree arm
	// can skip its filesystem walk once warmup has settled.
	loading int
}

func newRepoLoadSnapshot(records []RepoLoad) *repoLoadSnapshot {
	snap := &repoLoadSnapshot{records: records, index: make(map[string]int, 2*len(records))}
	for i, rec := range records {
		snap.index[rec.key] = i
		if rec.canonicalKey != "" {
			snap.index[rec.canonicalKey] = i
		}
		if rec.State.Loading() {
			snap.loading++
		}
	}
	return snap
}

func (t *repoLoadTable) load() *repoLoadSnapshot {
	if snap := t.snap.Load(); snap != nil {
		return snap
	}
	return &repoLoadSnapshot{}
}

// find returns the position of the record whose root is path.
func (s *repoLoadSnapshot) find(path string) (int, bool) {
	if len(s.records) == 0 || path == "" {
		return 0, false
	}
	if i, ok := s.index[pathkey.IdentityKey(path)]; ok {
		return i, true
	}
	return 0, false
}

// update applies mutate to a private copy of the records and publishes it.
// mutate reports whether anything changed; an unchanged table republishes
// nothing.
func (t *repoLoadTable) update(mutate func(records []RepoLoad) ([]RepoLoad, bool)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	current := t.load()
	next, changed := mutate(slices.Clone(current.records))
	if changed {
		t.snap.Store(newRepoLoadSnapshot(next))
	}
}

// newRepoLoad builds a record for root, reading the filesystem for its
// canonical spelling and git common directory. Called only off the hot path:
// when a start announces its repositories and when one registers.
func newRepoLoad(root, prefix string, state RepoLoadState, now time.Time) RepoLoad {
	rec := RepoLoad{Root: root, Prefix: prefix, State: state, Since: now, key: pathkey.IdentityKey(root)}
	if canonical := pathkey.CanonicalExistingRoot(root); canonical != "" {
		if key := pathkey.IdentityKey(canonical); key != rec.key {
			rec.canonicalKey = key
		}
	}
	if state.Loading() {
		if commonDir := ResolveWorktree(root).GitCommonDir; commonDir != "" {
			rec.commonDir = pathkey.CanonicalExistingRoot(commonDir)
		}
	}
	return rec
}

// absRepoRoot spells a configured path the way registration does.
func absRepoRoot(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	return pathkey.NormalizeVolume(abs)
}

// ExpectRepos records the configured repositories a start is about to load.
//
// Called once before checkout seeding, so a configured repository reads as
// loading from the moment the socket serves, and again when warmup takes its
// own snapshot of the config. A repository already recorded keeps its record;
// one that is no longer configured and not registered is dropped, so a stale
// entry can never read as loading forever.
func (mi *MultiIndexer) ExpectRepos(entries []config.RepoEntry) {
	if mi == nil {
		return
	}
	now := time.Now()
	mi.repoLoads.update(func(records []RepoLoad) ([]RepoLoad, bool) {
		current := newRepoLoadSnapshot(records)
		wanted := make(map[int]bool, len(entries))
		addedKeys := make(map[string]bool)
		var added []RepoLoad
		for _, entry := range entries {
			root := absRepoRoot(entry.Path)
			if root == "" || addedKeys[pathkey.IdentityKey(root)] {
				continue
			}
			if i, ok := current.find(root); ok {
				wanted[i] = true
				continue
			}
			state := RepoLoadQueued
			reason := ""
			if info, err := os.Stat(root); err != nil || !info.IsDir() {
				state, reason = RepoLoadMissing, "the configured path does not exist"
			}
			rec := newRepoLoad(root, config.ResolvePrefix(entry), state, now)
			rec.Reason = reason
			added = append(added, rec)
			addedKeys[rec.key] = true
		}
		next := make([]RepoLoad, 0, len(records)+len(added))
		for i, rec := range records {
			if wanted[i] || rec.State == RepoLoadReady {
				next = append(next, rec)
			}
		}
		next = append(next, added...)
		return next, len(added) > 0 || len(next) != len(records)
	})
}

// StartRepoLoad marks a queued repository as being worked on by warmup.
func (mi *MultiIndexer) StartRepoLoad(path string) {
	mi.setRepoLoad(path, func(rec *RepoLoad) bool {
		if rec.State != RepoLoadQueued {
			return false
		}
		rec.State, rec.Since = RepoLoadRunning, time.Now()
		return true
	})
}

// FinishRepoLoad records the end of a repository's warmup job. A job that
// registered the repository already moved it to ready; any other ending is
// terminal — missing when the path is gone, failed otherwise — so loading
// never outlives the job that was supposed to end it.
func (mi *MultiIndexer) FinishRepoLoad(path string, jobErr error) {
	if mi == nil {
		return
	}
	root := absRepoRoot(path)
	snap := mi.repoLoads.load()
	i, ok := snap.find(root)
	if !ok || !snap.records[i].State.Loading() {
		return
	}
	rec := snap.records[i]
	// The job may have found the repository registered under another spelling
	// of the same directory, which is a success the registration hook could
	// not attribute to this record.
	if prefix, registered := mi.registeredPrefixForRoot(root); registered {
		mi.markRepoRegistered(prefix, root)
		return
	}
	state, reason := RepoLoadFailed, repoLoadFailureReason(jobErr, rec.Prefix, mi.rootOfPrefix(rec.Prefix))
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		state, reason = RepoLoadMissing, "the configured path does not exist"
	}
	mi.setRepoLoad(root, func(rec *RepoLoad) bool {
		if !rec.State.Loading() {
			return false
		}
		rec.State, rec.Reason, rec.Since = state, reason, time.Now()
		return true
	})
}

// repoLoadFailureReason states why a warmup job ended without registering.
func repoLoadFailureReason(jobErr error, prefix, prefixOwner string) string {
	switch {
	case jobErr != nil:
		return jobErr.Error()
	case prefixOwner != "":
		return "prefix_taken: the prefix " + prefix + " is already registered to " + prefixOwner
	default:
		return "warmup finished without registering this repository"
	}
}

// registeredPrefixForRoot finds a registered repository at root. It takes
// mu, so it is for the warmup worker only, never the admission path.
func (mi *MultiIndexer) registeredPrefixForRoot(root string) (string, bool) {
	for prefix, meta := range mi.AllMetadata() {
		if meta != nil && pathkey.EqualPaths(meta.RootPath, root) {
			return prefix, true
		}
	}
	return "", false
}

// rootOfPrefix is the root registered under prefix, or "".
func (mi *MultiIndexer) rootOfPrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	if meta := mi.GetMetadata(prefix); meta != nil {
		return meta.RootPath
	}
	return ""
}

func (mi *MultiIndexer) setRepoLoad(path string, mutate func(*RepoLoad) bool) {
	if mi == nil {
		return
	}
	root := absRepoRoot(path)
	mi.repoLoads.update(func(records []RepoLoad) ([]RepoLoad, bool) {
		i, ok := newRepoLoadSnapshot(records).find(root)
		if !ok {
			return records, false
		}
		return records, mutate(&records[i])
	})
}

// OnRepoRegistered adds a listener for a repository's first registration in
// this process. Listeners run on their own goroutine, after the repository is
// already visible in the registry and recorded ready, so a listener that
// invalidates cached session scopes can never be overtaken by a resolution
// that still saw the repository as loading.
func (mi *MultiIndexer) OnRepoRegistered(fn func(RepoLoad)) {
	if mi == nil || fn == nil {
		return
	}
	mi.repoLoads.mu.Lock()
	mi.repoLoads.listeners = append(mi.repoLoads.listeners, fn)
	mi.repoLoads.mu.Unlock()
}

// markRepoRegistered records that prefix became visible in the registry at
// root and announces it. Called from the two places a repository enters the
// registry for the first time (cold track and warm reconcile), never from
// the rewrites a re-index or a file save makes to an existing entry: those
// would clear every session's scope and announce "ready" on each edit.
func (mi *MultiIndexer) markRepoRegistered(prefix, root string) {
	if mi == nil || root == "" {
		return
	}
	now := time.Now()
	var announced RepoLoad
	var listeners []func(RepoLoad)
	mi.repoLoads.update(func(records []RepoLoad) ([]RepoLoad, bool) {
		listeners = slices.Clone(mi.repoLoads.listeners)
		if i, ok := newRepoLoadSnapshot(records).find(root); ok {
			records[i].State, records[i].Reason, records[i].Since = RepoLoadReady, "", now
			records[i].commonDir = ""
			if prefix != "" {
				records[i].Prefix = prefix
			}
			announced = records[i]
			return records, true
		}
		announced = newRepoLoad(root, prefix, RepoLoadReady, now)
		return append(records, announced), true
	})
	if len(listeners) == 0 {
		return
	}
	go func() {
		for _, fn := range listeners {
			fn(announced)
		}
	}()
}

// forgetRepoLoad drops root's record once the repository leaves the registry
// through an untrack, so it reads neither as ready nor as loading.
func (mi *MultiIndexer) forgetRepoLoad(root string) {
	if mi == nil || root == "" {
		return
	}
	mi.repoLoads.update(func(records []RepoLoad) ([]RepoLoad, bool) {
		i, ok := newRepoLoadSnapshot(records).find(root)
		if !ok {
			return records, false
		}
		return slices.Delete(records, i, i+1), true
	})
}

// RepoLoads returns every recorded repository, in the order they were
// announced. Lock-free.
func (mi *MultiIndexer) RepoLoads() []RepoLoad {
	if mi == nil {
		return nil
	}
	return slices.Clone(mi.repoLoads.load().records)
}

// RepoLoadFor returns the record whose root is exactly path. Lock-free; reads
// no store.
func (mi *MultiIndexer) RepoLoadFor(path string) (RepoLoad, bool) {
	if mi == nil {
		return RepoLoad{}, false
	}
	snap := mi.repoLoads.load()
	i, ok := snap.find(absRepoRoot(path))
	if !ok {
		return RepoLoad{}, false
	}
	return snap.records[i], true
}

// PendingRepoForPath returns the configured, not-yet-registered repository
// that path belongs to: path is inside its root, or is a root above it, or is
// inside a linked worktree of a repository that is still loading. A path that
// belongs to no such repository — every configured repository it touches is
// ready, or none is configured — reports false.
//
// It reads only the lock-free snapshot and, for the worktree arm, the
// filesystem: never mu and never the store. Callers on the MCP admission path
// consult it only after the registry has failed to place path.
func (mi *MultiIndexer) PendingRepoForPath(path string) (RepoLoad, bool) {
	if mi == nil || path == "" {
		return RepoLoad{}, false
	}
	snap := mi.repoLoads.load()
	if len(snap.records) == 0 {
		return RepoLoad{}, false
	}
	abs := absRepoRoot(path)
	if rec, ok := snap.enclosing(abs); ok {
		return rec, !rec.State.Ready()
	}
	if canonical := pathkey.CanonicalPath(abs); canonical != abs {
		if rec, ok := snap.enclosing(canonical); ok {
			return rec, !rec.State.Ready()
		}
	}
	if rec, ok := snap.contained(abs); ok {
		return rec, true
	}
	if snap.loading == 0 {
		return RepoLoad{}, false
	}
	commonDir := enclosingWorktreeCommonDir(abs)
	if commonDir == "" {
		return RepoLoad{}, false
	}
	for _, rec := range snap.records {
		if rec.State.Loading() && rec.commonDir != "" && pathkey.EqualPaths(rec.commonDir, commonDir) {
			return rec, true
		}
	}
	return RepoLoad{}, false
}

// enclosing finds the nearest recorded root at or above path.
func (s *repoLoadSnapshot) enclosing(path string) (RepoLoad, bool) {
	for dir := filepath.Clean(path); ; {
		if i, ok := s.index[pathkey.IdentityKey(dir)]; ok {
			return s.records[i], true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return RepoLoad{}, false
		}
		dir = parent
	}
}

// contained finds a not-ready recorded root below path — the shape of an
// agent opened at the directory above its repositories. A loading root wins
// over an unavailable one, because retrying is the useful answer while any
// repository under path can still become ready.
func (s *repoLoadSnapshot) contained(path string) (RepoLoad, bool) {
	var found RepoLoad
	ok := false
	for _, rec := range s.records {
		if rec.State.Ready() || pathkey.EqualPaths(rec.Root, path) || !pathkey.HasPathPrefix(rec.Root, path) {
			continue
		}
		if rec.State.Loading() {
			return rec, true
		}
		if !ok {
			found, ok = rec, true
		}
	}
	return found, ok
}

// enclosingWorktreeCommonDir returns the shared git directory of the linked
// worktree path sits in, or "" when it sits in none. The nearest `.git` above
// path decides: a main checkout or a submodule is not a linked worktree.
func enclosingWorktreeCommonDir(path string) string {
	for dir := filepath.Clean(path); ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			info := ResolveWorktree(dir)
			if !info.IsWorktree || info.GitCommonDir == "" {
				return ""
			}
			// Git writes a worktree's gitdir with symlinks resolved while a
			// configured root keeps the spelling it was tracked with, so both
			// sides of the comparison are canonical.
			return pathkey.CanonicalExistingRoot(info.GitCommonDir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
