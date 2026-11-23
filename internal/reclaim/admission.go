package reclaim

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

var ErrStopped = errors.New("originals admission is stopped")

// Admission is owned by the process holding the volume lock. Ordinary
// operations take one permit before looking up references, reading an upload
// or claiming a job. Collection takes all permits only when nobody holds or
// awaits one; it never queues, so no operation waits behind a collector
// that is not running.
type Admission struct {
	permits *semaphore.Weighted
	stopped atomic.Bool
	fatal   sync.Once
	failed  chan struct{}
	cause   error
}

func NewAdmission() *Admission {
	return &Admission{permits: semaphore.NewWeighted(math.MaxInt64), failed: make(chan struct{})}
}

func (a *Admission) Enter(ctx context.Context) (func(), error) {
	if a == nil {
		return func() {}, ctx.Err()
	}
	return a.acquire(ctx, 1)
}

func (a *Admission) acquire(ctx context.Context, permits int64) (func(), error) {
	if a.stopped.Load() {
		return nil, ErrStopped
	}
	if err := a.permits.Acquire(ctx, permits); err != nil {
		return nil, err
	}
	if a.stopped.Load() {
		a.permits.Release(permits)
		return nil, ErrStopped
	}
	return func() { a.permits.Release(permits) }, nil
}

// tryExclusive takes every permit without waiting. It fails while any
// operation is admitted or queued, including the shutdown's Wait.
func (a *Admission) tryExclusive() (func(), bool) {
	if a.stopped.Load() || !a.permits.TryAcquire(math.MaxInt64) {
		return nil, false
	}
	if a.stopped.Load() {
		a.permits.Release(math.MaxInt64)
		return nil, false
	}
	return func() { a.permits.Release(math.MaxInt64) }, true
}

// Fence records a fatal outcome before the operation releases admission.
func (a *Admission) Fence(err error) {
	if a == nil {
		return
	}
	a.Stop()
	a.fatal.Do(func() {
		a.cause = err
		close(a.failed)
	})
}

func (a *Admission) Failed() <-chan struct{} { return a.failed }

func (a *Admission) Err() error {
	select {
	case <-a.failed:
		return a.cause
	default:
		return nil
	}
}

func (a *Admission) Stop() { a.stopped.Store(true) }

// Wait joins admitted work, including handlers still returning after the
// server closed their connections. Stop must be called first.
func (a *Admission) Wait() {
	_ = a.permits.Acquire(context.Background(), math.MaxInt64)
	a.permits.Release(math.MaxInt64)
}
