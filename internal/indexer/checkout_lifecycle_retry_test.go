package indexer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/reconcile"
)

func TestCheckoutLifecycleReconcileFamilyPersistsExpiredInaccessible(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	root := f.gitRepo("event-offline")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "event-offline"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	require.NoError(t, os.RemoveAll(root))

	graced, err := f.lc.ReconcileFamily(ctx, tracked.FamilyID)
	require.NoError(t, err)
	entry := checkoutReportByID(t, graced, tracked.CheckoutID)
	assert.Equal(t, reconcile.ActionAvailabilityGraceStarted, entry.Action)
	assert.NotZero(t, entry.RetryAt)

	f.lc.retryMu.Lock()
	retry, armed := f.lc.familyRetries[tracked.FamilyID]
	f.lc.retryMu.Unlock()
	require.True(t, armed, "the grace deadline must not depend on the hourly sweep")
	assert.Equal(t, entry.RetryAt, retry.deadline)

	f.clock.advance(lifecycleGrace.AvailabilityGrace + time.Second)
	expired, err := f.lc.ReconcileFamily(ctx, tracked.FamilyID)
	require.NoError(t, err)
	entry = checkoutReportByID(t, expired, tracked.CheckoutID)
	assert.Equal(t, reconcile.ActionPrimaryClosureRetired, entry.Action)
	// The build the registration started can still hold the corpus, so the
	// purge may be the cleanup runtime's to finish.
	require.Eventually(t, func() bool {
		return f.mi.GetMetadata("event-offline") == nil && !f.lc.RepositoryAdmissionClosed("event-offline")
	}, 30*time.Second, 10*time.Millisecond, "its corpus is never evicted")
	assert.NotContains(t, f.configPaths(), root, "event-driven cleanup is persisted")
}

func TestCheckoutLifecycleFamilyRetryTimerFires(t *testing.T) {
	lifecycle := &CheckoutLifecycle{
		now:           time.Now,
		logger:        zap.NewNop(),
		familyRetries: map[string]familyRetry{},
		coordinators:  map[string]*CheckoutCoordinator{},
	}
	t.Cleanup(func() { require.NoError(t, lifecycle.Close()) })

	initial := time.Now().Unix()
	lifecycle.scheduleFamilyRetryAt("family", initial)
	require.Eventually(t, func() bool {
		lifecycle.retryMu.Lock()
		defer lifecycle.retryMu.Unlock()
		retry, ok := lifecycle.familyRetries["family"]
		return ok && retry.deadline > initial
	}, time.Second, 5*time.Millisecond,
		"the expired timer should run reconciliation and schedule a bounded retry after failure")
}

func checkoutReportByID(t *testing.T, report reconcile.FamilyReport, checkoutID string) reconcile.CheckoutReport {
	t.Helper()
	for _, checkout := range report.Checkouts {
		if checkout.CheckoutID == checkoutID {
			return checkout
		}
	}
	t.Fatalf("checkout %s missing from report %+v", checkoutID, report)
	return reconcile.CheckoutReport{}
}
