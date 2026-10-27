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

// Read identity on both sides of the route: do not join an old incarnation
// to a replacement's route. Errors are unknown, never a healthy verdict.
func (l *CheckoutLifecycle) currentMutationRefusalRoute(ctx context.Context, checkoutID, root string) (store_sqlite.Checkout, store_sqlite.CheckoutRoute, bool) {
	var noCheckout store_sqlite.Checkout
	var noRoute store_sqlite.CheckoutRoute
	if l == nil || l.catalog == nil {
		return noCheckout, noRoute, false
	}
	checkout, found, err := l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil || !found || !sameMutationRoot(checkout.RootPath, root) || checkout.State != store_sqlite.CheckoutStateReady {
		return noCheckout, noRoute, false
	}
	route, found, err := l.catalog.GetCheckoutRoute(ctx, checkoutID)
	if err != nil || !found || route.RouteEpoch <= 0 || route.State != store_sqlite.RouteActive || route.CommitGenerationID <= 0 || route.DirtyGenerationID <= 0 {
		return noCheckout, noRoute, false
	}
	again, found, err := l.catalog.GetCheckout(ctx, checkoutID)
	if err != nil || !found || again.Incarnation != checkout.Incarnation || !sameMutationRoot(again.RootPath, checkout.RootPath) || again.State != store_sqlite.CheckoutStateReady {
		return noCheckout, noRoute, false
	}
	return checkout, route, true
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
	if w.slot == nil {
		return
	}
	checkout, route, known := l.currentMutationRefusalRoute(ctx, w.slot.checkoutID, w.slot.root)
	if !known || checkout.Incarnation != w.slot.incarnation || route != w.route {
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

// CheckoutMutationState distinguishes an authoritative healthy route from an
// older daemon's unknown state. This lets hooks ignore a legacy TTL fallback
// after a modern daemon has published a fresh route, without writing session
// state from every PreToolUse invocation.
//
// Snapshot the slot BEFORE catalog reads and revalidate it afterward. A
// concurrent admission/refusal/replacement yields unknown rather than a stale
// ready answer or clearing a newer refusal. Route publication linearizes after
// its catalog read; later readers cannot use a mark for the preceding route.
func (l *CheckoutLifecycle) CheckoutMutationState(ctx context.Context, checkoutID, root string) (state, reason string) {
	if l == nil {
		return "", ""
	}
	l.mutationRefusalMu.Lock()
	slot := l.mutationRefusals[checkoutID]
	var mark *checkoutMutationRefusalMark
	var revision uint64
	if slot != nil {
		mark, revision = slot.mark, slot.revision
	}
	l.mutationRefusalMu.Unlock()
	checkout, route, known := l.currentMutationRefusalRoute(ctx, checkoutID, root)
	if !known {
		return "", ""
	}
	l.mutationRefusalMu.Lock()
	defer l.mutationRefusalMu.Unlock()
	if l.mutationRefusals[checkoutID] != slot || (slot != nil && (slot.revision != revision || slot.mark != mark)) {
		return "", ""
	}
	if slot != nil && mark != nil {
		if slot.incarnation == checkout.Incarnation && pathkey.EqualPaths(slot.root, checkout.RootPath) && mark.route == route {
			return "read_only", mark.reason
		}
		slot.revision++
		slot.mark = nil
	}
	return "ready", ""
}

func (l *CheckoutLifecycle) CheckoutMutationRefusal(ctx context.Context, checkoutID, root string) (reason string, refused bool) {
	state, reason := l.CheckoutMutationState(ctx, checkoutID, root)
	return reason, state == "read_only"
}
