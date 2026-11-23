package blobstore

import (
	"context"
	"errors"
	"fmt"

	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
)

// Reasons of a Refusal.
const (
	RefusedMissing  = "original_missing"
	RefusedMismatch = "original_mismatch"
)

// Refusal is a removal that preflight proved impossible for this one
// original, before arm was called: in a verified shard on the originals
// mount, the canonical name is absent, or it is a regular file whose size
// or complete SHA-256 differs. Nothing was changed. RemoveOriginal returns
// it only on its own: a failed close or any other error joined to it, and
// every replay, is a plain error instead.
type Refusal struct {
	SHA    string
	Reason string
	Err    error
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("%s: %s: %v", r.Reason, r.SHA, r.Err)
}

func (r *Refusal) Unwrap() error { return r.Err }

// RemovalSteps are the namespace and durability steps that RemoveOriginal
// performs on the shard directory once the removal is armed. Production
// uses the shard's *fsops.Root itself.
type RemovalSteps interface {
	Remove(rel string) error
	SyncDir(rel string) error
	Close() error
}

// SetRemovalSteps wraps the shard of every later removal. It is never
// called in production. A test wraps the real root to observe that the
// unlink, the directory fsync and the checked close happen, in this order,
// or to inject their failure.
func (s *Store) SetRemovalSteps(wrap func(*fsops.Root) RemovalSteps) { s.removalSteps = wrap }

// RemoveOriginal verifies and unlinks one original while the caller holds
// exclusive originals admission and the volume lock. arm must return success
// only once the matching removal intent is durable. A missing leaf is
// accepted only during replay, and still requires a shard fsync. It returns
// nil only after the unlink (or the replayed absence), a successful fsync
// of the shard directory and its checked close.
func (s *Store) RemoveOriginal(ctx context.Context, b Blob, replay bool, arm func() error, hook failpoint.Hook) (err error) {
	if err := ValidateSHA(b.SHA256); err != nil {
		return err
	}
	root, err := s.originals.SubRoot(shardOf(b.SHA256))
	if err != nil {
		return err
	}
	var shard RemovalSteps = root
	if s.removalSteps != nil {
		shard = s.removalSteps(root)
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := shard.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
	}()
	leaf, absent, err := s.preflight(ctx, root, b, replay, true)
	if err != nil {
		return err
	}
	if err := arm(); err != nil {
		return err
	}
	if err := hook.Hit("removal_before_unlink"); err != nil {
		return err
	}
	current, err := root.Stat(b.SHA256)
	if absent {
		if !missingLeaf(err) {
			return errors.Join(fmt.Errorf("absent original changed during replay"), err)
		}
	} else {
		if err != nil {
			return err
		}
		if current != leaf {
			return fmt.Errorf("original changed after verification")
		}
		if err := shard.Remove(b.SHA256); err != nil {
			return err
		}
		if err := hook.Hit("removal_unlinked"); err != nil {
			return err
		}
	}
	if err := hook.Hit("removal_before_sync"); err != nil {
		return err
	}
	if err := shard.SyncDir(""); err != nil {
		return err
	}
	if err := hook.Hit("removal_synced"); err != nil {
		return err
	}
	if err := hook.Hit("removal_before_close"); err != nil {
		return err
	}
	closed = true
	return shard.Close()
}

// CheckRemoval repeats, read-only, the checks RemoveOriginal makes before
// arming an unarmed removal: nil when removal would proceed, a bare
// *Refusal when the collector would keep and skip the original, and any
// other error when it would stop the process. Without hash, a same-size
// content change is not detected.
func (s *Store) CheckRemoval(ctx context.Context, b Blob, hash bool) (err error) {
	if err := ValidateSHA(b.SHA256); err != nil {
		return err
	}
	root, err := s.originals.SubRoot(shardOf(b.SHA256))
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	_, _, err = s.preflight(ctx, root, b, false, hash)
	return err
}

// preflight checks the shard's mount and the leaf before arming. A missing
// leaf is a Refusal unless replaying; a regular file of another size, or
// with hash of another SHA-256, is a Refusal unless replaying. Anything
// else that is not a matching regular file is a plain error.
func (s *Store) preflight(ctx context.Context, root *fsops.Root, b Blob, replay, hash bool) (fsops.FileInfo, bool, error) {
	same, err := fsops.SameMount(s.originals, root)
	if err != nil {
		return fsops.FileInfo{}, false, err
	}
	if !same {
		return fsops.FileInfo{}, false, fmt.Errorf("original shard is on a different mount")
	}
	leaf, err := root.Stat(b.SHA256)
	absent := missingLeaf(err)
	if absent && !replay {
		return leaf, absent, &Refusal{SHA: b.SHA256, Reason: RefusedMissing, Err: err}
	}
	if absent {
		return leaf, absent, nil
	}
	if err != nil {
		return leaf, absent, err
	}
	if leaf.Type != fsops.TypeRegular {
		// An unexpected entry type is not proven local damage.
		return leaf, absent, &Error{Code: CodeCorrupt, Op: "verify removal", SHA: b.SHA256,
			Err: fmt.Errorf("expected a regular file, found %s", leaf.Type)}
	}
	got := Blob{SHA256: b.SHA256, Size: leaf.Size}
	if hash && leaf.Size == b.Size {
		if got, err = readBlob(ctx, root, b.SHA256, false); err != nil {
			return leaf, absent, err
		}
	}
	if err := compare("verify removal", b, got); err != nil {
		if replay {
			return leaf, absent, err
		}
		return leaf, absent, &Refusal{SHA: b.SHA256, Reason: RefusedMismatch, Err: err}
	}
	return leaf, absent, nil
}

// A joined close error makes an absent entry unsafe to replay or refuse.
func missingLeaf(err error) bool {
	e, ok := err.(*fsops.Error)
	return ok && e.Code == fsops.CodeNotFound
}
