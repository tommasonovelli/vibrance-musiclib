package reclaim

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/blobstore"
	"musiclib/internal/failpoint"
	"musiclib/internal/faulttest"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

type fixture struct {
	url, dir string
	db       *pgxpool.Pool
	vol      *volume.Volume
	blobs    *blobstore.Store
	album    uuid.UUID
}

func openVolume(t *testing.T, dir string, db *pgxpool.Pool) *volume.Volume {
	t.Helper()
	v, err := volume.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.CheckMaintenance(); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Identify(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := v.OpenLayout(); err != nil {
		t.Fatal(err)
	}
	if err := v.CheckFilesystem(); err != nil {
		t.Fatal(err)
	}
	return v
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := pgtest.EmptyDB(t)
	db := pgtest.Pool(t, url)
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	f := &fixture{url: url, db: db, dir: t.TempDir(), album: store.NewID()}
	f.vol = openVolume(t, f.dir, db)
	t.Cleanup(func() {
		if f.vol != nil {
			if err := f.vol.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	var err error
	f.blobs, err = blobstore.NewRegistered(f.vol.Originals(), f.vol.Work(), func(ctx context.Context, hash string) (bool, error) {
		return Registered(ctx, store.New(db), hash)
	})
	if err != nil {
		t.Fatal(err)
	}
	// A trashed album is sufficient for these inventory checks and has
	// no pending output when the fixture is opened by a fresh process.
	artist := store.NewID()
	f.sql(t, `INSERT INTO artists (id,name,folder_key,revision) VALUES ($1,'Artist','artist',1)`, artist)
	f.sql(t, `INSERT INTO albums (id, artist_id,title,folder_key,revision,deleted_at)
        VALUES ($1,$2,'Fixture','fixture',1,now())`, f.album, artist)
	return f
}

func (f *fixture) sql(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) put(t *testing.T, content string, register bool) blobstore.Blob {
	t.Helper()
	b, err := f.blobs.Put(t.Context(), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if register {
		f.sql(t, `INSERT INTO blobs (hash,size,created_at) VALUES ($1,$2,now())`, b.SHA256, b.Size)
	}
	return b
}

func (f *fixture) attach(t *testing.T, b blobstore.Blob) uuid.UUID {
	t.Helper()
	id := store.NewID()
	f.sql(t, `INSERT INTO attachments (id,album_id,rel_path,path_key,blob_hash) VALUES ($1,$2,$3,$3,$4)`, id, f.album, id.String(), b.SHA256)
	return id
}

func (f *fixture) candidate(t *testing.T) (blobstore.Blob, store.ReadOriginalRemovalRow) {
	t.Helper()
	b := f.put(t, "synthetic detached original", true)
	f.sql(t, `DELETE FROM attachments WHERE id = $1`, f.attach(t, b))
	return b, f.intent(t, b)
}

func (f *fixture) intent(t *testing.T, b blobstore.Blob) store.ReadOriginalRemovalRow {
	t.Helper()
	row, err := store.New(f.db).ReadOriginalReclamation(t.Context(), b.SHA256)
	if err != nil || row.Generation == nil {
		t.Fatalf("candidate %s: %+v, %v", b.SHA256, row, err)
	}
	return store.ReadOriginalRemovalRow{StoreID: f.vol.StoreID(), Hash: b.SHA256, Size: row.Size, Generation: *row.Generation}
}

func (f *fixture) assertState(t *testing.T, b blobstore.Blob, intent, registered, present bool) {
	t.Helper()
	q := store.New(f.db)
	active, err := Pending(t.Context(), q)
	if err != nil || (active == b.SHA256) != intent || (active != "" && !intent) {
		t.Fatalf("intent hash = %q, error = %v, want present=%t", active, err, intent)
	}
	known, err := q.OriginalIsRegistered(t.Context(), b.SHA256)
	if err != nil || known != registered {
		t.Fatalf("registered = %t, error = %v, want %t", known, err, registered)
	}
	leaf := filepath.Join(f.dir, "originals", b.SHA256[:2], b.SHA256[2:4], b.SHA256)
	_, err = os.Lstat(leaf)
	if (present && err != nil) || (!present && !errors.Is(err, os.ErrNotExist)) {
		t.Fatalf("canonical stat = %v, want present=%t", err, present)
	}
	if !present {
		if info, err := os.Stat(filepath.Dir(leaf)); err != nil || !info.IsDir() {
			t.Fatalf("shard was removed or changed: %v", err)
		}
	}
}

func (f *fixture) collector(h failpoint.Hook) *Collector {
	return New(f.db, f.vol.StoreID(), f.vol.Originals(), NewAdmission(), nil, h)
}

func TestReclamationLastCommittedReference(t *testing.T) {
	f := newFixture(t)
	historical := f.put(t, "historical registered orphan", true)
	fileOnly := f.put(t, "failed upload file-only orphan", false)
	live := f.put(t, "shared track audio, lyrics, extra and cover", true)
	trackID := store.NewID()
	f.sql(t, `INSERT INTO tracks (id,album_id,disc,no,title,blob_hash,source_path,lyrics_hash)
        VALUES ($1,$2,1,1,'fixture',$3,'one.flac',$3)`, trackID, f.album, live.SHA256)
	f.sql(t, `UPDATE albums SET cover_hash = $1 WHERE id = $2`, live.SHA256, f.album)
	attached := f.attach(t, live)
	q := store.New(f.db)
	assertNoPending := func() {
		t.Helper()
		if row, err := q.NextOriginalReclamation(t.Context(), nil); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("unexpected candidate %+v: %v", row, err)
		}
	}
	assertNoPending()
	f.sql(t, `DELETE FROM attachments WHERE id = $1`, attached)
	assertNoPending()
	f.sql(t, `UPDATE tracks SET lyrics_hash = NULL WHERE id = $1`, trackID)
	assertNoPending()
	f.sql(t, `DELETE FROM tracks WHERE id = $1`, trackID)
	assertNoPending()
	// A hash that was never live stays ineligible after a transaction
	// that only transiently references it via the cover.
	transient, err := f.db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer transient.Rollback(t.Context())
	if _, err := transient.Exec(t.Context(), `UPDATE albums SET cover_hash = $1 WHERE id = $2`, historical.SHA256, f.album); err != nil {
		t.Fatal(err)
	}
	if _, err := transient.Exec(t.Context(), `UPDATE albums SET cover_hash = NULL WHERE id = $1`, f.album); err != nil {
		t.Fatal(err)
	}
	if err := transient.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := f.intent(t, live)
	again := f.attach(t, live)
	assertNoPending()
	f.sql(t, `DELETE FROM attachments WHERE id = $1`, again)
	second := f.intent(t, live)
	if second.Generation <= first.Generation {
		t.Fatalf("generation regressed: %d → %d", first.Generation, second.Generation)
	}

	c := f.collector(nil)
	if err := c.remove(t.Context(), first, false); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, live, false, true, true)
	if err := c.collectOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, live, false, false, false)
	for _, b := range []blobstore.Blob{historical, fileOnly} {
		if size, err := f.blobs.Verify(t.Context(), b.SHA256); err != nil || size != b.Size {
			t.Fatalf("unrelated blob %s: %d, %v", b.SHA256, size, err)
		}
	}
	// A later same-hash put can pin a new original and start live history.
	if b := f.put(t, "shared track audio, lyrics, extra and cover", true); b != live {
		t.Fatalf("same content changed: %+v, want %+v", b, live)
	}
	f.sql(t, `DELETE FROM attachments WHERE id = $1`, f.attach(t, live))
	if third := f.intent(t, live); third.Generation <= second.Generation {
		t.Fatalf("new registration generation %d <= %d", third.Generation, second.Generation)
	}
}

func TestReclamationRealUnlinkCommitWindows(t *testing.T) {
	for _, test := range []struct {
		name, point                          string
		rollback, armed, present, registered bool
	}{
		{"prepare lost after commit", "removal_before_prepare_commit", false, true, true, true},
		{"prepare lost before commit", "removal_before_prepare_commit", true, false, true, true},
		{"finalize lost after commit", "removal_before_finalize_commit", false, false, false, false},
		{"finalize lost before commit", "removal_before_finalize_commit", true, true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			b, _ := f.candidate(t)
			proxy := pgtest.NewProxy(t, f.url)
			db := pgtest.Pool(t, proxy.URL)
			gate := NewAdmission()
			c := New(db, f.vol.StoreID(), f.vol.Originals(), gate, nil, func(p failpoint.Point) error {
				switch p.Name {
				case "removal_before_unlink":
					f.assertState(t, b, true, true, true)
				case "removal_before_finalize_commit":
					f.assertState(t, b, true, true, false)
				}
				if p.Name == test.point {
					if test.rollback {
						proxy.CutBeforeNextCommit()
					} else {
						proxy.LoseNextCommitAck()
					}
				}
				return nil
			})
			err := c.collectOne(t.Context())
			if store.Code(err) != store.CodeCommitUncertain {
				t.Fatalf("collect: %v", err)
			}
			if _, err := gate.Enter(t.Context()); !errors.Is(err, ErrStopped) {
				t.Fatalf("admission after uncertain COMMIT: %v", err)
			}
			f.assertState(t, b, test.armed, test.registered, test.present)
			if err := f.collector(nil).Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			unresolved := test.rollback && test.point == "removal_before_prepare_commit"
			f.assertState(t, b, false, unresolved, unresolved)
		})
	}
}

func TestHelperProcess(t *testing.T) {
	if faulttest.Mode() != "original-removal" {
		return
	}
	db := pgtest.Pool(t, os.Getenv("RECLAIM_TEST_DATABASE"))
	v := openVolume(t, os.Getenv("RECLAIM_TEST_VOLUME"), db)
	c := New(db, v.StoreID(), v.Originals(), NewAdmission(), nil, faulttest.Crash(os.Getenv("RECLAIM_TEST_CRASH")))
	if err := c.collectOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	faulttest.Exit("completed without killing process")
}

func TestReclamationRealSIGKILLReplay(t *testing.T) {
	for _, test := range []struct {
		point                    string
		armed, absent, finalized bool
	}{
		{"removal_before_prepare", false, false, false},
		{"removal_before_prepare_commit", false, false, false},
		{"removal_prepared", true, false, false},
		{"removal_before_unlink", true, false, false},
		{"removal_unlinked", true, true, false},
		{"removal_before_sync", true, true, false},
		{"removal_synced", true, true, false},
		{"removal_before_close", true, true, false},
		{"removal_before_finalize_commit", true, true, false},
		{"removal_finalized", false, true, true},
	} {
		t.Run(test.point, func(t *testing.T) {
			f := newFixture(t)
			b, _ := f.candidate(t)
			protected := f.put(t, "unchanged fixture neighbor", true)
			f.attach(t, protected)
			if err := f.vol.Close(); err != nil {
				t.Fatal(err)
			}
			f.vol = nil
			result := faulttest.RunChild(t, 30*time.Second, "original-removal",
				"RECLAIM_TEST_DATABASE="+f.url, "RECLAIM_TEST_VOLUME="+f.dir, "RECLAIM_TEST_CRASH="+test.point)
			if result != faulttest.Killed {
				t.Fatalf("child result %q, want SIGKILL", result)
			}
			f.assertState(t, b, test.armed, !test.finalized, !test.absent)
			f.vol = openVolume(t, f.dir, f.db)
			if err := f.collector(nil).Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			remains := !test.armed && !test.finalized
			f.assertState(t, b, false, remains, remains)
			if n, err := blobstore.OpenReadOnly(f.vol.Originals()).Verify(t.Context(), protected.SHA256); err != nil || n != protected.Size {
				t.Fatalf("protected neighbor: %d, %v", n, err)
			}
		})
	}
}

// A durable intent whose state matches no legal replay step suspends
// recovery. Nothing is unlinked, recreated or cleared, and the evidence in
// the shard stays exactly as found.
func TestReclamationRejectUnsafeReplay(t *testing.T) {
	for _, kind := range []string{"missing-shard", "wrong-content", "wrong-size", "symlink", "directory", "wrong-store", "wrong-generation", "wrong-intent-size"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			b, i := f.candidate(t)
			rel := filepath.Join(b.SHA256[:2], b.SHA256[2:4], b.SHA256)
			leaf := filepath.Join(f.dir, "originals", rel)
			if kind != "wrong-store" && kind != "wrong-generation" && kind != "wrong-intent-size" {
				// Move the fixture bytes aside before introducing damage.
				outside := filepath.Join(f.dir, "kept-synthetic-bytes")
				if err := os.Rename(leaf, outside); err != nil {
					t.Fatal(err)
				}
				var err error
				switch kind {
				case "missing-shard":
					err = os.Remove(filepath.Dir(leaf))
				case "wrong-content":
					err = os.WriteFile(leaf, []byte(strings.Repeat("!", int(b.Size))), 0o444)
				case "wrong-size":
					err = os.WriteFile(leaf, []byte("!"), 0o444)
				case "symlink":
					err = os.Symlink(outside, leaf)
				case "directory":
					err = os.Mkdir(leaf, 0o755)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "wrong-store":
				i.StoreID = store.NewID()
			case "wrong-generation":
				i.Generation++
			case "wrong-intent-size":
				i.Size++
			}
			if err := store.New(f.db).ArmOriginalRemoval(t.Context(), store.ArmOriginalRemovalParams(i)); err != nil {
				t.Fatal(err)
			}
			evidence := treeOf(t, f.dir)
			c := f.collector(func(p failpoint.Point) error {
				if p.Name == "removal_before_unlink" || p.Name == "removal_before_sync" {
					t.Errorf("reached filesystem mutation phase %s", p.Name)
				}
				return nil
			})
			err := c.Recover(t.Context())
			var unsafe *Error
			if !errors.As(err, &unsafe) {
				t.Fatalf("unsafe canonical state accepted: %v", err)
			}
			actual, err := store.New(f.db).ReadOriginalRemoval(t.Context())
			if err != nil || actual != i {
				t.Fatalf("damaged intent was changed: %+v, %v", actual, err)
			}
			if known, err := store.New(f.db).OriginalIsRegistered(t.Context(), b.SHA256); err != nil || !known {
				t.Fatalf("registration changed: %t %v", known, err)
			}
			if after := treeOf(t, f.dir); after != evidence {
				t.Fatalf("replay changed the volume:\n%s\nwant\n%s", after, evidence)
			}
		})
	}
}

// treeOf describes every entry under dir/originals and the fixture bytes:
// type, mode, size, content hash or link target.
func treeOf(t *testing.T, dir string) string {
	t.Helper()
	var out strings.Builder
	for _, root := range []string{"originals", "kept-synthetic-bytes"} {
		err := filepath.WalkDir(filepath.Join(dir, root), func(path string, d os.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) && path == filepath.Join(dir, root) {
				return nil
			}
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(&out, "%s %s %d", strings.TrimPrefix(path, dir), info.Mode(), info.Size())
			switch {
			case info.Mode()&os.ModeSymlink != 0:
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				fmt.Fprintf(&out, " -> %s", target)
			case info.Mode().IsRegular():
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				fmt.Fprintf(&out, " %x", sha256.Sum256(data))
			}
			out.WriteByte('\n')
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out.String()
}

func TestReclamationChecksEveryRemainingRoleOnReplay(t *testing.T) {
	for _, role := range []string{"audio", "lyrics", "attachment", "cover"} {
		t.Run(role, func(t *testing.T) {
			f := newFixture(t)
			b, i := f.candidate(t)
			switch role {
			case "audio", "lyrics":
				audio := b.SHA256
				var lyrics *string
				if role == "lyrics" {
					audio = f.put(t, "other live track synthetic file", true).SHA256
					lyrics = &b.SHA256
				}
				f.sql(t, `INSERT INTO tracks (id,album_id,disc,no,title,blob_hash,source_path,lyrics_hash)
                    VALUES ($1,$2,1,1,'fixture',$3,'one.flac',$4)`, store.NewID(), f.album, audio, lyrics)
			case "attachment":
				f.attach(t, b)
			case "cover":
				f.sql(t, `UPDATE albums SET cover_hash = $1 WHERE id = $2`, b.SHA256, f.album)
			}
			// Simulate catalog evidence inconsistent with the actual FK.
			f.sql(t, `UPDATE original_reclamation SET generation = $1 WHERE hash = $2`, i.Generation, b.SHA256)
			if err := store.New(f.db).ArmOriginalRemoval(t.Context(), store.ArmOriginalRemovalParams(i)); err != nil {
				t.Fatal(err)
			}
			err := f.collector(nil).Recover(t.Context())
			if err == nil || !strings.Contains(err.Error(), "still has a catalog reference") {
				t.Fatalf("remaining %s did not refuse replay before unlink: %v", role, err)
			}
			f.assertState(t, b, true, true, true)
		})
	}
}

func TestReclamationSyncAndCloseFailuresLeaveIntent(t *testing.T) {
	for _, point := range []string{"removal_before_sync", "removal_before_close"} {
		t.Run(point, func(t *testing.T) {
			f := newFixture(t)
			b, _ := f.candidate(t)
			injected := errors.New("stop at " + point)
			c := f.collector(func(p failpoint.Point) error {
				if p.Name == point {
					return injected
				}
				return nil
			})
			if err := c.collectOne(t.Context()); !errors.Is(err, injected) {
				t.Fatalf("collect: %v", err)
			}
			f.assertState(t, b, true, true, false)
			if _, err := c.gate.Enter(t.Context()); !errors.Is(err, ErrStopped) {
				t.Fatalf("admission: %v", err)
			}
			if err := f.collector(nil).Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.assertState(t, b, false, false, false)
		})
	}
}

// The collector never queues for admission: a held operation defers it,
// a new request is admitted at once, and the removal happens once the
// holders are gone.
func TestReclamationAdmissionDefersWithoutQueuing(t *testing.T) {
	f := newFixture(t)
	b, _ := f.candidate(t)
	c := f.collector(nil)
	held, err := c.gate.Enter(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := c.collectOne(t.Context()); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		release, err := c.gate.Enter(ctx)
		cancel()
		if err != nil {
			t.Fatalf("a new request waited behind a deferred collector: %v", err)
		}
		release()
		t.Logf("foreground admitted in %s while an operation held admission", time.Since(start))
	}
	f.assertState(t, b, false, true, true)

	// A waiter queued behind the holder also defers the collector.
	queued := make(chan func(), 1)
	go func() {
		release, err := c.gate.acquire(t.Context(), math.MaxInt64)
		if err != nil {
			t.Error(err)
		}
		queued <- release
	}()
	waitUntil(t, "queued exclusive waiter", func() bool {
		if c.gate.permits.TryAcquire(1) {
			c.gate.permits.Release(1)
			return false
		}
		return true
	})
	if err := c.collectOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, b, false, true, true)
	held()
	(<-queued)()

	if err := c.collectOne(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.assertState(t, b, false, false, false)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
