package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"musiclib/internal/fsops"
	"musiclib/internal/maintenance"
	"musiclib/internal/render"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
)

// DESIGN.md §11.3 and §12.2: prove that a real imported album whose bytes
// changed without changing size or mtime is detected, then repaired by the
// real renderer. Rebuild must independently regenerate every active album.
func TestOfflineDoctorRepairAndRebuildConverge(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeAlbum(t, p.imports, "Blue", "Miles Davis", "Kind of Blue", 2, 0.3)
	writeAlbum(t, p.imports, "Portrait", "Bill Evans", "Portrait", 1, 0.3)
	d := startDaemon(t, testConfig(dbURL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	db := dbPool(t, dbURL)
	d.mustAPI(t, http.MethodPost, "/api/imports", "", map[string]string{"id": uuid.NewString(), "path": ""}, http.StatusCreated)
	waitFor(t, "initial publications", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision=revision AND published_path IS NOT NULL`) == 2
	})
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}
	v, err := volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err = v.OpenExistingLayout(); err != nil {
		t.Fatal(err)
	}
	if err = v.CheckFilesystem(); err != nil {
		t.Fatal(err)
	}
	id := v.StoreID()
	var rel string
	if err = db.QueryRow(t.Context(), `SELECT published_path FROM albums WHERE title='Kind of Blue'`).Scan(&rel); err != nil {
		t.Fatal(err)
	}
	file := rel + "/01 - Track 1.flac"
	fi, err := v.Library().Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	f, err := v.Library().OpenFile(file, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	byteAt := make([]byte, 1)
	if _, err := f.ReadAt(byteAt, 0); err != nil {
		t.Fatal(err)
	}
	byteAt[0] ^= 1
	if _, err := f.WriteAt(byteAt, 0); err != nil {
		t.Fatal(err)
	}
	if err := fsops.SyncAndClose(f); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(p.data, "library", filepath.FromSlash(file)), fi.MTime, fi.MTime); err != nil {
		t.Fatal(err)
	}
	normal, err := maintenance.Doctor(t.Context(), db, v, false)
	if err != nil || normal.HasErrors() {
		t.Fatalf("normal doctor: %+v %v", normal, err)
	}
	deep, err := maintenance.Doctor(t.Context(), db, v, true)
	if err != nil || !hasDoctorCode(deep, "doctor_output_hash") {
		t.Fatalf("deep doctor: %+v %v", deep, err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	d = startDaemon(t, testConfig(dbURL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	var albumID uuid.UUID
	if err := db.QueryRow(t.Context(), `SELECT id FROM albums WHERE title='Kind of Blue'`).Scan(&albumID); err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/albums/" + albumID.String()
	cur := d.mustAPI(t, http.MethodGet, endpoint, "", nil, http.StatusOK)
	before := *published(t, db, albumID).PublishedBuild
	d.mustAPI(t, http.MethodPost, endpoint+"/render", cur.header.Get("ETag"), nil, http.StatusAccepted)
	waitFor(t, "repair render", func() bool { return idle(t, db) && *published(t, db, albumID).PublishedBuild != before })
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}
	v, err = volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err = v.OpenExistingLayout(); err != nil {
		t.Fatal(err)
	}
	repaired, err := maintenance.Doctor(t.Context(), db, v, true)
	if err != nil || repaired.HasErrors() {
		t.Fatalf("repaired doctor: %+v %v", repaired, err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	v, err = volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err = maintenance.Rebuild(t.Context(), db, v, id, nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	d = startDaemon(t, testConfig(dbURL), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	waitFor(t, "rebuild publications", func() bool {
		return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision=revision AND published_path IS NOT NULL`) == 2
	})
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}
	v, err = volume.Acquire(p.data)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = v.IdentifyExisting(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err = v.OpenExistingLayout(); err != nil {
		t.Fatal(err)
	}
	clean, err := maintenance.Doctor(t.Context(), db, v, true)
	if err != nil || clean.HasErrors() {
		t.Fatalf("rebuilt doctor: %+v %v", clean, err)
	}
}

func TestRestoreReceiptLineageAcrossDivergingCopies(t *testing.T) {
	ctx := t.Context()
	url := pgtest.EmptyDB(t)
	p := testPaths(t)
	writeAlbum(t, p.imports, "One", "Artist", "Album", 1, 0.3)
	d := startDaemon(t, testConfig(url), p)
	d.waitStatus(t, "/health/ready", http.StatusOK)
	d.mustAPI(t, http.MethodPost, "/api/imports", "", map[string]string{"id": uuid.NewString(), "path": ""}, http.StatusCreated)
	db := dbPool(t, url)
	awaitReceipt := func(db *pgxpool.Pool) {
		t.Helper()
		waitFor(t, "lineage publication", func() bool {
			return idle(t, db) && queryInt(t, db, `SELECT count(*) FROM albums WHERE published_revision=revision AND published_path IS NOT NULL`) == 1
		})
	}
	awaitReceipt(db)
	var albumID, trackID uuid.UUID
	var sourceHash string
	if err := db.QueryRow(ctx, `SELECT album_id, id, blob_hash FROM tracks`).Scan(&albumID, &trackID, &sourceHash); err != nil {
		t.Fatal(err)
	}
	storeID, err := store.New(db).GetStoreID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	read := func(db *pgxpool.Pool, p paths) render.Receipt {
		t.Helper()
		a := published(t, db, albumID)
		dir := filepath.Join(p.data, "library", filepath.FromSlash(*a.PublishedPath))
		b, err := os.ReadFile(filepath.Join(dir, render.ReceiptName))
		if err != nil {
			t.Fatal(err)
		}
		r, err := render.ParseReceipt(b)
		if err != nil || r.SchemaVersion != render.ReceiptSchemaVersion || r.StoreID != storeID || r.AlbumID != albumID ||
			r.BuildID != *a.PublishedBuild || render.ReceiptHash(b) != *a.PublishedReceiptHash || len(r.Files) != 1 ||
			r.Files[0].TrackID != trackID || r.Files[0].SourceBlobSHA256 != sourceHash {
			t.Fatalf("receipt lost lineage or DB anchor: %+v %v", r, err)
		}
		return r
	}
	original := read(db, p)
	if err := d.stop(t); err != nil {
		t.Fatal(err)
	}
	withVolume := func(p paths, fn func(*volume.Volume)) {
		t.Helper()
		v, err := volume.Acquire(p.data)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := v.Close(); err != nil {
				t.Error(err)
			}
		}()
		fn(v)
	}
	identify := func(db *pgxpool.Pool, v *volume.Volume) {
		t.Helper()
		if id, err := v.IdentifyExisting(ctx, db); err != nil || id != storeID {
			t.Fatalf("volume identity: %s %v, want %s", id, err, storeID)
		}
		if err := v.OpenExistingLayout(); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "archive")
	withVolume(p, func(v *volume.Volume) {
		identify(db, v)
		if err := maintenance.Backup(ctx, db, v, archive, url, nil); err != nil {
			t.Fatal(err)
		}
	})
	var copies []render.Receipt
	for _, title := range []string{"Branch One", "Branch Two"} {
		freshURL := pgtest.EmptyDB(t)
		freshDB := pgtest.Pool(t, freshURL)
		freshPaths := testPaths(t)
		withVolume(freshPaths, func(v *volume.Volume) {
			if err := maintenance.Restore(ctx, freshDB, v, archive, freshURL, nil); err != nil {
				t.Fatal(err)
			}
			a := published(t, freshDB, albumID)
			if a.PublishedPath != nil || a.PublishedBuild != nil || a.PublishedReceiptHash != nil {
				t.Fatal("restore retained derived publication state")
			}
		})
		up := startDaemon(t, testConfig(freshURL), freshPaths)
		up.waitStatus(t, "/health/ready", http.StatusOK)
		awaitReceipt(freshDB)
		restored := read(freshDB, freshPaths)
		if restored.BuildID == original.BuildID || restored.Files[0] != original.Files[0] {
			t.Fatal("restore must regenerate the inventory without changing track identity or bytes")
		}
		endpoint := "/api/albums/" + albumID.String()
		current := up.mustAPI(t, http.MethodGet, endpoint, "", nil, http.StatusOK)
		body := acceptancePut(current.body)
		body["title"] = title
		up.mustAPI(t, http.MethodPut, endpoint, current.header.Get("ETag"), body, http.StatusOK)
		awaitReceipt(freshDB)
		copies = append(copies, read(freshDB, freshPaths))
		if err := up.stop(t); err != nil {
			t.Fatal(err)
		}
		withVolume(freshPaths, func(v *volume.Volume) {
			identify(freshDB, v)
			report, err := maintenance.Doctor(ctx, freshDB, v, true)
			if err != nil || report.HasErrors() {
				t.Fatalf("deep doctor for %s: %+v %v", title, report, err)
			}
		})
	}
	if copies[0].AlbumRevision != copies[1].AlbumRevision || copies[0].Files[0].SHA256 == copies[1].Files[0].SHA256 {
		t.Fatal("copies at the same revision did not diverge in output while preserving lineage")
	}
}

func hasDoctorCode(r maintenance.Report, code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}
