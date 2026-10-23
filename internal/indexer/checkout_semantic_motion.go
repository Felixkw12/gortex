package indexer

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
)

// semanticMotionAttempt is private to one dirty build, not its refresh ticket.
// Watcher events only request a verifier; only a successful sample different
// from the build's exact before sample may cancel its unbounded semantic read.
// One verifier per attempt bounds work even when the watcher keeps firing.
// A missed change is still caught by the unchanged final publication fence.
type semanticMotionAttempt struct {
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelCauseFunc
	before, after string
	sample        func(context.Context) (gitstate.DirtySnapshot, error)
	verifyCtx     context.Context
	stopVerify    context.CancelFunc
	done          chan struct{}
	stopped       bool
	providerErr   error
}

func (*semanticMotionAttempt) Error() string {
	return "indexer: verified movement during working-copy enrichment"
}

func (c *CheckoutCoordinator) armSemanticMotion(a *semanticMotionAttempt, before gitstate.DirtySnapshot, started time.Time) func(error) {
	a.before = before.Fingerprint
	a.sample = func(ctx context.Context) (gitstate.DirtySnapshot, error) {
		if c.sampler != nil {
			return c.sampler.Sample(ctx)
		}
		return gitstate.SampleDirty(ctx, c.root)
	}
	a.verifyCtx, a.stopVerify = context.WithCancel(a.ctx)
	m := &c.motion
	m.mu.Lock()
	m.semantic = a
	hinted := m.lastEvent.After(started)
	m.mu.Unlock()
	if hinted {
		a.verify()
	}
	return func(providerErr error) {
		// Closing the arm and canceling/joining its verifier precede the return
		// from runEnrichment. Publication cannot race a live verifier.
		a.mu.Lock()
		a.stopped = true
		a.providerErr = providerErr
		a.stopVerify()
		done := a.done
		a.mu.Unlock()
		m.mu.Lock()
		if m.semantic == a {
			m.semantic = nil
		}
		m.mu.Unlock()
		if done != nil {
			<-done
		}
	}
}

func (a *semanticMotionAttempt) verify() {
	a.mu.Lock()
	if a.stopped || a.done != nil || a.ctx.Err() != nil || a.before == "" {
		a.mu.Unlock()
		return
	}
	a.done = make(chan struct{})
	a.mu.Unlock()
	go func() {
		defer close(a.done)
		after, err := a.sample(a.verifyCtx)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.stopped || err != nil || a.verifyCtx.Err() != nil || after.Fingerprint == "" || after.Fingerprint == a.before {
			return
		}
		a.after = after.Fingerprint
		a.cancel(a)
	}()
}

// The builder has already drained the provider and abandoned the private
// generation before this translation. Never translate an external cancel,
// deadline or unrelated failure into another build. The existing two-attempt
// loop records motion, retires the failed payload and defers enrichment.
func (a *semanticMotionAttempt) buildError(parent context.Context, generationID int64, root string, err error) error {
	if !errors.Is(err, context.Canceled) || context.Cause(a.ctx) != a {
		return err
	}
	if parentErr := parent.Err(); parentErr != nil {
		return parentErr
	}
	if a.providerErr != nil && !errors.Is(a.providerErr, context.Canceled) {
		return a.providerErr
	}
	return &DirtySnapshotChangedError{CheckoutRoot: root, GenerationID: generationID, Before: a.before, After: a.after}
}
