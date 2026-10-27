package indexer

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// These slots belong to the lifecycle, not an MCP session. Only a final stale
// admission refusal publishes a mark; admission retries carry an uncommitted
// witness. Success invalidates earlier witnesses before it returns its lease.
// mutationRefusalMu protects all mutable fields; catalog I/O never holds it.
type checkoutMutationRefusalSlot struct {
	checkoutID, incarnation, root string
	revision                      uint64
	mark                          *checkoutMutationRefusalMark
}

type checkoutMutationRefusalMark struct {
	route  store_sqlite.CheckoutRoute
	at     time.Time
	reason string
}

type checkoutMutationRefusalWitness struct {
	slot       *checkoutMutationRefusalSlot
	revision   uint64
	routeEpoch int64
	route      store_sqlite.CheckoutRoute
}

type checkoutMutationRefusalError struct {
	err     error
	witness checkoutMutationRefusalWitness
}

func (e *checkoutMutationRefusalError) Error() string { return e.err.Error() }
func (e *checkoutMutationRefusalError) Unwrap() error { return e.err }

func (l *CheckoutLifecycle) mutationRefusalWitness(checkout store_sqlite.Checkout, routeEpoch int64) checkoutMutationRefusalWitness {
	// Refuse oversized identities rather than retaining arbitrary error text or
	// caller data. Normal catalog IDs and OS paths fit comfortably inside this.
	if l == nil || checkout.CheckoutID == "" || checkout.Incarnation == "" || checkout.RootPath == "" ||
		len(checkout.CheckoutID) > 256 || len(checkout.Incarnation) > 256 || len(checkout.RootPath) > 32<<10 {
		return checkoutMutationRefusalWitness{}
	}
	l.mutationRefusalMu.Lock()
	defer l.mutationRefusalMu.Unlock()
	if l.mutationRefusals == nil {
		l.mutationRefusals = make(map[string]*checkoutMutationRefusalSlot)
	}
	slot := l.mutationRefusals[checkout.CheckoutID]
	if slot == nil || slot.incarnation != checkout.Incarnation || !pathkey.EqualPaths(slot.root, checkout.RootPath) {
		slot = &checkoutMutationRefusalSlot{checkoutID: strings.Clone(checkout.CheckoutID), incarnation: strings.Clone(checkout.Incarnation), root: strings.Clone(checkout.RootPath)}
		l.mutationRefusals[slot.checkoutID] = slot
	}
	return checkoutMutationRefusalWitness{slot: slot, revision: slot.revision, routeEpoch: routeEpoch}
}

// Capture route identity only on a stale error, after admission released its
// locks. The ordinary error chain and optional byte-route fallback stay intact.
func (l *CheckoutLifecycle) captureMutationRefusalError(ctx context.Context, err error, witness checkoutMutationRefusalWitness) error {
	if err == nil || witness.slot == nil || !errors.Is(err, ErrCheckoutMutationStale) {
		return err
	}
	if route, found, readErr := l.catalog.GetCheckoutRoute(ctx, witness.slot.checkoutID); readErr == nil && found && route.RouteEpoch == witness.routeEpoch {
		witness.route = route
	}
	return &checkoutMutationRefusalError{err: err, witness: witness}
}

func (l *CheckoutLifecycle) clearMutationRefusalOnAdmission(w checkoutMutationRefusalWitness) {
	if l == nil || w.slot == nil {
		return
	}
	l.mutationRefusalMu.Lock()
	defer l.mutationRefusalMu.Unlock()
	if l.mutationRefusals[w.slot.checkoutID] == w.slot {
		w.slot.revision++
		w.slot.mark = nil
	}
}

func (l *CheckoutLifecycle) forgetMutationRefusal(checkoutID string) {
	l.mutationRefusalMu.Lock()
	delete(l.mutationRefusals, checkoutID)
	l.mutationRefusalMu.Unlock()
}

func (l *CheckoutLifecycle) mutationRefusalRouteCurrent(ctx context.Context, slot *checkoutMutationRefusalSlot, expected store_sqlite.CheckoutRoute) (current, known bool) {
	if l == nil || l.catalog == nil || slot == nil || expected.RouteEpoch <= 0 {
		return false, false
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, slot.checkoutID)
	if err != nil {
		return false, false
	}
	if !found || checkout.Incarnation != slot.incarnation || !sameMutationRoot(checkout.RootPath, slot.root) {
		return false, true
	}
	route, found, err := l.catalog.GetCheckoutRoute(ctx, slot.checkoutID)
	if err != nil {
		return false, false
	}
	if !found || route != expected || route.State != store_sqlite.RouteActive {
		return false, true
	}
	// Revalidate incarnation after the route read rather than joining an old
	// checkout row to a replacement incarnation's route.
	checkout, found, err = l.catalog.GetCheckout(ctx, slot.checkoutID)
	if err != nil {
		return false, false
	}
	return found && checkout.Incarnation == slot.incarnation && sameMutationRoot(checkout.RootPath, slot.root), true
}

// RecordCheckoutMutationRefusal publishes only the final stale admission
// witness. Busy, Pending and unrelated view_read_only errors have no witness.
// Route publication invalidates the mark by identity, without adding another
// callback to the coordinator's publication critical path. A later coverage
// lookup retires the obsolete mark; it can never use it for the newer route.
func (l *CheckoutLifecycle) RecordCheckoutMutationRefusal(ctx context.Context, err error) {
	var refused *checkoutMutationRefusalError
	if l == nil || !errors.Is(err, ErrCheckoutMutationStale) || errors.Is(err, ErrCheckoutMutationBusy) || errors.Is(err, ErrCheckoutMutationPending) || !errors.As(err, &refused) {
		return
	}
	w := refused.witness
	if current, known := l.mutationRefusalRouteCurrent(ctx, w.slot, w.route); !known || !current {
		return
	}
	reason := "checkout view is stale"
	switch {
	case errors.Is(err, ErrCheckoutMutationRouteMoved):
		reason = "checkout route changed"
	case errors.Is(err, ErrCheckoutMutationDiskMoved):
		reason = "checkout source changed"
	}
	at := l.clock()
	l.mutationRefusalMu.Lock()
	defer l.mutationRefusalMu.Unlock()
	if l.mutationRefusals[w.slot.checkoutID] != w.slot || w.slot.revision != w.revision {
		return
	}
	w.slot.revision++
	w.slot.mark = &checkoutMutationRefusalMark{route: w.route, at: at, reason: reason}
}

// CheckoutMutationRefusal answers only for the supplied checkout/root. Copy
// the mark before catalog reads, then revalidate its pointer and revision:
// success, another refusal, replacement or retirement cannot be overwritten by
// a late lookup. A concurrent route flip linearizes after the catalog read;
// every lookup that observes the newer route rejects the old mark.
func (l *CheckoutLifecycle) CheckoutMutationRefusal(ctx context.Context, checkoutID, root string) (reason string, refused bool) {
	if l == nil {
		return "", false
	}
	l.mutationRefusalMu.Lock()
	slot := l.mutationRefusals[checkoutID]
	if slot == nil || slot.mark == nil || !pathkey.EqualPaths(slot.root, root) {
		l.mutationRefusalMu.Unlock()
		return "", false
	}
	mark, revision := slot.mark, slot.revision
	l.mutationRefusalMu.Unlock()
	current, known := l.mutationRefusalRouteCurrent(ctx, slot, mark.route)
	l.mutationRefusalMu.Lock()
	defer l.mutationRefusalMu.Unlock()
	if l.mutationRefusals[checkoutID] != slot || slot.revision != revision || slot.mark != mark {
		return "", false
	}
	if !known {
		return "", false
	}
	if !current {
		slot.revision++
		slot.mark = nil
		return "", false
	}
	return mark.reason, true
}
