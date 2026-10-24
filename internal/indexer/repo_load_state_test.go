package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

func newRepoLoadTestMulti(t *testing.T) *MultiIndexer {
	t.Helper()
	cm, err := config.NewConfigManager("")
	require.NoError(t, err)
	return NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
}

func writeRepoLoadSource(t *testing.T, root string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc LoadProbe() {}\n"), 0o644))
}

// linkedWorktreeLayout lays out a main checkout and one linked worktree the
// way git does, without running git: ResolveWorktree reads only files.
func linkedWorktreeLayout(t *testing.T) (main, worktree string) {
	t.Helper()
	base := t.TempDir()
	main = filepath.Join(base, "main")
	worktree = filepath.Join(base, "wt")
	admin := filepath.Join(main, ".git", "worktrees", "wt")
	require.NoError(t, os.MkdirAll(admin, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, "pkg"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+admin+"\n"), 0o644))
	return main, worktree
}

func TestExpectRepos_RecordsConfiguredReposAsLoading(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	present := t.TempDir()
	missing := filepath.Join(t.TempDir(), "gone")

	mi.ExpectRepos([]config.RepoEntry{{Path: present}, {Path: present}, {Path: missing}})

	loads := mi.RepoLoads()
	require.Len(t, loads, 2, "a duplicate entry must not add a second record")
	rec, ok := mi.RepoLoadFor(present)
	require.True(t, ok)
	assert.Equal(t, RepoLoadQueued, rec.State)
	assert.True(t, rec.State.Loading())

	gone, ok := mi.RepoLoadFor(missing)
	require.True(t, ok)
	assert.Equal(t, RepoLoadMissing, gone.State, "a configured path that does not exist is terminal at once")
}

func TestExpectRepos_ReannouncementDropsUnconfiguredLoadingRecords(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	keep, drop := t.TempDir(), t.TempDir()
	mi.ExpectRepos([]config.RepoEntry{{Path: keep}, {Path: drop}})
	mi.StartRepoLoad(keep)

	mi.ExpectRepos([]config.RepoEntry{{Path: keep}})

	_, dropped := mi.RepoLoadFor(drop)
	assert.False(t, dropped, "an entry warmup will never load must not read as loading forever")
	rec, ok := mi.RepoLoadFor(keep)
	require.True(t, ok)
	assert.Equal(t, RepoLoadRunning, rec.State, "a re-announcement keeps an existing record's progress")
}

func TestFinishRepoLoad_EndsLoadingWithAReason(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	failed, silent, vanished := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "later-gone")
	require.NoError(t, os.MkdirAll(vanished, 0o755))
	mi.ExpectRepos([]config.RepoEntry{{Path: failed}, {Path: silent}, {Path: vanished}})
	for _, p := range []string{failed, silent, vanished} {
		mi.StartRepoLoad(p)
	}
	require.NoError(t, os.RemoveAll(vanished))

	mi.FinishRepoLoad(failed, errors.New("indexing exploded"))
	mi.FinishRepoLoad(silent, nil)
	mi.FinishRepoLoad(vanished, errors.New("path does not exist"))

	rec, _ := mi.RepoLoadFor(failed)
	assert.Equal(t, RepoLoadFailed, rec.State)
	assert.Equal(t, "indexing exploded", rec.Reason)

	rec, _ = mi.RepoLoadFor(silent)
	assert.Equal(t, RepoLoadFailed, rec.State, "a job that ends without registering is terminal, not loading")
	assert.NotEmpty(t, rec.Reason)

	rec, _ = mi.RepoLoadFor(vanished)
	assert.Equal(t, RepoLoadMissing, rec.State)
}

func TestRepoRegistration_MarksReadyAndAnnouncesOnce(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	root := filepath.Join(t.TempDir(), "probe")
	writeRepoLoadSource(t, root)
	mi.ExpectRepos([]config.RepoEntry{{Path: root}})

	var announced atomic.Int32
	got := make(chan RepoLoad, 4)
	mi.OnRepoRegistered(func(rec RepoLoad) {
		announced.Add(1)
		got <- rec
	})

	mi.StartRepoLoad(root)
	res, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: root})
	require.NoError(t, err)
	mi.FinishRepoLoad(root, nil)

	select {
	case rec := <-got:
		assert.Equal(t, RepoLoadReady, rec.State)
		assert.Equal(t, res.RepoPrefix, rec.Prefix)
	case <-time.After(5 * time.Second):
		t.Fatal("registration was never announced")
	}
	rec, _ := mi.RepoLoadFor(root)
	assert.Equal(t, RepoLoadReady, rec.State, "FinishRepoLoad must not demote a registered repository")

	// A rewrite of the existing entry is not a registration: re-indexing must
	// neither clear every session's scope nor announce ready again.
	_, err = mi.IndexRepo(res.RepoPrefix)
	require.NoError(t, err)
	time.Sleep(50 * time.Millisecond)
	assert.EqualValues(t, 1, announced.Load())
}

func TestPendingRepoForPath(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	main, worktree := linkedWorktreeLayout(t)
	registered := filepath.Join(t.TempDir(), "registered")
	writeRepoLoadSource(t, registered)
	mi.ExpectRepos([]config.RepoEntry{{Path: main}, {Path: registered}})
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: registered})
	require.NoError(t, err)

	for name, path := range map[string]string{
		"root":            main,
		"subdirectory":    filepath.Join(main, "internal", "pkg"),
		"parent of root":  filepath.Dir(main),
		"linked worktree": filepath.Join(worktree, "pkg"),
	} {
		t.Run(name, func(t *testing.T) {
			rec, pending := mi.PendingRepoForPath(path)
			require.True(t, pending)
			assert.Equal(t, RepoLoadQueued, rec.State)
			assert.Equal(t, absRepoRoot(main), rec.Root)
		})
	}

	t.Run("registered repository is not pending", func(t *testing.T) {
		_, pending := mi.PendingRepoForPath(filepath.Join(registered, "main.go"))
		assert.False(t, pending)
	})
	t.Run("unconfigured path is not pending", func(t *testing.T) {
		_, pending := mi.PendingRepoForPath(t.TempDir())
		assert.False(t, pending)
	})
	t.Run("worktree of a primary that finished loading is not pending", func(t *testing.T) {
		mi.StartRepoLoad(main)
		mi.FinishRepoLoad(main, errors.New("boom"))
		_, pending := mi.PendingRepoForPath(worktree)
		assert.False(t, pending, "only a loading primary claims its worktrees")
		rec, pending := mi.PendingRepoForPath(main)
		require.True(t, pending, "the failed root itself still reports its state")
		assert.Equal(t, RepoLoadFailed, rec.State)
	})
}

// The admission gate and the probe read the load state on every frame; it
// must answer while a topology mutation holds the registry lock.
func TestRepoLoadReadsDoNotTakeRegistryLock(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	root := t.TempDir()
	mi.ExpectRepos([]config.RepoEntry{{Path: root}})

	mi.mu.Lock()
	defer mi.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = mi.RepoLoadFor(root)
		_, _ = mi.PendingRepoForPath(filepath.Join(root, "x"))
		_ = mi.RepoLoads()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("load-state reads blocked behind MultiIndexer.mu")
	}
}

func TestForgetRepoLoad_DropsTheRecord(t *testing.T) {
	mi := newRepoLoadTestMulti(t)
	root := t.TempDir()
	mi.markRepoRegistered("probe", root)
	_, ok := mi.RepoLoadFor(root)
	require.True(t, ok)

	mi.forgetRepoLoad(root)
	_, ok = mi.RepoLoadFor(root)
	assert.False(t, ok)
}

// On a restart, seeding applies coordinators before warmup registers any
// primary, so an admitted coordinator meets an unregistered primary and is
// skipped. Its primary's registration must start it, instead of leaving it to
// the janitor an hour later.
func TestPrimaryRegisteredStartsTheCoordinatorsThatWaitedForIt(t *testing.T) {
	f := newFamilyFixture(t, "await-primary")
	defer f.close()
	ctx := context.Background()

	f.restart()
	require.NoError(t, f.lc.Seed(ctx))
	require.Nil(t, f.mi.GetMetadata(f.mainPrefix), "the restart window: the primary is bound in the catalog but not registered")
	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)

	f.lc.ensureCoordinator(ctx, f.primaryGraph, checkout)
	require.False(t, f.lc.hasCoordinator(checkout.CheckoutID), "no coordinator can start before its primary registers")

	f.lc.PrimaryRegistered("some-other-repo")
	require.False(t, f.lc.hasCoordinator(checkout.CheckoutID), "another repository's registration must not start it")

	_, err = f.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: f.main, Name: f.mainPrefix})
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	require.False(t, f.lc.hasCoordinator(checkout.CheckoutID),
		"registration alone starts nothing; the daemon's registration listener calls PrimaryRegistered")
	f.lc.PrimaryRegistered(f.mainPrefix)
	require.Eventually(t, func() bool { return f.lc.hasCoordinator(checkout.CheckoutID) },
		5*time.Second, 20*time.Millisecond, "the primary's registration must start the coordinator that waited for it")
}
