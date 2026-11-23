// Package reclaim collects originals whose last committed catalog reference
// is gone. The volume owner supplies the paired store ID and holds the
// volume lock for recovery and throughout the collector's lifetime.
package reclaim

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/blobstore"
	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
	"musiclib/internal/store"
)

const (
	CodeUnsafe  = "original_removal_unsafe"
	CodePending = "original_removal_pending"
	// CodeDeferred is logged, not returned: the candidate's own file is
	// missing or differs, so it is kept and skipped until restart.
	CodeDeferred = "original_reclamation_deferred"

	interval     = 2 * time.Second
	databaseWait = 30 * time.Second
)

type Error struct {
	Hash string
	Err  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: original %s: %v", CodeUnsafe, e.Hash, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

type Collector struct {
	db      *pgxpool.Pool
	storeID uuid.UUID
	blobs   *blobstore.Store
	gate    *Admission
	log     *slog.Logger
	hook    failpoint.Hook
	// deferred holds the generations refused in this process. It is used
	// by the Run goroutine only and pruned to pending generations.
	deferred map[int64]struct{}
}

func New(db *pgxpool.Pool, id uuid.UUID, originals *fsops.Root, gate *Admission, log *slog.Logger, hook failpoint.Hook) *Collector {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Collector{db: db, storeID: id, blobs: blobstore.OpenReadOnly(originals), gate: gate, log: log, hook: hook,
		deferred: map[int64]struct{}{}}
}

// Registered reports whether an unexpectedly absent original must keep its
// existing file: it has a committed reference, a reclamation row (live or
// eligible) or an armed removal. An orphan registration with none of them,
// such as one older than reclamation or reset by a restore, is never
// collected, so the same bytes may be pinned again. A failed lookup must
// never authorize a new pin.
func Registered(ctx context.Context, q *store.Queries, hash string) (bool, error) {
	registered, err := q.OriginalFileRequired(ctx, hash)
	if err != nil && ctx.Err() == nil {
		err = &store.Error{Code: store.CodeConnectionLost, Msg: "checking an original registration", Err: err}
	}
	return registered, err
}

// Pending reads the outstanding intent for offline diagnostics and refusal.
func Pending(ctx context.Context, q *store.Queries) (string, error) {
	row, err := q.ReadOriginalRemoval(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return row.Hash, err
}

// Armed describes the outstanding removal intent for offline diagnostics.
// Problem is empty when startup can replay it as far as the checks show;
// without deep, a same-size content change is not detected.
type Armed struct {
	Hash    string
	Problem string
}

// InspectArmed repeats the replay checks of Recover without changing the
// database or the volume. The caller holds the volume lock.
func InspectArmed(ctx context.Context, q *store.Queries, storeID uuid.UUID, originals *fsops.Root, deep bool) (Armed, error) {
	i, err := q.ReadOriginalRemoval(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Armed{}, nil
	}
	if err != nil {
		return Armed{}, err
	}
	a := Armed{Hash: i.Hash}
	problem := func(p string) (Armed, error) {
		a.Problem = p
		return a, nil
	}
	if i.StoreID != storeID {
		return problem("the intent names another store")
	}
	if blobstore.ValidateSHA(i.Hash) != nil {
		return problem("the intent has an invalid hash")
	}
	row, err := q.ReadOriginalReclamation(ctx, i.Hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Armed{}, err
	}
	if err != nil || row.Generation == nil || *row.Generation != i.Generation {
		return problem("the intent's generation is not the current candidate")
	}
	if row.Size != i.Size {
		return problem("the intent's size differs from the registration")
	}
	live, err := q.OriginalHasReferences(ctx, i.Hash)
	if err != nil {
		return Armed{}, err
	}
	if live {
		return problem("the original has a catalog reference")
	}
	shard := i.Hash[:2] + "/" + i.Hash[2:4]
	fi, err := originals.Stat(shard)
	switch {
	case fsops.Code(err) == fsops.CodeNotFound:
		return problem("the shard directory is missing")
	case err != nil:
		return problem("the shard directory is unreadable")
	case fi.Type != fsops.TypeDir:
		return problem("the shard is not a directory")
	}
	bs := blobstore.OpenReadOnly(originals)
	b := blobstore.Blob{SHA256: i.Hash, Size: i.Size}
	err = bs.Check(b)
	switch {
	case blobstore.Code(err) == blobstore.CodeNotFound:
		return a, nil // replay finalizes an absent leaf
	case err != nil:
		return problem("the original is not a regular file of the recorded size")
	case deep:
		if n, err := bs.Verify(ctx, i.Hash); err != nil || n != i.Size {
			if ctx.Err() != nil {
				return Armed{}, ctx.Err()
			}
			return problem("the original's content differs from its hash")
		}
	}
	return a, nil
}

// Recover completes an armed removal before HTTP, jobs and work cleanup.
func (c *Collector) Recover(ctx context.Context) error {
	i, err := store.New(c.db).ReadOriginalRemoval(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.remove(ctx, i, true)
}

// Run tries one candidate per interval. It takes exclusive admission only
// when no operation holds or awaits a permit, so an active user defers
// collection instead of waiting behind it. A fresh interval follows each
// pass.
func (c *Collector) Run(ctx context.Context) {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.gate.Failed():
			return
		case <-timer.C:
		}
		if err := c.collectOne(ctx); err != nil {
			c.gate.Fence(err)
			return
		}
		timer.Reset(interval)
	}
}

var errStale = errors.New("original candidate changed before admission")

// collectOne runs on the Run goroutine only. A candidate whose own leaf is
// proven missing or different is deferred for the rest of this process,
// by its generation: a later generation of the same hash is tried anew,
// and so is every candidate after a restart.
func (c *Collector) collectOne(ctx context.Context) error {
	q := store.New(c.db)
	deferred, err := c.pruneDeferred(ctx, q)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	row, err := q.NextOriginalReclamation(ctx, deferred)
	if errors.Is(err, pgx.ErrNoRows) || ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return err
	}
	if row.Generation == nil {
		return &Error{Hash: row.Hash, Err: errors.New("candidate has no generation")}
	}

	release, ok := c.gate.tryExclusive()
	if !ok {
		return nil
	}
	defer release()
	i := store.ReadOriginalRemovalRow{StoreID: c.storeID, Hash: row.Hash, Size: row.Size, Generation: *row.Generation}
	err = c.remove(ctx, i, false)
	if refusal, ok := err.(*blobstore.Refusal); ok {
		c.deferred[i.Generation] = struct{}{}
		c.log.Warn("original kept: its file is missing or differs, removal deferred until restart",
			"code", CodeDeferred, "hash", i.Hash, "generation", i.Generation, "reason", refusal.Reason)
		return nil
	}
	if err != nil {
		// Fence while still holding exclusive admission.
		c.gate.Fence(err)
		return err
	}
	return nil
}

// pruneDeferred forgets deferred generations that are no longer pending,
// so the set stays bounded by the current candidates, and returns the rest.
func (c *Collector) pruneDeferred(ctx context.Context, q *store.Queries) ([]int64, error) {
	if len(c.deferred) == 0 {
		return []int64{}, nil
	}
	known := make([]int64, 0, len(c.deferred))
	for g := range c.deferred {
		known = append(known, g)
	}
	pending, err := q.PendingOriginalGenerations(ctx, known)
	if err != nil {
		return nil, err
	}
	clear(c.deferred)
	for _, g := range pending {
		c.deferred[g] = struct{}{}
	}
	return append([]int64{}, pending...), nil
}

// remove returns nil for a completed removal, or for a candidate that is
// stale or interrupted before anything was armed. It returns a bare
// *blobstore.Refusal only when preflight proved the candidate's own leaf
// missing or different before arm was called. Every other failure is an
// *Error that the caller must fence.
func (c *Collector) remove(ctx context.Context, i store.ReadOriginalRemovalRow, replay bool) error {
	armed, armCalled := replay, false
	err := c.transition(ctx, i, replay, false, func(context.Context, *store.CatalogTx) error { return nil })
	if err == nil {
		err = c.blobs.RemoveOriginal(ctx, blobstore.Blob{SHA256: i.Hash, Size: i.Size}, replay, func() error {
			armCalled = true
			if replay {
				return ctx.Err()
			}
			if err := c.hook.Hit("removal_before_prepare"); err != nil {
				return err
			}
			if err := c.transition(ctx, i, false, true, func(ctx context.Context, tx *store.CatalogTx) error {
				if err := tx.ArmOriginalRemoval(ctx, store.ArmOriginalRemovalParams(i)); err != nil {
					return err
				}
				return c.hook.Hit("removal_before_prepare_commit")
			}); err != nil {
				return err
			}
			armed = true
			return c.hook.Hit("removal_prepared")
		}, c.hook)
	}
	if err != nil {
		if !armed && (err == errStale || (ctx.Err() != nil && (err == ctx.Err() || store.Code(err) == store.CodeCanceled))) {
			return nil
		}
		if refusal, ok := err.(*blobstore.Refusal); ok && !replay && !armCalled {
			return refusal
		}
		return &Error{Hash: i.Hash, Err: err}
	}

	// Once PREPARE has been acknowledged, finish even if shutdown started.
	ctx = context.WithoutCancel(ctx)
	if err := c.transition(ctx, i, true, true, func(ctx context.Context, tx *store.CatalogTx) error {
		if err := tx.ClearOriginalRemoval(ctx); err != nil {
			return err
		}
		if err := tx.ClearOriginalReclamation(ctx, i.Hash); err != nil {
			return err
		}
		if err := tx.DeleteOriginalRegistration(ctx, i.Hash); err != nil {
			return err
		}
		return c.hook.Hit("removal_before_finalize_commit")
	}); err != nil {
		return &Error{Hash: i.Hash, Err: err}
	}
	return c.hook.Hit("removal_finalized")
}

// transition rechecks the exact tuple under the catalog lock. Filesystem
// operations happen between these short transactions, under admission.
func (c *Collector) transition(ctx context.Context, i store.ReadOriginalRemovalRow, expectIntent, durable bool, fn func(context.Context, *store.CatalogTx) error) error {
	ctx, cancel := context.WithTimeout(ctx, databaseWait)
	defer cancel()
	return store.InCatalogTxBefore(ctx, c.db, func(tx *store.CatalogTx) error {
		id, err := tx.GetStoreID(ctx)
		if err != nil {
			return err
		}
		if id == uuid.Nil || id != c.storeID || id != i.StoreID {
			return errors.New("removal store ID differs from paired store")
		}

		current, err := tx.ReadOriginalRemoval(ctx)
		if expectIntent {
			if err != nil {
				return err
			}
			if current != i {
				return errors.New("removal intent changed")
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			if err != nil {
				return err
			}
			return errors.New("another removal intent is present")
		}

		row, err := tx.ReadOriginalReclamation(ctx, i.Hash)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if row.Generation == nil || *row.Generation != i.Generation {
			if !expectIntent {
				return errStale
			}
			return errors.New("removal generation differs from current candidate")
		}
		if row.Size != i.Size {
			return errors.New("removal size differs from current registration")
		}
		live, err := tx.OriginalHasReferences(ctx, i.Hash)
		if err != nil {
			return err
		}
		if live {
			return errors.New("candidate still has a catalog reference")
		}
		if durable {
			if err := tx.MakeOriginalRemovalDurable(ctx); err != nil {
				return err
			}
		}
		return fn(ctx, tx)
	})
}
