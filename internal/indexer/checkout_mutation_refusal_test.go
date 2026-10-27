package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Drive real checked admission against changed bytes; merely constructing a
// view_read_only message would not prove that the refusal has a valid witness.
func staleMutationRefusal(t *testing.T, f *coordinatorFixture, c *CheckoutCoordinator, l *CheckoutLifecycle) (error, []byte) {
	t.Helper()
	original, err := os.ReadFile(filepath.Join(f.worktree, "helper.go"))
	if err != nil {
		t.Fatal(err)
	}
	forceSampledMutationAdmission(t, f, c)
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc ExternalRefusalChange() {}\n")
	lease, err := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if lease != nil {
		lease.Close()
	}
	if !errors.Is(err, ErrCheckoutMutationStale) {
		t.Fatalf("real stale admission = %v", err)
	}
	var witness *checkoutMutationRefusalError
	if !errors.As(err, &witness) || witness.witness.slot == nil {
		t.Fatal("stale admission lost its guarded identity")
	}
	return err, original
}

func TestCheckoutMutationRefusalOnlyFinalStaleAndSuccessClears(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	at := time.Unix(1200, 0)
	l.now = func() time.Time { return at }
	err, original := staleMutationRefusal(t, f, c, l)
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("intermediate admission retry published a refusal")
	}
	for _, excluded := range []error{ErrCheckoutMutationBusy, ErrCheckoutMutationPending, errors.Join(err, ErrCheckoutMutationBusy), errors.Join(err, ErrCheckoutMutationPending)} {
		l.RecordCheckoutMutationRefusal(t.Context(), excluded)
		if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
			t.Fatalf("transient %v armed native allowance", excluded)
		}
	}
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if reason, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); !marked || reason == "" || len(reason) > 80 {
		t.Fatalf("final stale mark = %q/%v", reason, marked)
	}
	l.mutationRefusalMu.Lock()
	stamp := l.mutationRefusals[f.checkoutID].mark.at
	l.mutationRefusalMu.Unlock()
	if !stamp.Equal(at) {
		t.Fatalf("mark timestamp=%v", stamp)
	}
	for _, target := range []struct{ id, root string }{{f.primaryID, f.primary}, {f.checkoutID, f.primary}, {"other-checkout", f.worktree}} {
		if _, marked := l.CheckoutMutationRefusal(t.Context(), target.id, target.root); marked {
			t.Fatal("refusal escaped checkout identity")
		}
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, marked := l.CheckoutMutationRefusal(cancelled, f.checkoutID, f.worktree); marked {
		t.Fatal("unavailable catalog supplied a verdict")
	}
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); !marked {
		t.Fatal("cancelled probe discarded the established refusal")
	}
	if writeErr := os.WriteFile(filepath.Join(f.worktree, "helper.go"), original, 0o644); writeErr != nil {
		t.Fatal(writeErr)
	}
	lease, admissionErr := l.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, f.route().RouteEpoch)
	if admissionErr != nil {
		t.Fatal(admissionErr)
	}
	lease.Close()
	// The old error can finish after this successful request returns.
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("late stale error rearmed a successful admission")
	}
}

func TestCheckoutMutationRefusalFreshRouteAndRetirementRejectOldWitness(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	err, _ := staleMutationRefusal(t, f, c, l)
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	before := f.route()
	if cycle := c.reconcile(t.Context()); cycle.Err != nil {
		t.Fatal(cycle.Err)
	}
	if f.route() == before {
		t.Fatal("fixture did not publish a genuinely new route")
	}
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("published route inherited the old refusal")
	}
	// A replacement slot with identical strings is still a different lifetime;
	// the delayed error must not acquire it through map-key equality.
	l.forgetMutationRefusal(f.checkoutID)
	checkout, found, readErr := l.catalog.GetCheckout(t.Context(), f.checkoutID)
	if readErr != nil || !found {
		t.Fatalf("checkout: %v/%v", found, readErr)
	}
	newWitness := l.mutationRefusalWitness(checkout, f.route().RouteEpoch)
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if newWitness.slot.mark != nil {
		t.Fatal("retired witness changed a replacement slot")
	}
}

func TestCheckoutMutationRefusalConcurrentOldPublishAndClear(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	err, _ := staleMutationRefusal(t, f, c, l)
	var refused *checkoutMutationRefusalError
	if !errors.As(err, &refused) {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 25 {
				l.RecordCheckoutMutationRefusal(t.Context(), err)
				_, _ = l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree)
			}
		})
	}
	l.clearMutationRefusalOnAdmission(refused.witness)
	workers.Wait()
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("older publishers overwrote the later success")
	}
}

func TestCheckoutMutationRefusalIncarnationAndFullRouteIdentity(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	err, _ := staleMutationRefusal(t, f, c, l)
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	route := f.route()
	// Catalog upsert can reuse an epoch. Even then a different state/route is
	// not the one whose admission failed.
	route.State = store_sqlite.RoutePending
	if writeErr := l.catalog.UpsertCheckoutRoute(t.Context(), route); writeErr != nil {
		t.Fatal(writeErr)
	}
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("same epoch hid a changed route identity")
	}
	checkout, found, readErr := l.catalog.GetCheckout(t.Context(), f.checkoutID)
	if readErr != nil || !found {
		t.Fatal(readErr)
	}
	checkout.Incarnation += "-replacement"
	next := l.mutationRefusalWitness(checkout, route.RouteEpoch)
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if next.slot.mark != nil {
		t.Fatal("old incarnation armed the new lifetime")
	}
}

func TestCheckoutMutationRefusalByteHandoffSuccessClears(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	err, original := staleMutationRefusal(t, f, c, l)
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if writeErr := os.WriteFile(filepath.Join(f.worktree, "helper.go"), original, 0o644); writeErr != nil {
		t.Fatal(writeErr)
	}
	lease, admissionErr := l.BeginCheckoutByteHandoff(t.Context(), f.checkoutID, f.worktree, byteFixtureIncarnation(t, f))
	if admissionErr != nil {
		t.Fatal(admissionErr)
	}
	lease.Close()
	l.RecordCheckoutMutationRefusal(t.Context(), err)
	if _, marked := l.CheckoutMutationRefusal(t.Context(), f.checkoutID, f.worktree); marked {
		t.Fatal("successful public byte admission retained or rearmed refusal")
	}
}
