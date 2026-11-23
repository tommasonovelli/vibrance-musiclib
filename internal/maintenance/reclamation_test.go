package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"musiclib/internal/blobstore"
	"musiclib/internal/buildinfo"
	"musiclib/internal/fsops"
	"musiclib/internal/reclaim"
	"musiclib/internal/render"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
	"musiclib/internal/volume"
	"musiclib/migrations"
)

func addReclamationOriginals(t *testing.T, db *pgxpool.Pool, v *volume.Volume, album uuid.UUID, drop bool) (blobstore.Blob, blobstore.Blob) {
	t.Helper()
	bs, err := blobstore.New(v.Originals(), v.Work())
	if err != nil {
		t.Fatal(err)
	}
	put := func(content string, register bool) blobstore.Blob {
		t.Helper()
		b, err := bs.Put(t.Context(), strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		if register {
			if _, err := db.Exec(t.Context(), `INSERT INTO blobs (hash,size,created_at) VALUES ($1,$2,now())`, b.SHA256, b.Size); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	next := put("historical extra or removable original", true)
	failed := put("failed file-only upload", false)
	if drop {
		id := store.NewID()
		if _, err := db.Exec(t.Context(), `INSERT INTO attachments (id,album_id,rel_path,path_key,blob_hash)
            VALUES ($1,$2,'extra','extra',$3)`, id, album, next.SHA256); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(t.Context(), `DELETE FROM attachments WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
	}
	return next, failed
}

// A real PostgreSQL dump with a consistent manifest for the two adversarial
// archive cases Backup cannot produce. Every file is from the synthetic source.
func writeFixtureArchive(t *testing.T, db *pgxpool.Pool, v *volume.Volume, version int64, blobs []BackupBlob) string {
	t.Helper()
	dir := t.TempDir()
	uri, password, err := pgConnection(db.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/usr/lib/postgresql/17/bin/pg_dump", "--format=custom", "--no-owner", "--no-acl",
		"--dbname="+uri, "--file="+filepath.Join(dir, backupDump))
	cmd.Env = append(os.Environ(), "PGPASSWORD="+password)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pg_dump of fixture: %s %v", safeToolStderr(out, password), err)
	}
	dump, err := os.ReadFile(filepath.Join(dir, backupDump))
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blobs {
		rel := filepath.Join("originals", b.SHA256[:2], b.SHA256[2:4], b.SHA256)
		f, err := v.Root().Open(rel)
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(f)
		if err := errors.Join(err, f.Close()); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), content, 0o444); err != nil {
			t.Fatal(err)
		}
	}
	slices.SortFunc(blobs, func(a, b BackupBlob) int { return strings.Compare(a.SHA256, b.SHA256) })
	sum := sha256.Sum256(dump)
	m := Manifest{StoreID: v.StoreID(), SchemaVersion: version, AppVersion: buildinfo.Version,
		RenderVersion: render.Version, DumpSHA256: hex.EncodeToString(sum[:]), Blobs: blobs}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backupManifest), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReclamationOfflineArchivePolicy(t *testing.T) {
	v, db, _, _ := doctorFixture(t)
	var album uuid.UUID
	if err := db.QueryRow(t.Context(), `SELECT id FROM albums`).Scan(&album); err != nil {
		t.Fatal(err)
	}
	eligible, failed := addReclamationOriginals(t, db, v, album, true)
	q := store.New(db)
	i, err := q.ReadOriginalReclamation(t.Context(), eligible.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	backup := filepath.Join(dir, "valid")
	if err := Backup(t.Context(), db, v, backup, db.Config().ConnString(), nil); err != nil {
		t.Fatal(err)
	}
	qIntent := store.ArmOriginalRemovalParams{StoreID: v.StoreID(), Hash: i.Hash, Size: i.Size, Generation: *i.Generation}
	if err := q.ArmOriginalRemoval(t.Context(), qIntent); err != nil {
		t.Fatal(err)
	}

	checkRefusal := func(err error) {
		t.Helper()
		var me *Error
		if !errors.As(err, &me) || me.Code != reclaim.CodePending || !me.Refusal {
			t.Fatalf("armed offline operation: %v", err)
		}
	}
	checkRefusal(Backup(t.Context(), db, v, filepath.Join(dir, "refused"), db.Config().ConnString(), nil))
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 || entries[0].Name() != "valid" {
		t.Fatalf("refused backup wrote entries: %v %v", entries, err)
	}
	checkRefusal(Rebuild(t.Context(), db, v, v.StoreID(), nil))
	if err := v.CheckMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := v.Library().CheckAccess(false); err != nil {
		t.Fatal(err)
	}
	if report, err := Doctor(t.Context(), db, v, true); err != nil || !hasFinding(report, "doctor_original_removal_pending") {
		t.Fatalf("armed doctor: %+v %v", report, err)
	}
	raw, err := os.ReadFile(filepath.Join(backup, backupManifest))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	armed := writeFixtureArchive(t, db, v, m.SchemaVersion, m.Blobs)
	for _, archive := range []string{armed, backup} {
		t.Run(filepath.Base(archive), func(t *testing.T) {
			restoredURL := pgtest.EmptyDB(t)
			restored := pgtest.Pool(t, restoredURL)
			fresh, err := volume.Acquire(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := fresh.Close(); err != nil {
					t.Error(err)
				}
			}()
			err = Restore(t.Context(), restored, fresh, archive, restoredURL, nil)
			if archive == armed {
				var me *Error
				if !errors.As(err, &me) || me.Code != "restore_original_removal_pending" || volume.Code(fresh.CheckMaintenance()) != volume.CodeMaintenance {
					t.Fatalf("restore armed archive: %v, marker=%v", err, fresh.CheckMaintenance())
				}
				if _, err := fresh.Root().Stat(volume.Originals); fsops.Code(err) != fsops.CodeNotFound {
					t.Fatalf("armed archive installed originals: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := fresh.CheckMaintenance(); err != nil {
				t.Fatal(err)
			}
			var live int
			if err := restored.QueryRow(t.Context(), `SELECT count(*) FROM original_reclamation WHERE generation IS NULL`).Scan(&live); err != nil || live != 1 {
				t.Fatalf("restored live count %d: %v", live, err)
			}
			if _, err := store.New(restored).NextOriginalReclamation(t.Context(), nil); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("restored unarmed candidate: %v", err)
			}
			for _, b := range []blobstore.Blob{eligible, failed} {
				if got, err := blobstore.OpenReadOnly(fresh.Originals()).Verify(t.Context(), b.SHA256); err != nil || got != b.Size {
					t.Fatalf("restored original %s: %d %v", b.SHA256, got, err)
				}
			}
		})
	}
	if got, err := os.ReadFile(filepath.Join(backup, backupManifest)); err != nil || string(got) != string(raw) {
		t.Fatalf("valid archive changed during restore: %v", err)
	}
	if pending, err := reclaim.Pending(t.Context(), q); err != nil || pending != eligible.SHA256 {
		t.Fatalf("offline operation changed the source intent: %s %v", pending, err)
	}
}

func TestReclamationPopulatedSchemaThreeUpgradeAndRestore(t *testing.T) {
	db := pgtest.Pool(t, pgtest.EmptyDB(t))
	oldDB := stdlib.OpenDBFromPool(db)
	defer func() {
		if err := oldDB.Close(); err != nil {
			t.Error(err)
		}
	}()
	gooseProvider, err := goose.NewProvider(goose.DialectPostgres, oldDB, migrations.FS, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gooseProvider.UpTo(t.Context(), 3); err != nil {
		t.Fatal(err)
	}
	v, err := volume.Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := v.Identify(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	if err := v.OpenLayout(); err != nil {
		t.Fatal(err)
	}
	bs, err := blobstore.New(v.Originals(), v.Work())
	if err != nil {
		t.Fatal(err)
	}
	original, err := bs.Put(t.Context(), strings.NewReader("trashed live fixture"))
	if err != nil {
		t.Fatal(err)
	}
	artist, album, track := store.NewID(), store.NewID(), store.NewID()
	for _, cmd := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO blobs (hash,size,created_at) VALUES ($1,$2,now())`, []any{original.SHA256, original.Size}},
		{`INSERT INTO artists (id,name,folder_key,revision) VALUES ($1,'Artist','artist',1)`, []any{artist}},
		{`INSERT INTO albums (id,artist_id,title,folder_key,revision,deleted_at) VALUES ($1,$2,'Fixture','fixture',1,now())`, []any{album, artist}},
		{`INSERT INTO tracks (id,album_id,disc,no,title,blob_hash,source_path) VALUES ($1,$2,1,1,'Track',$3,'one.flac')`, []any{track, album, original.SHA256}},
	} {
		if _, err := db.Exec(t.Context(), cmd.sql, cmd.args...); err != nil {
			t.Fatal(err)
		}
	}
	historical, failed := addReclamationOriginals(t, db, v, album, false)
	all := []BackupBlob{{SHA256: original.SHA256, Size: original.Size}, {SHA256: historical.SHA256, Size: historical.Size}, {SHA256: failed.SHA256, Size: failed.Size}}
	archive := writeFixtureArchive(t, db, v, 3, all)

	check := func(ctx context.Context, pg *pgxpool.Pool, vol *volume.Volume) {
		t.Helper()
		var actual uuid.UUID
		if err := pg.QueryRow(ctx, `SELECT id FROM tracks WHERE album_id=$1`, album).Scan(&actual); err != nil || actual != track || vol.StoreID() != v.StoreID() {
			t.Fatalf("identity changed: track=%s store=%s err=%v", actual, vol.StoreID(), err)
		}
		var live string
		if err := pg.QueryRow(ctx, `SELECT hash FROM original_reclamation`).Scan(&live); err != nil || live != original.SHA256 {
			t.Fatalf("migration/restore did not seed live original in trash: %s %v", live, err)
		}
		if _, err := store.New(pg).NextOriginalReclamation(ctx, nil); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("migration/restore manufactured a candidate: %v", err)
		}
		for _, b := range all {
			if got, err := blobstore.OpenReadOnly(vol.Originals()).Verify(ctx, b.SHA256); err != nil || got != b.Size {
				t.Fatalf("original %s: %d %v", b.SHA256, got, err)
			}
		}
	}
	if err := store.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	check(t.Context(), db, v)

	destURL := pgtest.EmptyDB(t)
	dest := pgtest.Pool(t, destURL)
	restored, err := volume.Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := Restore(t.Context(), dest, restored, archive, destURL, nil); err != nil {
		t.Fatal(err)
	}
	check(t.Context(), dest, restored)
}

// reclamationCandidate pins content, attaches and detaches it, and returns
// it with its intent parameters.
func reclamationCandidate(t *testing.T, db *pgxpool.Pool, v *volume.Volume, album uuid.UUID, content string) (blobstore.Blob, store.ArmOriginalRemovalParams) {
	t.Helper()
	bs, err := blobstore.New(v.Originals(), v.Work())
	if err != nil {
		t.Fatal(err)
	}
	b, err := bs.Put(t.Context(), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	id := store.NewID()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO blobs (hash,size,created_at) VALUES ($1,$2,now())`, []any{b.SHA256, b.Size}},
		{`INSERT INTO attachments (id,album_id,rel_path,path_key,blob_hash) VALUES ($1,$2,$3,$3,$4)`, []any{id, album, id.String(), b.SHA256}},
		{`DELETE FROM attachments WHERE id=$1`, []any{id}},
	} {
		if _, err := db.Exec(t.Context(), stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	row, err := store.New(db).ReadOriginalReclamation(t.Context(), b.SHA256)
	if err != nil || row.Generation == nil {
		t.Fatalf("candidate %s: %v", b.SHA256, err)
	}
	return b, store.ArmOriginalRemovalParams{StoreID: v.StoreID(), Hash: b.SHA256, Size: b.Size, Generation: *row.Generation}
}

func leafPath(dir, hash string) string {
	return filepath.Join(dir, volume.Originals, hash[:2], hash[2:4], hash)
}

func corruptSameSize(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".damage", []byte(strings.Repeat("!", int(info.Size()))), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".damage", path); err != nil {
		t.Fatal(err)
	}
}

func findings(r Report, entity string) []string {
	var out []string
	for _, f := range r.Findings {
		if f.Entity == entity {
			out = append(out, f.Severity+" "+f.Code)
		}
	}
	slices.Sort(out)
	return out
}

func findingAdvice(r Report, code string) string {
	for _, f := range r.Findings {
		if f.Code == code {
			return f.Advice
		}
	}
	return ""
}

// sourceState is every originals entry with type, mode and content hash,
// and the catalog rows that reclamation and recovery could change.
func sourceState(t *testing.T, db *pgxpool.Pool, dir string) string {
	t.Helper()
	var out strings.Builder
	err := filepath.WalkDir(filepath.Join(dir, volume.Originals), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out.WriteString(strings.TrimPrefix(path, dir) + " " + info.Mode().String())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			out.WriteString(" " + hex.EncodeToString(sum[:]))
		}
		out.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var rows string
	if err := db.QueryRow(t.Context(), `SELECT coalesce(string_agg(x, E'\n' ORDER BY x), '') FROM (
        SELECT 'blob '||hash||' '||size FROM blobs
        UNION ALL SELECT 'candidate '||hash||' '||coalesce(generation::text,'live') FROM original_reclamation
        UNION ALL SELECT 'intent '||store_id||' '||hash||' '||size||' '||generation FROM original_removal
        UNION ALL SELECT 'attachment '||id||' '||blob_hash FROM attachments
        UNION ALL SELECT 'track '||id||' '||blob_hash FROM tracks) s(x)`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return out.String() + rows
}

// Doctor separates retained, eligible, missing, damaged and armed
// originals; deep is needed for same-size damage. An unreplayable armed
// removal blocks backup and rebuild, and a known-good earlier backup
// restores into a new empty installation without touching the source.
func TestReclamationDoctorAndFreshTargetRecovery(t *testing.T) {
	v, db, dir, check := doctorFixture(t)
	var album uuid.UUID
	if err := db.QueryRow(t.Context(), `SELECT id FROM albums`).Scan(&album); err != nil {
		t.Fatal(err)
	}
	historical, _ := addReclamationOriginals(t, db, v, album, false)
	eligible, _ := reclamationCandidate(t, db, v, album, "eligible and intact")
	known := filepath.Join(t.TempDir(), "known-good")
	if err := Backup(t.Context(), db, v, known, db.Config().ConnString(), nil); err != nil {
		t.Fatal(err)
	}
	rawManifest, err := os.ReadFile(filepath.Join(known, backupManifest))
	if err != nil {
		t.Fatal(err)
	}

	// Changes after the known-good backup, then damage.
	missing, _ := reclamationCandidate(t, db, v, album, "eligible, later missing")
	damaged, _ := reclamationCandidate(t, db, v, album, "eligible, later damaged")
	armed, intent := reclamationCandidate(t, db, v, album, "armed, later damaged")
	if err := os.Remove(leafPath(dir, missing.SHA256)); err != nil {
		t.Fatal(err)
	}
	corruptSameSize(t, leafPath(dir, damaged.SHA256))
	if err := store.New(db).ArmOriginalRemoval(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	corruptSameSize(t, leafPath(dir, armed.SHA256))
	before := sourceState(t, db, dir)

	plain, deep := check(false), check(true)
	for _, c := range []struct {
		name        string
		hash        string
		plain, deep []string
	}{
		{"historical orphan", historical.SHA256, []string{"info doctor_unreferenced_blob"}, []string{"info doctor_unreferenced_blob"}},
		{"eligible", eligible.SHA256, []string{"info doctor_original_reclamation_eligible"}, []string{"info doctor_original_reclamation_eligible"}},
		{"missing", missing.SHA256, []string{"error doctor_original_reclamation_missing"}, []string{"error doctor_original_reclamation_missing"}},
		{"same-size damage", damaged.SHA256, []string{"info doctor_original_reclamation_eligible"}, []string{"error doctor_original_reclamation_damaged"}},
		{"armed same-size damage", armed.SHA256, []string{"warning doctor_original_removal_pending"}, []string{"error doctor_original_removal_unreplayable"}},
	} {
		if got := findings(plain, c.hash); !slices.Equal(got, c.plain) {
			t.Errorf("%s: plain doctor %v, want %v", c.name, got, c.plain)
		}
		if got := findings(deep, c.hash); !slices.Equal(got, c.deep) {
			t.Errorf("%s: deep doctor %v, want %v", c.name, got, c.deep)
		}
	}
	if advice := findingAdvice(plain, "doctor_original_removal_pending"); !strings.Contains(advice, "doctor --deep") {
		t.Errorf("plain pending advice does not ask for deep verification: %q", advice)
	}
	if advice := findingAdvice(deep, "doctor_original_removal_unreplayable"); !strings.Contains(advice, "new empty installation") {
		t.Errorf("unreplayable advice: %q", advice)
	}

	var refusal *Error
	backupDir := t.TempDir()
	if err := Backup(t.Context(), db, v, filepath.Join(backupDir, "refused"), db.Config().ConnString(), nil); !errors.As(err, &refusal) || refusal.Code != reclaim.CodePending {
		t.Fatalf("backup with an armed removal: %v", err)
	}
	if entries, err := os.ReadDir(backupDir); err != nil || len(entries) != 0 {
		t.Fatalf("refused backup wrote %v %v", entries, err)
	}
	if err := Rebuild(t.Context(), db, v, v.StoreID(), nil); !errors.As(err, &refusal) || refusal.Code != reclaim.CodePending {
		t.Fatalf("rebuild with an armed removal: %v", err)
	}
	if err := v.CheckMaintenance(); err != nil {
		t.Fatal(err)
	}

	// Recovery: the known-good backup into a new empty installation.
	freshURL := pgtest.EmptyDB(t)
	freshDB := pgtest.Pool(t, freshURL)
	freshDir := t.TempDir()
	fresh, err := volume.Acquire(freshDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := fresh.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := Restore(t.Context(), freshDB, fresh, known, freshURL, nil); err != nil {
		t.Fatal(err)
	}
	if fresh.StoreID() != v.StoreID() {
		t.Fatalf("restored lineage %s, want %s", fresh.StoreID(), v.StoreID())
	}
	var m Manifest
	if err := json.Unmarshal(rawManifest, &m); err != nil {
		t.Fatal(err)
	}
	for _, b := range m.Blobs {
		if got, err := blobstore.OpenReadOnly(fresh.Originals()).Verify(t.Context(), b.SHA256); err != nil || got != b.Size {
			t.Fatalf("restored original %s: %d %v", b.SHA256, got, err)
		}
	}
	if len(m.Blobs) != 4 { // track, historical, failed upload, eligible
		t.Fatalf("known-good archive has %d originals", len(m.Blobs))
	}
	fq := store.New(freshDB)
	if hash, err := reclaim.Pending(t.Context(), fq); err != nil || hash != "" {
		t.Fatalf("restored intent %q %v", hash, err)
	}
	if _, err := fq.NextOriginalReclamation(t.Context(), nil); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("restored eligibility was not reset: %v", err)
	}
	for _, b := range []blobstore.Blob{missing, damaged, armed} {
		if registered, err := fq.OriginalIsRegistered(t.Context(), b.SHA256); err != nil || registered {
			t.Fatalf("post-backup original %s restored: %t %v", b.SHA256, registered, err)
		}
	}
	r, err := Doctor(t.Context(), freshDB, fresh, true)
	if err != nil || r.HasErrors() {
		t.Fatalf("restored deep doctor: %+v %v", r, err)
	}
	if got, err := os.ReadFile(filepath.Join(known, backupManifest)); err != nil || string(got) != string(rawManifest) {
		t.Fatalf("known-good archive changed: %v", err)
	}
	if after := sourceState(t, db, dir); after != before {
		t.Fatalf("diagnosis or recovery changed the failed source:\n%s\nwant\n%s", after, before)
	}
}

// Doctor repeats each replay precondition read-only.
func TestReclamationDoctorArmedReplayChecks(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(t *testing.T, db *pgxpool.Pool, dir string, b blobstore.Blob, i *store.ArmOriginalRemovalParams)
	}{
		{"intact", "warning doctor_original_removal_pending", nil},
		{"absent leaf", "warning doctor_original_removal_pending", func(t *testing.T, _ *pgxpool.Pool, dir string, b blobstore.Blob, _ *store.ArmOriginalRemovalParams) {
			if err := os.Remove(leafPath(dir, b.SHA256)); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing shard", "error doctor_original_removal_unreplayable", func(t *testing.T, _ *pgxpool.Pool, dir string, b blobstore.Blob, _ *store.ArmOriginalRemovalParams) {
			if err := os.Remove(leafPath(dir, b.SHA256)); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Dir(leafPath(dir, b.SHA256))); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong size", "error doctor_original_removal_unreplayable", func(t *testing.T, _ *pgxpool.Pool, dir string, b blobstore.Blob, _ *store.ArmOriginalRemovalParams) {
			if err := os.Remove(leafPath(dir, b.SHA256)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(leafPath(dir, b.SHA256), []byte("!"), 0o444); err != nil {
				t.Fatal(err)
			}
		}},
		{"wrong store", "error doctor_original_removal_unreplayable", func(_ *testing.T, _ *pgxpool.Pool, _ string, _ blobstore.Blob, i *store.ArmOriginalRemovalParams) {
			i.StoreID = store.NewID()
		}},
		{"wrong generation", "error doctor_original_removal_unreplayable", func(_ *testing.T, _ *pgxpool.Pool, _ string, _ blobstore.Blob, i *store.ArmOriginalRemovalParams) {
			i.Generation++
		}},
		{"live reference", "error doctor_original_removal_unreplayable", func(t *testing.T, db *pgxpool.Pool, _ string, b blobstore.Blob, i *store.ArmOriginalRemovalParams) {
			var album uuid.UUID
			if err := db.QueryRow(t.Context(), `SELECT id FROM albums`).Scan(&album); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(t.Context(), `UPDATE albums SET cover_hash=$1 WHERE id=$2`, b.SHA256, album); err != nil {
				t.Fatal(err)
			}
			// Catalog evidence inconsistent with the reference.
			if _, err := db.Exec(t.Context(), `UPDATE original_reclamation SET generation=$1 WHERE hash=$2`, i.Generation, b.SHA256); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, db, dir, check := doctorFixture(t)
			var album uuid.UUID
			if err := db.QueryRow(t.Context(), `SELECT id FROM albums`).Scan(&album); err != nil {
				t.Fatal(err)
			}
			b, i := reclamationCandidate(t, db, v, album, "armed doctor fixture")
			if tc.change != nil {
				tc.change(t, db, dir, b, &i)
			}
			if err := store.New(db).ArmOriginalRemoval(t.Context(), i); err != nil {
				t.Fatal(err)
			}
			before := sourceState(t, db, dir)
			for _, deep := range []bool{false, true} {
				if got := findings(check(deep), b.SHA256); !slices.Equal(got, []string{tc.want}) {
					t.Errorf("deep=%t: %v, want %s", deep, got, tc.want)
				}
			}
			if after := sourceState(t, db, dir); after != before {
				t.Fatal("doctor changed the source")
			}
		})
	}
}

// A missing orphan that waits for removal refuses a same-byte put, while
// an older orphan does not. A backup restored into a new installation
// resets the waiting one's eligibility, so its put recreates it there.
func TestReclamationMissingOrphanPutAfterRestore(t *testing.T) {
	v, db, dir, _ := doctorFixture(t)
	var album uuid.UUID
	if err := db.QueryRow(t.Context(), `SELECT id FROM albums`).Scan(&album); err != nil {
		t.Fatal(err)
	}
	const historicalContent, waitingContent = "historical extra or removable original", "eligible, later missing"
	historical, _ := addReclamationOriginals(t, db, v, album, false)
	waiting, _ := reclamationCandidate(t, db, v, album, waitingContent)
	for _, b := range []blobstore.Blob{historical, waiting} {
		if err := os.Remove(leafPath(dir, b.SHA256)); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(t.TempDir(), "backup")
	if err := Backup(t.Context(), db, v, archive, db.Config().ConnString(), nil); err != nil {
		t.Fatal(err)
	}
	online := func(db *pgxpool.Pool, v *volume.Volume) *blobstore.Store {
		t.Helper()
		bs, err := blobstore.NewRegistered(v.Originals(), v.Work(), func(ctx context.Context, hash string) (bool, error) {
			return reclaim.Registered(ctx, store.New(db), hash)
		})
		if err != nil {
			t.Fatal(err)
		}
		return bs
	}

	freshURL := pgtest.EmptyDB(t)
	freshDB := pgtest.Pool(t, freshURL)
	freshDir := t.TempDir()
	fresh, err := volume.Acquire(freshDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := fresh.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := Restore(t.Context(), freshDB, fresh, archive, freshURL, nil); err != nil {
		t.Fatal(err)
	}
	restored := online(freshDB, fresh)
	for _, c := range []struct {
		b       blobstore.Blob
		content string
	}{{historical, historicalContent}, {waiting, waitingContent}} {
		if got, err := restored.Put(t.Context(), strings.NewReader(c.content)); err != nil || got != c.b {
			t.Fatalf("restored put of %q: %+v %v", c.content, got, err)
		}
		if n, err := restored.Verify(t.Context(), c.b.SHA256); err != nil || n != c.b.Size {
			t.Fatalf("restored original %s: %d %v", c.b.SHA256, n, err)
		}
	}
	if r, err := Doctor(t.Context(), freshDB, fresh, true); err != nil || r.HasErrors() {
		t.Fatalf("restored deep doctor: %+v %v", r, err)
	}

	source := online(db, v)
	if _, err := source.Put(t.Context(), strings.NewReader(waitingContent)); blobstore.Code(err) != blobstore.CodeCorrupt {
		t.Fatalf("source put of a waiting missing original: %v", err)
	}
	if _, err := os.Lstat(leafPath(dir, waiting.SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("waiting original recreated: %v", err)
	}
	if got, err := source.Put(t.Context(), strings.NewReader(historicalContent)); err != nil || got != historical {
		t.Fatalf("source put of a historical orphan: %+v %v", got, err)
	}
}

// Doctor reports an eligible original as the collector would treat it: a
// missing name or a regular file of another size or content in a verified
// shard is a local skip; a missing or unsafe shard or an unexpected entry
// stops the app. Doctor changes nothing in either case.
func TestReclamationDoctorUnarmedSkipAndFence(t *testing.T) {
	for _, tc := range []struct {
		name, plain, deep string
	}{
		{"missing leaf", "error doctor_original_reclamation_missing", "error doctor_original_reclamation_missing"},
		{"wrong size", "error doctor_original_reclamation_damaged", "error doctor_original_reclamation_damaged"},
		{"same-size damage", "info doctor_original_reclamation_eligible", "error doctor_original_reclamation_damaged"},
		{"missing shard", "error doctor_original_reclamation_unsafe", "error doctor_original_reclamation_unsafe"},
		{"symlink shard", "error doctor_original_reclamation_unsafe", "error doctor_original_reclamation_unsafe"},
		{"symlink", "error doctor_original_reclamation_unsafe", "error doctor_original_reclamation_unsafe"},
		{"directory", "error doctor_original_reclamation_unsafe", "error doctor_original_reclamation_unsafe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, db, dir, check := doctorFixture(t)
			var album uuid.UUID
			if err := db.QueryRow(t.Context(), `SELECT id FROM albums`).Scan(&album); err != nil {
				t.Fatal(err)
			}
			b, _ := reclamationCandidate(t, db, v, album, "eligible doctor fixture")
			leaf := leafPath(dir, b.SHA256)
			shard := filepath.Dir(leaf)
			kept := filepath.Join(dir, "kept-synthetic-bytes")
			var err error
			switch tc.name {
			case "missing leaf":
				err = os.Remove(leaf)
			case "wrong size":
				if err = os.Remove(leaf); err == nil {
					err = os.WriteFile(leaf, []byte("!"), 0o444)
				}
			case "same-size damage":
				corruptSameSize(t, leaf)
			case "missing shard":
				if err = os.Remove(leaf); err == nil {
					err = os.Remove(shard)
				}
			case "symlink shard":
				if err = os.Rename(shard, kept); err == nil {
					err = os.Symlink(kept, shard)
				}
			case "symlink":
				if err = os.Rename(leaf, kept); err == nil {
					err = os.Symlink(kept, leaf)
				}
			case "directory":
				if err = os.Remove(leaf); err == nil {
					err = os.Mkdir(leaf, 0o755)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before := sourceState(t, db, dir)
			for _, deep := range []bool{false, true} {
				want := tc.plain
				if deep {
					want = tc.deep
				}
				r := check(deep)
				if got := findings(r, b.SHA256); !slices.Equal(got, []string{want}) {
					t.Errorf("deep=%t: %v, want %s", deep, got, want)
				}
				if strings.HasSuffix(want, "_unsafe") {
					if advice := findingAdvice(r, "doctor_original_reclamation_unsafe"); !strings.Contains(advice, "Keep the app stopped") || !strings.Contains(advice, "new empty installation") {
						t.Errorf("fence advice: %q", advice)
					}
				}
				if strings.HasSuffix(want, "_missing") {
					if advice := findingAdvice(r, "doctor_original_reclamation_missing"); !strings.Contains(advice, "skips it") {
						t.Errorf("skip advice: %q", advice)
					}
				}
			}
			if after := sourceState(t, db, dir); after != before {
				t.Fatalf("doctor changed the source:\n%s\nwant\n%s", after, before)
			}
		})
	}
}
