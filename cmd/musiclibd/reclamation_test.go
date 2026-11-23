package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sys/unix"

	"musiclib/internal/blobstore"
	"musiclib/internal/maintenance"
	"musiclib/internal/reclaim"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

func TestDaemonReplaysOriginalRemovalBeforeReadiness(t *testing.T) {
	for _, state := range []string{"present", "absent", "invalid", "corrupt", "missing-shard"} {
		t.Run(state, func(t *testing.T) {
			url := pgtest.EmptyDB(t)
			db := pgtest.Pool(t, url)
			if err := store.Migrate(t.Context(), db); err != nil {
				t.Fatal(err)
			}
			p := testPaths(t)
			v, err := volume.Acquire(p.data)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := v.Identify(t.Context(), db); err != nil {
				t.Fatal(err)
			}
			if err := v.OpenLayout(); err != nil {
				t.Fatal(err)
			}
			blobs, err := blobstore.New(v.Originals(), v.Work())
			if err != nil {
				t.Fatal(err)
			}
			b, err := blobs.Put(t.Context(), strings.NewReader("original synthetic daemon replay fixture"))
			if err != nil {
				t.Fatal(err)
			}
			kept, err := blobs.Put(t.Context(), strings.NewReader("file-only fixture stays"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(t.Context(), `INSERT INTO blobs (hash,size,created_at) VALUES ($1,$2,now())`, b.SHA256, b.Size); err != nil {
				t.Fatal(err)
			}
			var generation int64
			if err := db.QueryRow(t.Context(), `INSERT INTO original_reclamation (hash,generation)
                VALUES ($1,nextval('original_reclamation_generation')) RETURNING generation`, b.SHA256).Scan(&generation); err != nil {
				t.Fatal(err)
			}
			i := store.ArmOriginalRemovalParams{StoreID: v.StoreID(), Hash: b.SHA256, Size: b.Size, Generation: generation}
			if err := store.New(db).ArmOriginalRemoval(t.Context(), i); err != nil {
				t.Fatal(err)
			}
			if err := v.Close(); err != nil {
				t.Fatal(err)
			}
			leaf := filepath.Join(p.data, "originals", b.SHA256[:2], b.SHA256[2:4], b.SHA256)
			if state == "absent" {
				if err := os.Remove(leaf); err != nil {
					t.Fatal(err)
				}
			}
			switch state {
			case "invalid":
				outside := filepath.Join(p.data, "fixture-bytes")
				if err := os.Rename(leaf, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, leaf); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(leaf+".damage", bytes.Repeat([]byte{'!'}, int(b.Size)), 0o444); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(leaf+".damage", leaf); err != nil {
					t.Fatal(err)
				}
			case "missing-shard":
				if err := os.Rename(filepath.Dir(leaf), filepath.Join(p.data, "fixture-shard")); err != nil {
					t.Fatal(err)
				}
			}
			evidence := hashTree(t, filepath.Join(p.data, "originals"))
			tmp := filepath.Join(p.data, "work", "blobs", "interrupted.tmp")
			if err := os.WriteFile(tmp, []byte("only clean after replay"), 0o600); err != nil {
				t.Fatal(err)
			}

			if state != "present" && state != "absent" {
				// Every start refuses the same way and changes nothing:
				// no intent cleared, no file touched, no work admitted.
				for range 2 {
					s := startServerProcess(t, url, p, 0o022)
					if code := s.wait(t); code != exitFailure || !strings.Contains(s.stderr.String(), reclaim.CodeUnsafe) {
						t.Fatalf("%s replay exit %d: %s", state, code, s.stderr)
					}
					if !exists(t, tmp) || s.stderr.has(t, "ready") || s.stderr.has(t, "workers started") || s.stderr.has(t, "journal recovered") || s.stderr.has(t, "no pending publication") {
						t.Fatalf("boot passed original replay: %s", s.stderr)
					}
					if active, err := store.New(db).ReadOriginalRemoval(t.Context()); err != nil || active != store.ReadOriginalRemovalRow(i) {
						t.Fatalf("unsafe replay changed intent: %+v %v", active, err)
					}
					if known, err := store.New(db).OriginalIsRegistered(t.Context(), b.SHA256); err != nil || !known {
						t.Fatalf("unsafe replay changed registration: %t %v", known, err)
					}
					if after := hashTree(t, filepath.Join(p.data, "originals")); !sameTree(after, evidence) {
						t.Fatalf("unsafe replay changed the volume: %v, want %v", after, evidence)
					}
					assertLockFree(t, p.data)
				}
				return
			}
			s := startServerProcess(t, url, p, 0o022)
			s.waitHealthy(t)
			if exists(t, leaf) || exists(t, tmp) {
				t.Fatalf("ready before original replay or work cleanup: %s", s.stderr)
			}
			if hash, err := reclaim.Pending(t.Context(), store.New(db)); err != nil || hash != "" {
				t.Fatalf("ready before finalization: %q %v", hash, err)
			}
			keptLeaf := filepath.Join(p.data, "originals", kept.SHA256[:2], kept.SHA256[2:4], kept.SHA256)
			if contents, err := os.ReadFile(keptLeaf); err != nil || string(contents) != "file-only fixture stays" {
				t.Fatalf("file-only fixture affected: %q %v", contents, err)
			}
			if err := s.cmd.Process.Signal(unix.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if code := s.wait(t); code != exitOK {
				t.Fatalf("shutdown exit %d: %s", code, s.stderr)
			}
			assertLockFree(t, p.data)
		})
	}
}

func TestDaemonJoinsAdmittedCommitAfterHTTPTimeout(t *testing.T) {
	url := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeAlbum(t, p.imports, "Fixture", "Artist", "Album", 1, 0.2)
	d := startDaemon(t, testConfig(url), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	db := pgtest.Pool(t, url)
	d.mustAPI(t, http.MethodPost, "/api/imports", "", map[string]string{"id": store.NewID().String(), "path": ""}, http.StatusCreated)
	waitFor(t, "import and render", func() bool {
		return queryInt(t, db, `SELECT count(*) FROM albums WHERE published_path IS NOT NULL`) == 1
	})
	var album string
	if err := db.QueryRow(t.Context(), `SELECT id::text FROM albums`).Scan(&album); err != nil {
		t.Fatal(err)
	}
	path := "/api/albums/" + album
	initial := d.mustAPI(t, http.MethodGet, path, "", nil, http.StatusOK)
	body := []byte("synthetic attachment held at commit")
	d.upload(t, http.MethodPost, path+"/attachments?path=booklet.txt", initial.header.Get("ETag"), body, http.StatusCreated)
	current := d.mustAPI(t, http.MethodGet, path, "", nil, http.StatusOK)
	var att string
	if err := db.QueryRow(t.Context(), `SELECT id::text FROM attachments`).Scan(&att); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), `CREATE FUNCTION hold_fixture_commit() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN PERFORM pg_advisory_xact_lock(1721303); RETURN NULL; END $$;
        CREATE CONSTRAINT TRIGGER hold_fixture_delete AFTER DELETE ON attachments DEFERRABLE INITIALLY DEFERRED
        FOR EACH ROW EXECUTE FUNCTION hold_fixture_commit()`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), `SELECT pg_advisory_lock(1721303)`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := conn.Exec(t.Context(), `SELECT pg_advisory_unlock(1721303)`); err != nil {
			t.Error(err)
		}
	}()

	req, err := http.NewRequest(http.MethodDelete, d.base+path+"/attachments/"+att, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:8080"
	d.withSession(t, req)
	req.Header.Set("X-Musiclib-Request", "1")
	req.Header.Set("If-Match", current.header.Get("ETag"))
	outcome := make(chan error, 1)
	go func() {
		res, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err == nil {
			err = errors.Join(fmt.Errorf("DELETE unexpectedly returned HTTP %d", res.StatusCode), res.Body.Close())
		}
		outcome <- err
	}()
	waitFor(t, "admitted DELETE is inside COMMIT", func() bool {
		return queryInt(t, db, `SELECT count(*) FROM pg_locks WHERE locktype='advisory'
            AND classid=0 AND objid=1721303 AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database())`) == 1
	})
	d.cancel()
	d.logs.waitLog(t, "http server stopped")
	if err := <-outcome; !errors.Is(err, io.EOF) {
		t.Fatalf("HTTP connection did not close while COMMIT was blocked: %v", err)
	}
	if v, err := volume.Acquire(p.data); volume.Code(err) != volume.CodeLocked {
		if v != nil {
			if err := v.Close(); err != nil {
				t.Error(err)
			}
		}
		t.Fatalf("volume lock released while admitted handler remained in COMMIT: %v", err)
	}
	if _, err := conn.Exec(t.Context(), `SELECT pg_advisory_unlock(1721303)`); err != nil {
		t.Fatal(err)
	}
	if err := d.wait(t); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want HTTP deadline", err)
	}
	assertLockFree(t, p.data)
	hash := sum(body)
	leaf := filepath.Join(p.data, "originals", hash[:2], hash[2:4], hash)
	if got, err := os.ReadFile(leaf); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("shutdown touched the admitted original: %q %v", got, err)
	}
	if _, err := store.New(db).ReadOriginalReclamation(t.Context(), hash); err != nil {
		t.Fatalf("committed DELETE lost its eligibility: %v", err)
	}
	fresh := startDaemon(t, testConfig(url), p)
	fresh.waitStatus(t, "/health/ready", http.StatusOK)
	waitFor(t, "post-restart original reclamation", func() bool {
		known, err := store.New(db).OriginalIsRegistered(t.Context(), hash)
		return err == nil && !known && !exists(t, leaf)
	})
}

func TestDaemonReclamationWaitsForRetainedRenderAndUpload(t *testing.T) {
	url := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeAlbum(t, p.imports, "Fixture", "Artist", "Album", 1, 0.2)
	realFFmpeg := p.ffmpeg
	p.ffmpeg = filepath.Join(t.TempDir(), "ffmpeg")
	// Pause the actual render while it digests the staged audio, before
	// the older snapshot opens its attachment. All native work runs with
	// the pinned binary once the test releases it.
	wrapper := fmt.Sprintf(`#!/bin/bash
if [[ -f "$0.arm" && " $* " == *" pcm_f64le "* ]]; then
    mv "$0.arm" "$0.started"
    while [[ ! -f "$0.release" ]]; do sleep 0.01; done
fi
exec %q "$@"
`, realFFmpeg)
	if err := os.WriteFile(p.ffmpeg, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}

	d := startDaemon(t, testConfig(url), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	db := pgtest.Pool(t, url)
	d.mustAPI(t, http.MethodPost, "/api/imports", "", map[string]string{"id": store.NewID().String(), "path": ""}, http.StatusCreated)
	waitFor(t, "fixture import and render", func() bool {
		return queryInt(t, db, `SELECT count(*) FROM albums WHERE published_path IS NOT NULL`) == 1
	})
	var album string
	if err := db.QueryRow(t.Context(), `SELECT id::text FROM albums`).Scan(&album); err != nil {
		t.Fatal(err)
	}
	path := "/api/albums/" + album
	tag := func() string {
		return d.mustAPI(t, http.MethodGet, path, "", nil, http.StatusOK).header.Get("ETag")
	}
	body := []byte("same-hash deferred attachment")
	hash := sum(body)
	leaf := filepath.Join(p.data, "originals", hash[:2], hash[2:4], hash)
	added := d.upload(t, http.MethodPost, path+"/attachments?path=booklet.txt", tag(), body, http.StatusCreated)
	attachment := strings.TrimSuffix(added.header.Get("Location"), "/content")
	waitFor(t, "attachment publication", func() bool { return idle(t, db) })

	if err := os.WriteFile(p.ffmpeg+".arm", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d.mustAPI(t, http.MethodPost, path+"/render", tag(), nil, http.StatusAccepted)
	waitFor(t, "old render snapshot before attachment read", func() bool {
		return exists(t, p.ffmpeg+".started")
	})
	d.mustAPI(t, http.MethodDelete, attachment, tag(), nil, http.StatusOK)
	first, err := store.New(db).ReadOriginalReclamation(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	assertRetained := func(phase string) {
		t.Helper()
		if got, err := os.ReadFile(leaf); err != nil || !bytes.Equal(got, body) {
			t.Fatalf("%s: retained original = %q, %v", phase, got, err)
		}
	}

	// Cross a complete collector interval and its admission wait while
	// the original is needed only by the claimed render.
	time.Sleep(2500 * time.Millisecond)
	assertRetained("claimed render")

	stream, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	req, err := http.NewRequest(http.MethodPost, d.base+path+"/attachments?path=reattached.txt", stream)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = int64(len(body))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-Musiclib-Request", "1")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("If-Match", tag())
	d.withSession(t, req)
	result := make(chan error, 1)
	go func() {
		res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err == nil {
			defer func() { _ = res.Body.Close() }()
			if res.StatusCode != http.StatusCreated {
				diagnostic, _ := io.ReadAll(res.Body)
				err = fmt.Errorf("reattach upload HTTP %d: %s", res.StatusCode, diagnostic)
			}
		}
		result <- err
	}()
	waitFor(t, "real upload before its hash is known", func() bool {
		entries, err := os.ReadDir(filepath.Join(p.data, "work", "blobs"))
		return err == nil && len(entries) == 1
	})
	if err := os.WriteFile(p.ffmpeg+".release", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both render revisions settled", func() bool { return idle(t, db) })
	if failed := queryInt(t, db, `SELECT count(*) FROM jobs WHERE state='failed'`); failed != 0 {
		t.Fatalf("retained render failed: %s", d.logs)
	}
	time.Sleep(2500 * time.Millisecond)
	assertRetained("upload before hash discovery")
	if _, err := writer.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := store.New(db).ReadOriginalReclamation(t.Context(), hash); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("same-hash reattachment left the old candidate: %v", err)
	}
	assertRetained("committed same-hash reattachment")

	var att string
	if err := db.QueryRow(t.Context(), `SELECT id::text FROM attachments WHERE blob_hash=$1`, hash).Scan(&att); err != nil {
		t.Fatal(err)
	}
	d.mustAPI(t, http.MethodDelete, path+"/attachments/"+att, tag(), nil, http.StatusOK)
	// The collector can race this read. A surviving candidate must have
	// received a new generation, and eventual finalization is required.
	next, err := store.New(db).ReadOriginalReclamation(t.Context(), hash)
	if err == nil && *next.Generation <= *first.Generation {
		t.Fatalf("reused generation %d", *next.Generation)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	waitFor(t, "new generation removed", func() bool {
		known, err := store.New(db).OriginalIsRegistered(t.Context(), hash)
		return err == nil && !known && !exists(t, leaf)
	})
}

// deferrals counts the collector's deferral events for hash.
func deferrals(t *testing.T, logs *syncBuffer, hash string) int {
	t.Helper()
	n := 0
	for _, ev := range logs.events(t) {
		if ev["code"] == reclaim.CodeDeferred && ev["hash"] == hash {
			n++
		}
	}
	return n
}

func running(d *testDaemon) bool {
	select {
	case err := <-d.done:
		d.done <- err
		return false
	default:
		return true
	}
}

// Through the real daemon, detaching the last reference to a damaged track
// original and to a missing extra keeps the server up: both are kept and
// reported, the healthy candidate queued behind them is reclaimed, and two
// restarts neither crash nor change the evidence.
func TestDaemonDefersDamagedCandidatesAndKeepsServing(t *testing.T) {
	url := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeAlbum(t, p.imports, "Fixture", "Artist", "Album", 3, 0.2)
	d := startDaemon(t, testConfig(url), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	db := pgtest.Pool(t, url)
	d.mustAPI(t, http.MethodPost, "/api/imports", "", map[string]string{"id": store.NewID().String(), "path": ""}, http.StatusCreated)
	waitFor(t, "import and render", func() bool {
		return queryInt(t, db, `SELECT count(*) FROM albums WHERE published_path IS NOT NULL`) == 1 && idle(t, db)
	})
	var album, track, damaged string
	if err := db.QueryRow(t.Context(), `SELECT album_id::text, id::text, blob_hash FROM tracks ORDER BY no LIMIT 1`).Scan(&album, &track, &damaged); err != nil {
		t.Fatal(err)
	}
	path := "/api/albums/" + album
	tag := func(d *testDaemon) string {
		return d.mustAPI(t, http.MethodGet, path, "", nil, http.StatusOK).header.Get("ETag")
	}
	lost := []byte("synthetic extra whose original goes missing")
	healthy := []byte("synthetic healthy extra queued last")
	lostAttachment := strings.TrimSuffix(d.upload(t, http.MethodPost, path+"/attachments?path=lost.txt", tag(d), lost, http.StatusCreated).header.Get("Location"), "/content")
	healthyAttachment := strings.TrimSuffix(d.upload(t, http.MethodPost, path+"/attachments?path=healthy.txt", tag(d), healthy, http.StatusCreated).header.Get("Location"), "/content")
	waitFor(t, "attachments published", func() bool { return idle(t, db) })
	leafOf := func(hash string) string { return filepath.Join(p.data, "originals", hash[:2], hash[2:4], hash) }
	missing, good := sum(lost), sum(healthy)

	// Same-size damage of a track original, a vanished extra original.
	info, err := os.Stat(leafOf(damaged))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leafOf(damaged)+".damage", bytes.Repeat([]byte{0x5a}, int(info.Size())), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(leafOf(damaged)+".damage", leafOf(damaged)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(leafOf(missing)); err != nil {
		t.Fatal(err)
	}
	d.mustAPI(t, http.MethodDelete, path+"/tracks/"+track, tag(d), nil, http.StatusOK)
	d.mustAPI(t, http.MethodDelete, lostAttachment, tag(d), nil, http.StatusOK)
	d.mustAPI(t, http.MethodDelete, healthyAttachment, tag(d), nil, http.StatusOK)
	generations := map[string]int64{}
	for _, h := range []string{damaged, missing} {
		row, err := store.New(db).ReadOriginalReclamation(t.Context(), h)
		if err != nil {
			t.Fatal(err)
		}
		generations[h] = *row.Generation
	}
	waitFor(t, "healthy candidate reclaimed behind the damaged ones", func() bool {
		known, err := store.New(db).OriginalIsRegistered(t.Context(), good)
		return err == nil && !known && !exists(t, leafOf(good))
	})
	evidence := hashTree(t, filepath.Join(p.data, "originals"))
	assertKept := func(d *testDaemon) {
		t.Helper()
		if !running(d) {
			t.Fatalf("the daemon stopped: %v\n%s", d.wait(t), d.logs)
		}
		d.waitStatus(t, "/health/ready", http.StatusOK)
		d.mustAPI(t, http.MethodGet, path, "", nil, http.StatusOK)
		if hash, err := reclaim.Pending(t.Context(), store.New(db)); err != nil || hash != "" {
			t.Fatalf("intent %q %v", hash, err)
		}
		for h, g := range generations {
			row, err := store.New(db).ReadOriginalReclamation(t.Context(), h)
			if err != nil || *row.Generation != g {
				t.Fatalf("candidate %s changed: %+v %v", h, row, err)
			}
			if deferrals(t, d.logs, h) != 1 {
				t.Fatalf("%s deferred %d times in this run:\n%s", h, deferrals(t, d.logs, h), d.logs)
			}
		}
		if after := hashTree(t, filepath.Join(p.data, "originals")); !sameTree(after, evidence) {
			t.Fatalf("evidence changed: %v, want %v", after, evidence)
		}
	}
	waitFor(t, "both damaged candidates deferred", func() bool {
		return deferrals(t, d.logs, damaged) == 1 && deferrals(t, d.logs, missing) == 1
	})
	// Two more intervals without a retry or a crash.
	time.Sleep(4500 * time.Millisecond)
	assertKept(d)
	// The same bytes cannot be uploaded over the missing original.
	d.upload(t, http.MethodPost, path+"/attachments?path=again.txt", tag(d), lost, http.StatusInternalServerError)
	assertKept(d)
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}

	for restart := 1; restart <= 2; restart++ {
		r := startDaemon(t, testConfig(url), p)
		r.waitStatus(t, "/health/ready", http.StatusOK)
		waitFor(t, "deferral after restart", func() bool {
			return deferrals(t, r.logs, damaged) == 1 && deferrals(t, r.logs, missing) == 1
		})
		time.Sleep(2500 * time.Millisecond)
		assertKept(r)
		if err := r.stop(t); err != nil {
			t.Fatalf("restart %d: %v", restart, err)
		}
	}

	v, err := volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := v.OpenExistingLayout(); err != nil {
		t.Fatal(err)
	}
	want := map[bool]map[string]string{
		false: {damaged: "info doctor_original_reclamation_eligible", missing: "error doctor_original_reclamation_missing"},
		true:  {damaged: "error doctor_original_reclamation_damaged", missing: "error doctor_original_reclamation_missing"},
	}
	for _, deep := range []bool{false, true} {
		r, err := maintenance.Doctor(t.Context(), db, v, deep)
		if err != nil {
			t.Fatal(err)
		}
		for h, code := range want[deep] {
			var got []string
			for _, f := range r.Findings {
				if f.Entity == h {
					got = append(got, f.Severity+" "+f.Code)
				}
			}
			if len(got) != 1 || got[0] != code {
				t.Errorf("deep=%t %s: %v, want %s", deep, h, got, code)
			}
		}
	}
	if after := hashTree(t, filepath.Join(p.data, "originals")); !sameTree(after, evidence) {
		t.Fatal("doctor changed the evidence")
	}
}

// A download whose client stops reading holds admission: the collector
// defers instead of queuing, so other requests are not held behind it, and
// it reclaims once the client is gone.
func TestDaemonPausedDownloadDefersCollectionOnly(t *testing.T) {
	url := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeAlbum(t, p.imports, "Fixture", "Artist", "Album", 1, 0.2)
	d := startDaemon(t, testConfig(url), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	db := pgtest.Pool(t, url)
	d.mustAPI(t, http.MethodPost, "/api/imports", "", map[string]string{"id": store.NewID().String(), "path": ""}, http.StatusCreated)
	waitFor(t, "import", func() bool {
		return queryInt(t, db, `SELECT count(*) FROM albums WHERE published_path IS NOT NULL`) == 1 && idle(t, db)
	})
	var album string
	if err := db.QueryRow(t.Context(), `SELECT id::text FROM albums`).Scan(&album); err != nil {
		t.Fatal(err)
	}
	path := "/api/albums/" + album
	tag := func() string { return d.mustAPI(t, http.MethodGet, path, "", nil, http.StatusOK).header.Get("ETag") }
	big := bytes.Repeat([]byte("synthetic large extra "), (32<<20)/22)
	bigRes := d.upload(t, http.MethodPost, path+"/attachments?path=big.bin", tag(), big, http.StatusCreated)
	small := []byte("synthetic small extra to reclaim")
	smallRes := d.upload(t, http.MethodPost, path+"/attachments?path=small.txt", tag(), small, http.StatusCreated)
	waitFor(t, "idle", func() bool { return idle(t, db) })

	conn, err := net.Dial("tcp", strings.TrimPrefix(d.base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	c := d.signIn(t)
	if _, err := fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nCookie: %s=%s\r\n\r\n", bigRes.header.Get("Location"), c.Name, c.Value); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, make([]byte, 64<<10)); err != nil {
		t.Fatal(err)
	}
	d.mustAPI(t, http.MethodDelete, strings.TrimSuffix(smallRes.header.Get("Location"), "/content"), tag(), nil, http.StatusOK)
	hash := sum(small)
	leaf := filepath.Join(p.data, "originals", hash[:2], hash[2:4], hash)
	var worst time.Duration
	requests := 0
	for deadline := time.Now().Add(6 * time.Second); time.Now().Before(deadline); requests++ {
		start := time.Now()
		d.mustAPI(t, http.MethodGet, path+"/status", "", nil, http.StatusOK)
		worst = max(worst, time.Since(start))
		time.Sleep(50 * time.Millisecond)
	}
	if got, err := os.ReadFile(leaf); err != nil || !bytes.Equal(got, small) {
		t.Fatalf("reclaimed while a download held admission: %v", err)
	}
	t.Logf("paused download held admission for 6s: %d status requests, worst %s", requests, worst)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	waitFor(t, "reclaim after the client closed", func() bool {
		known, err := store.New(db).OriginalIsRegistered(t.Context(), hash)
		return err == nil && !known && !exists(t, leaf)
	})
	t.Logf("reclaimed %s after the client closed", time.Since(start))
}
