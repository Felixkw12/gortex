package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestByteHandoffReadyIdleKeepsOrdinaryAuthority(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	before := f.route()
	require.Equal(t, store_sqlite.RouteActive, before.State)
	require.Positive(t, before.CommitGenerationID)
	require.Positive(t, before.DirtyGenerationID)
	c.byteAnchorMu.Lock()
	anchor := c.byteAnchor
	c.byteAnchorMu.Unlock()
	held := f.leases.Held()
	m, err := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
	require.Nil(t, m)
	require.ErrorIs(t, err, ErrCheckoutByteAnchorUnavailable)
	require.Equal(t, before, f.route())
	require.Equal(t, held, f.leases.Held())
	c.byteAnchorMu.Lock()
	unchangedAnchor := anchor == c.byteAnchor
	c.byteAnchorMu.Unlock()
	require.True(t, unchangedAnchor, "an idle decline must not capture an anchor")
	c.byteMu.Lock()
	attempt, demand := c.byteAttempt, c.byteDemand
	c.byteMu.Unlock()
	require.Nil(t, attempt)
	require.Zero(t, demand)
	c.mu.Lock()
	admissions := c.sourceMutations
	c.mu.Unlock()
	require.Zero(t, admissions)

	// The decline grants nothing: ordinary authority still checks the exact
	// route and fresh whole checkout before a real selected-WT mutation.
	path := filepath.Join(f.worktree, "helper.go")
	primary, err := os.ReadFile(filepath.Join(f.primary, "helper.go"))
	require.NoError(t, err)
	m, err = l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	require.NoError(t, err)
	defer m.Close()
	require.Nil(t, m.byteAnchor)
	require.NoError(t, m.Prepare(t.Context()))
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\nfunc IdleOrdinaryHelper() {}\n")
	ticket, err := m.EnqueueRefresh(t.Context(), path)
	require.NoError(t, err)
	m.Close()
	c.cycle(t.Context())
	result := awaitCheckoutRefresh(t, ticket)
	require.NoError(t, result.Err)
	require.True(t, result.Reindexed)
	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(disk), "IdleOrdinaryHelper")
	unchanged, err := os.ReadFile(filepath.Join(f.primary, "helper.go"))
	require.NoError(t, err)
	require.Equal(t, primary, unchanged)
	sample, err := c.sampler.Sample(t.Context())
	require.NoError(t, err)
	row, found := f.generation(f.route().DirtyGenerationID)
	require.True(t, found)
	require.Equal(t, sample.Fingerprint, row.LowerViewFingerprint)
}

func TestByteHandoffRetainsPendingAttemptAndDemand(t *testing.T) {
	for _, kind := range []string{"pending", "registered attempt", "existing demand"} {
		t.Run(kind, func(t *testing.T) {
			f, c, l := newCheckoutMutationFixture(t)
			if !byteFixtureSupportsAuthorityOrChecksFallback(t, f, c, l) {
				return
			}
			route := f.route()
			var release func()
			var attempt context.Context
			switch kind {
			case "pending":
				require.NoError(t, c.clearDirtySlot(t.Context(), &route))
				require.Equal(t, store_sqlite.RoutePending, route.State)
			case "existing demand":
				require.NoError(t, c.captureByteAnchor(t.Context(), route))
				release, _ = c.reserveByteMutation()
				defer release()
			default:
				var finish func()
				var ok bool
				attempt, finish, ok = c.beginByteMutationAttempt(t.Context())
				require.True(t, ok)
				release = finish
			}
			type admission struct {
				m   *CheckoutMutation
				err error
			}
			got := make(chan admission, 1)
			incarnation := byteFixtureIncarnation(t, f)
			go func() {
				m, err := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, incarnation)
				got <- admission{m, err}
			}()
			received := false
			defer func() {
				if release != nil {
					release()
				}
				if !received {
					result := semanticMotionReceive(t, got)
					if result.m != nil {
						result.m.Close()
					}
				}
			}()
			if attempt != nil {
				semanticMotionReceive(t, attempt.Done())
				require.ErrorIs(t, context.Cause(attempt), errByteMutationHandoff)
				select {
				case early := <-got:
					received = true
					if early.m != nil {
						early.m.Close()
					}
					t.Fatal("handoff returned before registered attempt drained")
				default:
				}
				release()
				release = nil
			}
			result := semanticMotionReceive(t, got)
			received = true
			require.NoError(t, result.err)
			require.NotNil(t, result.m)
			defer result.m.Close()
			require.NotNil(t, result.m.byteAnchor)
			require.NotNil(t, result.m.byteSnapshot)
			result.m.Close()
			if release != nil {
				release()
			}
			c.byteMu.Lock()
			demand := c.byteDemand
			c.byteMu.Unlock()
			require.Zero(t, demand)
		})
	}
}

func TestByteHandoffIdleDecisionDoesNotCancelALaterCycle(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
	f, c, l := newCheckoutMutationFixture(t)
	before := f.route()
	m, err := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
	require.Nil(t, m)
	require.ErrorIs(t, err, ErrCheckoutByteAnchorUnavailable)
	p := newMotionContextProvider(t, c)
	ticket := queueCheckoutSourceEdit(t, f, l, "package fixture\nfunc RacedOrdinaryHelper() {}\n")
	done := semanticMotionCycle(t, c, t.Context(), p)
	semanticMotionReceive(t, p.started)
	// Work started after the advisory decline. The unchanged ordinary guard
	// refuses the withdrawn old route; it never treats the idle observation as
	// freshness or interrupts the later cycle.
	m, err = l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if m != nil {
		m.Close()
	}
	require.Nil(t, m)
	require.ErrorIs(t, err, ErrCheckoutMutationRouteMoved)
	select {
	case <-p.canceled:
		t.Fatal("ordinary fallback canceled a later cycle")
	default:
	}
	p.unblock()
	semanticMotionReceive(t, done)
	result := awaitCheckoutRefresh(t, ticket)
	require.NoError(t, result.Err)
	require.True(t, result.Reindexed)
}
