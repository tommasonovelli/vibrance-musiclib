package reclaim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/blobstore"
	"musiclib/internal/failpoint"
	"musiclib/internal/fsops"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

func (f *fixture) loggingCollector(logs *logBuffer, h failpoint.Hook) *Collector {
	return New(f.db, f.vol.StoreID(), f.vol.Originals(), NewAdmission(), slog.New(slog.NewTextHandler(logs, nil)), h)
}

func (f *fixture) leaf(b blobstore.Blob) string {
	return filepath.Join(f.dir, "originals", b.SHA256[:2], b.SHA256[2:4], b.SHA256)
}

func (f *fixture) generation(t *testing.T, b blobstore.Blob) int64 {
	t.Helper()
	return f.intent(t, b).Generation
}

func admitted(t *testing.T, gate *Admission) {
	t.Helper()
	release, err := gate.Enter(t.Context())
	if err != nil {
		t.Fatalf("admission after a local refusal: %v", err)
	}
	release()
}

// damage replaces or removes the candidate's own leaf in its healthy shard.
func damage(t *testing.T, leaf, kind string, size int64) {
	t.Helper()
	switch kind {
	case "missing":
		if err := os.Remove(leaf); err != nil {
			t.Fatal(err)
		}
	case "same-size-corrupt", "wrong-size":
		n := size
		if kind == "wrong-size" {
			n++
		}
		if err := os.WriteFile(leaf+".damage", bytes.Repeat([]byte{'!'}, int(n)), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(leaf+".damage", leaf); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown damage %s", kind)
	}
}

// A candidate whose own leaf is proven missing or different is kept with
// its registration and generation, deferred once per process and never
// fences; the candidate queued behind it is reclaimed.
func TestReclamationDefersProvenLocalRefusals(t *testing.T) {
	for _, kind := range []string{"missing", "same-size-corrupt", "wrong-size"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			content := "synthetic detached original " + kind
			bad := f.put(t, content, true)
			f.sql(t, `DELETE FROM attachments WHERE id = $1`, f.attach(t, bad))
			good := f.put(t, "healthy candidate queued behind "+kind, true)
			f.sql(t, `DELETE FROM attachments WHERE id = $1`, f.attach(t, good))
			first := f.generation(t, bad)
			damage(t, f.leaf(bad), kind, bad.Size)
			evidence := treeOf(t, f.dir)

			logs := &logBuffer{}
			c := f.loggingCollector(logs, nil)
			for range 4 {
				if err := c.collectOne(t.Context()); err != nil {
					t.Fatalf("local refusal stopped collection: %v", err)
				}
				admitted(t, c.gate)
			}
			if n := logs.count(CodeDeferred); n != 1 {
				t.Fatalf("deferred %d times in one process, want once:\n%s", n, logs.b.String())
			}
			if !strings.Contains(logs.b.String(), bad.SHA256) {
				t.Fatalf("deferral does not name the hash: %s", logs.b.String())
			}
			f.assertState(t, good, false, false, false)
			assertKept := func() {
				t.Helper()
				if active, err := Pending(t.Context(), store.New(f.db)); err != nil || active != "" {
					t.Fatalf("intent %q %v", active, err)
				}
				if g := f.generation(t, bad); g != first {
					t.Fatalf("generation %d, want %d", g, first)
				}
				known, err := store.New(f.db).OriginalIsRegistered(t.Context(), bad.SHA256)
				if err != nil || !known {
					t.Fatalf("registration lost: %v", err)
				}
				if after := treeOf(t, f.dir); after != strings.Replace(evidence, goodLine(evidence, good), "", 1) {
					t.Fatalf("evidence changed:\n%s\nwant\n%s", after, evidence)
				}
			}
			assertKept()

			// The upload path refuses to recreate or trust the bad leaf.
			if _, err := f.blobs.Put(t.Context(), strings.NewReader(content)); blobstore.Code(err) != blobstore.CodeCorrupt {
				t.Fatalf("same-hash put over a refused candidate: %v", err)
			}
			assertKept()

			// A restart rechecks once, and still does not fence.
			restarted := f.loggingCollector(logs, nil)
			for range 2 {
				if err := restarted.collectOne(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if n := logs.count(CodeDeferred); n != 2 {
				t.Fatalf("restart deferred %d times in total, want 2", n)
			}
			assertKept()

			// A later generation of the same hash is not covered by the
			// earlier skip; the stale skip is pruned.
			f.sql(t, `DELETE FROM attachments WHERE id = $1`, f.attach(t, bad))
			second := f.generation(t, bad)
			if second <= first {
				t.Fatalf("generation %d after %d", second, first)
			}
			if err := c.collectOne(t.Context()); err != nil {
				t.Fatal(err)
			}
			if n := logs.count(CodeDeferred); n != 3 {
				t.Fatalf("new generation was skipped: %d deferrals", n)
			}
			if _, stale := c.deferred[first]; stale || len(c.deferred) != 1 {
				t.Fatalf("deferred set not pruned: %v", c.deferred)
			}
		})
	}
}

func goodLine(tree string, good blobstore.Blob) string {
	for _, line := range strings.SplitAfter(tree, "\n") {
		if strings.Contains(line, good.SHA256+" ") {
			return line
		}
	}
	return ""
}

// The skip belongs to one generation in one process. Once good bytes are
// back, a restart or a new generation reclaims; the old skip alone does not.
func TestReclamationSkipIsPerGenerationAndProcess(t *testing.T) {
	for _, path := range []string{"restart", "new-generation"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t)
			content := "synthetic original restored from elsewhere"
			b := f.put(t, content, true)
			f.sql(t, `DELETE FROM attachments WHERE id = $1`, f.attach(t, b))
			kept := filepath.Join(f.dir, "kept-synthetic-bytes")
			if err := os.Rename(f.leaf(b), kept); err != nil {
				t.Fatal(err)
			}
			c := f.collector(nil)
			if err := c.collectOne(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(kept, f.leaf(b)); err != nil {
				t.Fatal(err)
			}
			if err := c.collectOne(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.assertState(t, b, false, true, true) // still skipped by generation
			switch path {
			case "restart":
				c = f.collector(nil)
			case "new-generation":
				f.sql(t, `DELETE FROM attachments WHERE id = $1`, f.attach(t, b))
			}
			if err := c.collectOne(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.assertState(t, b, false, false, false)
		})
	}
}

// Anything short of a proven local refusal keeps the fencing rule: an
// unexpected entry, a missing or symlinked shard, a close failure joined to
// an absent leaf, or a store mismatch.
func TestReclamationUnarmedUnsafeStateFences(t *testing.T) {
	for _, kind := range []string{"missing-shard", "symlink-shard", "symlink", "directory", "close-error", "wrong-store"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			b, _ := f.candidate(t)
			leaf := f.leaf(b)
			outside := filepath.Join(f.dir, "kept-synthetic-bytes")
			if kind != "wrong-store" {
				if err := os.Rename(leaf, outside); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			switch kind {
			case "missing-shard":
				err = os.Remove(filepath.Dir(leaf))
			case "symlink-shard":
				keptShard := filepath.Join(f.dir, "kept-synthetic-shard")
				if err = os.Rename(filepath.Dir(leaf), keptShard); err == nil {
					err = os.Symlink(keptShard, filepath.Dir(leaf))
				}
			case "symlink":
				err = os.Symlink(outside, leaf)
			case "directory":
				err = os.Mkdir(leaf, 0o755)
			}
			if err != nil {
				t.Fatal(err)
			}
			evidence := treeOf(t, f.dir)
			c := f.collector(nil)
			if kind == "wrong-store" {
				c = New(f.db, uuid.New(), f.vol.Originals(), NewAdmission(), nil, nil)
			}
			if kind == "close-error" {
				c.blobs.SetRemovalSteps(func(r *fsops.Root) blobstore.RemovalSteps {
					return &observedShard{root: r, closeErr: errors.New("injected close failure")}
				})
			}
			err = c.collectOne(t.Context())
			var unsafe *Error
			if !errors.As(err, &unsafe) {
				t.Fatalf("collect = %v, want a fence", err)
			}
			if _, err := c.gate.Enter(t.Context()); !errors.Is(err, ErrStopped) {
				t.Fatalf("admission after unsafe state: %v", err)
			}
			if active, err := Pending(t.Context(), store.New(f.db)); err != nil || active != "" {
				t.Fatalf("intent %q %v", active, err)
			}
			if known, err := store.New(f.db).OriginalIsRegistered(t.Context(), b.SHA256); err != nil || !known {
				t.Fatalf("registration: %t %v", known, err)
			}
			if after := treeOf(t, f.dir); after != evidence {
				t.Fatalf("evidence changed:\n%s\nwant\n%s", after, evidence)
			}
		})
	}
}

// observedShard delegates every step to the real shard root, records the
// steps that succeeded and can inject a failure.
type observedShard struct {
	root              *fsops.Root
	mu                *sync.Mutex
	events            *[]string
	syncErr, closeErr error
}

func (o *observedShard) record(e string) {
	if o.events != nil {
		o.mu.Lock()
		*o.events = append(*o.events, e)
		o.mu.Unlock()
	}
}

func (o *observedShard) Remove(rel string) error {
	err := o.root.Remove(rel)
	if err == nil {
		o.record("unlink")
	}
	return err
}

func (o *observedShard) SyncDir(rel string) error {
	if o.syncErr != nil {
		return o.syncErr
	}
	err := o.root.SyncDir(rel)
	if err == nil && rel == "" {
		o.record("sync")
	}
	return err
}

func (o *observedShard) Close() error {
	err := o.root.Close()
	if err == nil {
		o.record("close")
	}
	if o.closeErr != nil {
		return o.closeErr
	}
	return err
}

// The real removal path performs the unlink (or replays the absence), a
// successful fsync of the shard and its checked close, and only then
// finalizes. A failed fsync or close leaves the intent for replay.
func TestReclamationSyncAndCloseBeforeFinalize(t *testing.T) {
	for _, test := range []struct {
		name             string
		replay           bool
		syncErr, closeEr bool
		want             []string
	}{
		{"collect", false, false, false, []string{"prepared", "unlink", "sync", "close", "finalize"}},
		{"replay-absent", true, false, false, []string{"sync", "close", "finalize"}},
		{"collect-sync-error", false, true, false, []string{"prepared", "unlink", "close"}},
		{"collect-close-error", false, false, true, []string{"prepared", "unlink", "sync", "close"}},
		{"replay-sync-error", true, true, false, []string{"close"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			b, i := f.candidate(t)
			var mu sync.Mutex
			var events []string
			record := func(e string) {
				mu.Lock()
				events = append(events, e)
				mu.Unlock()
			}
			c := f.collector(func(p failpoint.Point) error {
				switch p.Name {
				case "removal_prepared":
					record("prepared")
				case "removal_before_finalize_commit":
					record("finalize")
				}
				return nil
			})
			injected := errors.New("injected durability failure")
			c.blobs.SetRemovalSteps(func(r *fsops.Root) blobstore.RemovalSteps {
				o := &observedShard{root: r, mu: &mu, events: &events}
				if test.syncErr {
					o.syncErr = injected
				}
				if test.closeEr {
					o.closeErr = injected
				}
				return o
			})
			var err error
			if test.replay {
				if err := store.New(f.db).ArmOriginalRemoval(t.Context(), store.ArmOriginalRemovalParams(i)); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(f.leaf(b)); err != nil {
					t.Fatal(err)
				}
				err = c.Recover(t.Context())
			} else {
				err = c.collectOne(t.Context())
			}
			if fmt.Sprint(events) != fmt.Sprint(test.want) {
				t.Fatalf("steps %v, want %v", events, test.want)
			}
			if !test.syncErr && !test.closeEr {
				if err != nil {
					t.Fatal(err)
				}
				f.assertState(t, b, false, false, false)
				return
			}
			if !errors.Is(err, injected) {
				t.Fatalf("durability failure not returned: %v", err)
			}
			f.assertState(t, b, true, true, false)
			if err := f.collector(nil).Recover(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.assertState(t, b, false, false, false)
		})
	}
}

// Randomized committed-reference model across the four roles, rollbacks,
// multi-statement transactions, trash, trash emptying and reattachment.
// After every transaction: a referenced hash is live; an unreferenced hash
// with a row is pending; a row exists only for a hash referenced by some
// earlier commit; a registered orphan and a file-only leftover never get one.
func TestReclamationRandomizedReferenceModel(t *testing.T) {
	f := newFixture(t)
	q := store.New(f.db)
	var hashes []string
	for i := range 6 {
		hashes = append(hashes, f.put(t, fmt.Sprintf("model original %d", i), true).SHA256)
	}
	orphan := f.put(t, "registered orphan never referenced", true).SHA256
	fileOnly := f.put(t, "file-only leftover never registered", false).SHA256
	var artist uuid.UUID
	if err := f.db.QueryRow(t.Context(), `SELECT artist_id FROM albums WHERE id=$1`, f.album).Scan(&artist); err != nil {
		t.Fatal(err)
	}
	albums := []uuid.UUID{f.album}
	everLive := map[string]bool{}
	rng := mrand.New(mrand.NewPCG(20260929, 13))
	pick := func() string { return hashes[rng.IntN(len(hashes))] }
	optional := func() any {
		if rng.IntN(3) == 0 {
			return nil
		}
		return pick()
	}
	no := 0
	for round := range 400 {
		committed := slices.Clone(albums)
		tx, err := f.db.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for range 1 + rng.IntN(4) {
			album := albums[rng.IntN(len(albums))]
			var sql string
			var args []any
			switch rng.IntN(12) {
			case 0, 1:
				no++
				sql, args = `INSERT INTO tracks (id,album_id,disc,no,title,blob_hash,source_path,lyrics_hash) VALUES ($1,$2,$3,$4,'t',$5,'s',$6)`,
					[]any{store.NewID(), album, no/900 + 1, no%900 + 1, pick(), optional()}
			case 2:
				sql = `DELETE FROM tracks WHERE id = (SELECT id FROM tracks ORDER BY random() LIMIT 1)`
			case 3:
				sql, args = `UPDATE tracks SET lyrics_hash=$1 WHERE id = (SELECT id FROM tracks ORDER BY random() LIMIT 1)`, []any{optional()}
			case 4:
				sql, args = `UPDATE tracks SET blob_hash=$1 WHERE id = (SELECT id FROM tracks ORDER BY random() LIMIT 1)`, []any{pick()}
			case 5:
				id := store.NewID()
				sql, args = `INSERT INTO attachments (id,album_id,rel_path,path_key,blob_hash) VALUES ($1,$2,$3,$3,$4)`, []any{id, album, id.String(), pick()}
			case 6:
				sql = `DELETE FROM attachments WHERE id = (SELECT id FROM attachments ORDER BY random() LIMIT 1)`
			case 7:
				sql, args = `UPDATE albums SET cover_hash=$1 WHERE id=$2`, []any{optional(), album}
			case 8:
				sql = `UPDATE tracks SET blob_hash=lyrics_hash, lyrics_hash=blob_hash WHERE lyrics_hash IS NOT NULL AND id = (SELECT id FROM tracks WHERE lyrics_hash IS NOT NULL ORDER BY random() LIMIT 1)`
			case 9:
				sql, args = `UPDATE attachments SET blob_hash=$1 WHERE id = (SELECT id FROM attachments ORDER BY random() LIMIT 1)`, []any{pick()}
			case 10:
				// A new trashed album, as a move or a trash would leave it.
				id := store.NewID()
				albums = append(albums, id)
				sql, args = `INSERT INTO albums (id,artist_id,title,folder_key,revision,deleted_at,cover_hash) VALUES ($1,$2,'B',$3,1,now(),$4)`,
					[]any{id, artist, id.String(), optional()}
			case 11:
				// Empty the trash of one album in one transaction.
				if len(albums) < 2 {
					continue
				}
				victim := albums[len(albums)-1]
				albums = albums[:len(albums)-1]
				for _, stmt := range []string{`DELETE FROM tracks WHERE album_id=$1`, `DELETE FROM attachments WHERE album_id=$1`} {
					if _, err := tx.Exec(t.Context(), stmt, victim); err != nil {
						t.Fatal(err)
					}
				}
				sql, args = `DELETE FROM albums WHERE id=$1`, []any{victim}
			}
			if _, err := tx.Exec(t.Context(), sql, args...); err != nil {
				t.Fatalf("round %d: %s: %v", round, sql, err)
			}
		}
		if rng.IntN(5) == 0 {
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			albums = committed
		} else if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("round %d commit: %v", round, err)
		}
		for _, h := range append([]string{orphan, fileOnly}, hashes...) {
			live, err := q.OriginalHasReferences(t.Context(), h)
			if err != nil {
				t.Fatal(err)
			}
			if live {
				everLive[h] = true
			}
			var gen *int64
			err = f.db.QueryRow(t.Context(), `SELECT generation FROM original_reclamation WHERE hash=$1`, h).Scan(&gen)
			row := !errors.Is(err, pgx.ErrNoRows)
			if err != nil && row {
				t.Fatal(err)
			}
			switch {
			case live && (!row || gen != nil):
				t.Fatalf("round %d: live %s row=%t gen=%v", round, h[:8], row, gen)
			case !live && row && gen == nil:
				t.Fatalf("round %d: unreferenced %s stuck live", round, h[:8])
			case row && !everLive[h]:
				t.Fatalf("round %d: never-live %s has a row", round, h[:8])
			case !live && everLive[h] && !row:
				t.Fatalf("round %d: lost eligibility of %s", round, h[:8])
			}
		}
	}
	if everLive[orphan] || everLive[fileOnly] {
		t.Fatal("model referenced an orphan")
	}
}

// A same-byte put recreates a missing original only for an orphan that
// nothing requires: one older than reclamation, or one whose eligibility a
// restore reset. A reference, an eligible or armed candidate, or a
// reference whose reclamation row was lost still refuses, an existing
// damaged file is never replaced, and a failed lookup never authorizes it.
func TestPutRecreatesOnlyUnrequiredMissingOriginals(t *testing.T) {
	f := newFixture(t)
	q := store.New(f.db)
	detach := func(b blobstore.Blob) { f.sql(t, `DELETE FROM attachments WHERE id = $1`, f.attach(t, b)) }

	// The restore reset runs first: it rewrites every reclamation row.
	restored := f.put(t, "eligible, then reset by a restore", true)
	detach(restored)
	if err := q.RestoreClearOriginalReclamation(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := q.RestoreSeedOriginalReclamation(t.Context()); err != nil {
		t.Fatal(err)
	}
	historical := f.put(t, "historical registered orphan", true)
	live := f.put(t, "live reference", true)
	f.attach(t, live)
	pending := f.put(t, "eligible candidate", true)
	detach(pending)
	armed := f.put(t, "armed candidate", true)
	detach(armed)
	if err := q.ArmOriginalRemoval(t.Context(), store.ArmOriginalRemovalParams(f.intent(t, armed))); err != nil {
		t.Fatal(err)
	}
	lostRow := f.put(t, "live reference whose row was lost", true)
	f.attach(t, lostRow)
	f.sql(t, `DELETE FROM original_reclamation WHERE hash = $1`, lostRow.SHA256)
	uncertain := f.put(t, "orphan checked over a lost connection", true)
	damagedOrphan := f.put(t, "historical orphan, damaged in place", true)

	contents := map[blobstore.Blob]string{
		restored: "eligible, then reset by a restore", historical: "historical registered orphan",
		live: "live reference", pending: "eligible candidate", armed: "armed candidate",
		lostRow: "live reference whose row was lost", uncertain: "orphan checked over a lost connection",
	}
	for b := range contents {
		if err := os.Remove(f.leaf(b)); err != nil {
			t.Fatal(err)
		}
	}
	damage(t, f.leaf(damagedOrphan), "same-size-corrupt", damagedOrphan.Size)
	absent := func(b blobstore.Blob) {
		t.Helper()
		if _, err := os.Lstat(f.leaf(b)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was recreated: %v", b.SHA256, err)
		}
	}

	// A lookup that fails is an error, never an authorization.
	closed := pgtest.Pool(t, f.url)
	closed.Close()
	lost, err := blobstore.NewRegistered(f.vol.Originals(), f.vol.Work(), func(ctx context.Context, hash string) (bool, error) {
		return Registered(ctx, store.New(closed), hash)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lost.Put(t.Context(), strings.NewReader(contents[uncertain])); store.Code(err) != store.CodeConnectionLost {
		t.Fatalf("put with a failed lookup: %v", err)
	}
	absent(uncertain)

	for _, b := range []blobstore.Blob{live, pending, armed, lostRow} {
		if _, err := f.blobs.Put(t.Context(), strings.NewReader(contents[b])); blobstore.Code(err) != blobstore.CodeCorrupt {
			t.Fatalf("put of required missing %q: %v", contents[b], err)
		}
		absent(b)
	}
	before, err := os.ReadFile(f.leaf(damagedOrphan))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.blobs.Put(t.Context(), strings.NewReader("historical orphan, damaged in place")); blobstore.Code(err) != blobstore.CodeCorrupt {
		t.Fatalf("put over a damaged orphan: %v", err)
	}
	if after, err := os.ReadFile(f.leaf(damagedOrphan)); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("damaged orphan replaced: %v", err)
	}

	for _, b := range []blobstore.Blob{historical, restored, uncertain} {
		for range 2 { // recreate, then deduplicate
			if got, err := f.blobs.Put(t.Context(), strings.NewReader(contents[b])); err != nil || got != b {
				t.Fatalf("put of orphan %q: %+v %v", contents[b], got, err)
			}
		}
		if n, err := f.blobs.Verify(t.Context(), b.SHA256); err != nil || n != b.Size {
			t.Fatalf("recreated orphan: %d %v", n, err)
		}
		if _, err := q.ReadOriginalReclamation(t.Context(), b.SHA256); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("orphan became a candidate: %v", err)
		}
	}
	if hash, err := Pending(t.Context(), q); err != nil || hash != armed.SHA256 {
		t.Fatalf("armed intent %q %v", hash, err)
	}
}
