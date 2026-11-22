package publish

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/jobs"
	"musiclib/internal/render"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

// scenario sets up an album whose next render is one kind of publication
// and returns it with the paths the library must hold afterwards.
type scenario struct {
	name   string
	points []string // the failpoints this publication passes
	setup  func(e *env) uuid.UUID
	// after lists the entries of library/ after the publication.
	after []string
}

var scenarios = []scenario{
	{
		name: "new", points: []string{"prepared", "installed", "synced", "finalized"},
		setup: func(e *env) uuid.UUID { return e.importAlbum("Artist", "Album") },
		after: []string{"Artist"},
	},
	{
		name: "exchange", points: []string{"prepared", "installed", "synced", "finalized"},
		setup: func(e *env) uuid.UUID {
			id := e.importAlbum("Artist", "Album")
			e.mustPublish("old")
			e.bump(id)
			return id
		},
		after: []string{"Artist"},
	},
	{
		name: "rename", points: []string{"prepared", "installed", "retired", "synced", "finalized"},
		setup: func(e *env) uuid.UUID {
			id := e.importAlbum("Old", "Album")
			e.mustPublish("old")
			e.renameArtist(id, "New")
			return id
		},
		after: []string{"New"},
	},
	{
		name: "removal", points: []string{"prepared", "retired", "synced", "finalized"},
		setup: func(e *env) uuid.UUID {
			id := e.importAlbum("Artist", "Album")
			e.mustPublish("old")
			e.trash(id)
			return id
		},
	},
}

// §9.4 and §12.2 "Crash prima/dopo PREPARE", "subito dopo exchange, prima
// degli fsync, prima/dopo FINALIZE", "tra installazione del nuovo path e
// ritiro del vecchio", in process: the publication fails at every named
// point after PREPARE; a new publisher (a new process, with no memory)
// completes it forward, never with a reverse exchange; a second recovery
// finds nothing to do; the boot's cleanup leaves work/ empty.
func TestRecoverAfterEveryPoint(t *testing.T) {
	for _, sc := range scenarios {
		for _, at := range sc.points {
			t.Run(sc.name+"/"+at, func(t *testing.T) {
				e := newEnv(t)
				id := sc.setup(e)
				e.failAt(at)
				_, res, err := e.render("new")
				if !jobs.Stops(err) {
					t.Fatalf("err %v, want a stop", err)
				}
				e.setFailpoint(nil)
				j, pending := e.journal()
				if pending != (at != "finalized") {
					t.Fatalf("journal pending = %v after a failure at %s", pending, at)
				}
				p2 := e.publisher(e.db)
				got, err := p2.Recover(context.Background())
				if err != nil {
					t.Fatalf("Recover: %v", err)
				}
				if pending && (got == nil || *got != j) {
					t.Fatalf("Recover completed %v, want %v", got, j)
				}
				e.wantPublished(id, res)
				if got := e.entries("library"); !slices.Equal(got, sc.after) {
					t.Fatalf("library holds %q, want %q", got, sc.after)
				}
				if _, ok := e.renderJob(id); ok {
					t.Fatal("the job survived FINALIZE")
				}
				// Idempotent.
				if again, err := e.publisher(e.db).Recover(context.Background()); err != nil || again != nil {
					t.Fatalf("a second Recover: %v %v", again, err)
				}
				if _, err := p2.CleanWork(context.Background()); err != nil {
					t.Fatal(err)
				}
				e.wantWorkClean()
				e.wantPublished(id, res)
				if res.Removal {
					e.wantClaims(id)
				} else {
					e.wantClaims(id, res.Dir)
				}
			})
		}
	}
}

// Unlike the general crash matrix, this test must keep putting v1 bytes on
// disk even after the default writer changes. The renderer string and receipt
// layout are from v1.2.0; the media is synthetic, as in stageBuild.
func TestRecoverLegacyV1Exchange(t *testing.T) {
	const version = "musiclib-render/3 names/1 go1.25.14 ffmpeg/8.1.3-musiclib1 musiclib-tags/4 taglib/2.3.2-musiclib1"
	const albumPath = "Artist/Album"
	const audioPath = "01 - One.flac"
	for _, at := range []string{"prepared", "installed"} {
		t.Run(at, func(t *testing.T) {
			e := newEnv(t)
			id := e.importAlbum("Artist", "Album")
			// Only the publisher creating the pre-upgrade state uses the old
			// renderer. Recovery children use the running binary's default.
			e.p.renderVersion = version
			stageV1 := func(content string) (*jobs.RenderSnapshot, render.Result) {
				snap := e.claim().Render
				snap.RenderVersion = version
				res := render.Result{AlbumID: id, AlbumRevision: snap.Album.Revision, BuildID: store.NewID(),
					RenderVersion: version, Dir: albumPath}
				res.Staging = render.StagingDir(res.BuildID)
				receipt := fmt.Sprintf(`{"schema_version":1,"album_id":"%s","build_id":"%s","album_revision":%d,"render_version":"%s",`+
					`"files":[{"relative_path":"%s","size":%d,"sha256":"%x"}]}`,
					id, res.BuildID, res.AlbumRevision, version, audioPath, len(content), sha256.Sum256([]byte(content)))
				res.ReceiptHash = fmt.Sprintf("%x", sha256.Sum256([]byte(receipt)))
				e.write("work/"+res.Staging+"/"+audioPath, content)
				e.write("work/"+res.Staging+"/"+render.ReceiptName, receipt)
				return snap, res
			}

			snap, old := stageV1("old audio")
			if rep, err := e.p.Publish(t.Context(), snap, old); err != nil || rep.Outcome != Published {
				t.Fatalf("publish old v1 build: %+v %v", rep, err)
			}
			lib := "library/" + albumPath
			oldTree := e.tree(lib)
			e.bump(id)
			snap, next := stageV1("new audio")
			staging := "work/" + next.Staging
			newTree := e.tree(staging)
			hit := e.failAt(at)
			if _, err := e.p.Publish(t.Context(), snap, next); !jobs.Stops(err) || !hit.Load() {
				t.Fatalf("publish new v1 build: %v, want a stop at %s", err, at)
			}

			want := Journal{AlbumID: id, Ticket: snap.Attempt.Ticket, Revision: next.AlbumRevision, Renderer: version,
				BuildID: next.BuildID, ReceiptHash: next.ReceiptHash, OldPath: albumPath, OldBuild: old.BuildID, NewPath: albumPath}
			if got := e.mustJournal(); got != want {
				t.Fatalf("pending journal: got %+v, want %+v", got, want)
			}
			if a := e.album(id); a.PublishedRevision != old.AlbumRevision || deref(a.PublishedBuild) != old.BuildID {
				t.Fatalf("published state changed before FINALIZE: revision %d, build %v", a.PublishedRevision, deref(a.PublishedBuild))
			}
			var crashAt string
			if at == "installed" {
				e.wantTree(lib, newTree)
				e.wantTree(staging, oldTree)
				crashAt = "installed" // Any attempt to exchange again kills the recovery child.
			} else {
				e.wantTree(lib, oldTree)
				e.wantTree(staging, newTree)
			}

			// An edit may commit while the journal is pending. FINALIZE must
			// record the built revision, not the now newer desired revision.
			e.bump(id)
			if got := e.runHelper("recover", crashAt, ""); got != "recovered "+next.BuildID.String() {
				t.Fatalf("recovery after %s: %s", at, got)
			}
			e.wantPublished(id, next)
			e.wantTree(lib, newTree)
			if got := e.runHelper("recover", "installed", ""); got != "none" {
				t.Fatalf("second recovery: %s", got)
			}
			e.wantPublished(id, next)
			e.wantTree(lib, newTree)
			e.wantWorkClean()
			if job, ok := e.renderJob(id); !ok || job.State != string(jobs.StatePending) {
				t.Fatalf("the newer render must stay pending: %+v", job)
			}
		})
	}
}

// V2 can replace a DB-anchored legacy output, and recovery must recognize
// the two formats on opposite sides of a same-path exchange.
func TestRecoverMixedReceiptExchange(t *testing.T) {
	for _, at := range []string{"prepared", "installed"} {
		t.Run(at, func(t *testing.T) {
			e := newEnv(t)
			id := e.importAlbum("Artist", "Album")
			snap := e.claim().Render
			old := e.stage(snap, "legacy")
			file := "work/" + old.Staging + "/" + render.ReceiptName
			// Independently write the legacy receipt and anchor its exact bytes.
			body := "audio legacy"
			legacy := fmt.Sprintf(`{"schema_version":1,"album_id":"%s","build_id":"%s","album_revision":%d,"render_version":"%s","files":[{"relative_path":"01 - One.flac","size":%d,"sha256":"%x"}]}`,
				id, old.BuildID, old.AlbumRevision, old.RenderVersion, len(body), sha256.Sum256([]byte(body)))
			e.write(file, legacy)
			old.ReceiptHash = render.ReceiptHash([]byte(legacy))
			if rep, err := e.p.Publish(t.Context(), snap, old); err != nil || rep.Outcome != Published {
				t.Fatalf("publish v1: %+v %v", rep, err)
			}
			e.bump(id)
			snap = e.claim().Render
			next := e.stage(snap, "v2")
			newTree := e.tree("work/" + next.Staging)
			hit := e.failAt(at)
			if _, err := e.p.Publish(t.Context(), snap, next); !jobs.Stops(err) || !hit.Load() {
				t.Fatalf("expected stop at %s: %v", at, err)
			}
			crashAt := ""
			if at == "installed" {
				e.wantTree("library/"+next.Dir, newTree)
				if got, err := os.ReadFile(e.path("work/" + next.Staging + "/" + render.ReceiptName)); err != nil || string(got) != legacy {
					t.Fatalf("old receipt after exchange: %s %v", got, err)
				}
				crashAt = "installed"
			}
			if got := e.runHelper("recover", crashAt, ""); got != "recovered "+next.BuildID.String() {
				t.Fatalf("mixed recovery: %s", got)
			}
			if got := e.runHelper("recover", "installed", ""); got != "none" {
				t.Fatalf("second recovery: %s", got)
			}
			e.wantPublished(id, next)
			e.wantTree("library/"+next.Dir, newTree)
			e.wantWorkClean()
		})
	}
}

// foreignReceipt changes only the lineage, so album and build IDs cannot
// distinguish this valid foreign output from the expected build.
func (e *env) foreignReceipt(dir string) string {
	e.t.Helper()
	path := dir + "/" + render.ReceiptName
	data, err := os.ReadFile(e.path(path))
	if err != nil {
		e.t.Fatal(err)
	}
	r, err := render.ParseReceipt(data)
	if err != nil {
		e.t.Fatal(err)
	}
	r.StoreID = uuid.New()
	data, err = r.Encode()
	if err != nil {
		e.t.Fatal(err)
	}
	e.write(path, string(data))
	return render.ReceiptHash(data)
}

func TestRecoverForeignStore(t *testing.T) {
	for _, tc := range []struct {
		name, at string
		scenario int
		tamper   func(*env, Journal)
	}{
		{"matching journal staging", "prepared", 1, func(e *env, j Journal) {
			hash := e.foreignReceipt("work/" + j.staging())
			e.exec(`UPDATE publication SET receipt_hash = $1`, hash)
		}},
		{"matching journal destination", "installed", 1, func(e *env, j Journal) {
			hash := e.foreignReceipt("library/" + j.NewPath)
			e.exec(`UPDATE publication SET receipt_hash = $1`, hash)
		}},
		{"previous output after exchange", "installed", 1, func(e *env, j Journal) {
			e.foreignReceipt("work/" + j.staging())
		}},
		{"old path before retirement", "installed", 2, func(e *env, j Journal) {
			e.foreignReceipt("library/" + j.OldPath)
		}},
		{"retired output", "retired", 3, func(e *env, j Journal) {
			e.foreignReceipt("work/" + j.retired())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			id := scenarios[tc.scenario].setup(e)
			hit := e.failAt(tc.at)
			if _, _, err := e.render("new"); !jobs.Stops(err) || !hit.Load() {
				t.Fatalf("expected stop at %s: %v", tc.at, err)
			}
			e.setFailpoint(nil)
			tc.tamper(e, e.mustJournal())
			journal := e.mustJournal()
			before := e.album(id)
			lib, work := e.tree("library"), e.tree("work")
			for range 2 {
				got, err := e.publisher(e.db).Recover(t.Context())
				wantCode(t, err, CodeIllegalState)
				if got == nil || *got != journal || e.mustJournal() != journal {
					t.Fatalf("journal changed at refusal: %v", got)
				}
				e.wantTree("work", work)
				e.wantTree("library", lib)
				if a := e.album(id); a.PublishedRevision != before.PublishedRevision || deref(a.PublishedBuild) != deref(before.PublishedBuild) {
					t.Fatalf("foreign output was finalized: %+v", a)
				}
			}
		})
	}
}

// §12.2 "Crash prima di PREPARE": nothing was prepared; the old output is
// valid, there is no journal, and the boot's cleanup and RecoverRunning
// make the render run again.
func TestRecoverBeforePrepare(t *testing.T) {
	e := newEnv(t)
	id := e.importAlbum("Artist", "Album")
	_, first := e.mustPublish("old")
	e.bump(id)
	c := e.claim()
	res := e.stage(c.Render, "new") // a crash here: the staging is left
	_ = res
	before := e.tree("library")
	if got, err := e.publisher(e.db).Recover(context.Background()); err != nil || got != nil {
		t.Fatalf("Recover: %v %v", got, err)
	}
	removed, err := e.p.CleanWork(context.Background())
	if err != nil || len(removed) != 1 {
		t.Fatalf("CleanWork removed %q: %v", removed, err)
	}
	e.wantTree("library", before)
	e.wantInstalled(first)
	if n, err := jobs.RecoverRunning(context.Background(), e.db); err != nil || n != 1 {
		t.Fatalf("RecoverRunning = %d, %v", n, err)
	}
	_, res = e.mustPublish("new")
	e.wantPublished(id, res)
}

// §9.4: a state that matches no legal transition suspends publishing and
// exposes the error; nothing is deleted or moved, and the journal stays.
func TestRecoverIllegalStates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sc     int    // index in scenarios
		failAt string // where the publication stopped
		tamper func(e *env, j Journal)
	}{
		{"staging missing", 0, "prepared", func(e *env, j Journal) {
			if err := os.RemoveAll(e.path("work/" + j.staging())); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"foreign directory at the new path", 0, "prepared", func(e *env, j Journal) {
			e.write("library/"+j.NewPath+"/mine.txt", "the user's")
		}},
		{"another album at the published path", 1, "prepared", func(e *env, j Journal) {
			r := render.Receipt{SchemaVersion: render.ReceiptSchemaVersion, StoreID: e.p.storeID, AlbumID: uuid.New(), BuildID: uuid.New(), AlbumRevision: 1, RenderVersion: render.Version}
			b, err := r.Encode()
			if err != nil {
				e.t.Fatal(err)
			}
			e.write("library/"+j.NewPath+"/"+render.ReceiptName, string(b))
		}},
		{"exchanged output damaged: never a reverse exchange", 1, "installed", func(e *env, j Journal) {
			// The new output lost its receipt after the exchange: the
			// staging now holds the old album, not the expected build.
			if err := os.Remove(e.path("library/" + j.NewPath + "/" + render.ReceiptName)); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"retired directory already present", 2, "installed", func(e *env, j Journal) {
			e.write("work/"+j.retired()+"/other.txt", "something")
		}},
		{"symlink at the old path", 2, "prepared", func(e *env, j Journal) {
			if err := os.Rename(e.path("library/"+j.OldPath), e.path("moved")); err != nil {
				e.t.Fatal(err)
			}
			if err := os.Symlink(e.path("moved"), e.path("library/"+j.OldPath)); err != nil {
				e.t.Fatal(err)
			}
		}},
		{"removal: another album at the old path", 3, "prepared", func(e *env, j Journal) {
			r := render.Receipt{SchemaVersion: render.ReceiptSchemaVersion, StoreID: e.p.storeID, AlbumID: uuid.New(), BuildID: uuid.New(), AlbumRevision: 1, RenderVersion: render.Version}
			b, err := r.Encode()
			if err != nil {
				e.t.Fatal(err)
			}
			e.write("library/"+j.OldPath+"/"+render.ReceiptName, string(b))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			scenarios[tc.sc].setup(e)
			e.failAt(tc.failAt)
			if _, _, err := e.render("new"); !jobs.Stops(err) {
				t.Fatalf("err %v, want a stop", err)
			}
			e.setFailpoint(nil)
			j := e.mustJournal()
			tc.tamper(e, j)
			lib, work := e.tree("library"), e.tree("work")
			for range 2 {
				got, err := e.publisher(e.db).Recover(context.Background())
				wantCode(t, err, CodeIllegalState)
				if got == nil || *got != j {
					t.Fatalf("the refusal names %v, want the pending journal %v", got, j)
				}
				e.wantTree("library", lib)
				e.wantTree("work", work)
				if e.mustJournal() != j {
					t.Fatal("the journal changed")
				}
			}
		})
	}
}

// §12.2 "Errore DB dopo rename": the database is lost at FINALIZE, after
// the rename. The publisher stops with a fatal error; the journal is
// completed at the restart. A FINALIZE that committed but whose answer was
// lost leaves nothing to recover.
func TestRecoverDatabaseErrorAfterRename(t *testing.T) {
	for _, mode := range []string{"commit cut", "ack lost"} {
		t.Run(mode, func(t *testing.T) {
			dbURL := pgtest.EmptyDB(t)
			direct := pgtest.Pool(t, dbURL)
			if err := store.Migrate(t.Context(), direct); err != nil {
				t.Fatal(err)
			}
			e := newEnvOn(t, dbURL, direct, t.TempDir()+"/data")
			proxy := pgtest.NewProxy(t, dbURL)
			viaProxy := e.publisher(pgtest.Pool(t, proxy.URL))
			id := e.importAlbum("Artist", "Album")
			e.setFailpoint(func(point string) error {
				if point == "synced" {
					if mode == "commit cut" {
						proxy.CutBeforeNextCommit()
					} else {
						proxy.LoseNextCommitAck()
					}
				}
				return nil
			})
			c := e.claim()
			res := e.stage(c.Render, "v1")
			_, err := viaProxy.Publish(context.Background(), c.Render, res)
			if !store.IsFatal(err) || !jobs.Stops(err) {
				t.Fatalf("err %v, want a fatal store error", err)
			}
			e.setFailpoint(nil)
			_, pending := e.journal()
			if pending != (mode == "commit cut") {
				t.Fatalf("journal pending = %v", pending)
			}
			if _, err := e.publisher(direct).Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			e.wantPublished(id, res)
			if _, err := e.p.CleanWork(context.Background()); err != nil {
				t.Fatal(err)
			}
			e.wantWorkClean()
		})
	}
}

// CleanWork keeps the build a pending journal references, and removes
// every other build and retired directory.
func TestCleanWorkKeepsTheJournalsBuild(t *testing.T) {
	e := newEnv(t)
	e.importAlbum("Artist", "Album")
	e.failAt("prepared")
	if _, _, err := e.render("v1"); !jobs.Stops(err) {
		t.Fatal(err)
	}
	j := e.mustJournal()
	e.write("work/render/leftover/album/x", "x")
	e.write("work/retired/"+uuid.NewString()+"/y", "y")
	e.write("work/retired/"+j.BuildID.String()+"/z", "z")
	removed, err := e.p.CleanWork(context.Background())
	if err != nil || len(removed) != 2 {
		t.Fatalf("removed %q: %v", removed, err)
	}
	if got := e.entries("work/render"); !slices.Equal(got, []string{j.BuildID.String()}) {
		t.Fatalf("work/render holds %q", got)
	}
	if got := e.entries("work/retired"); !slices.Equal(got, []string{j.BuildID.String()}) {
		t.Fatalf("work/retired holds %q", got)
	}
	if !strings.HasPrefix(removed[0], "render/") {
		t.Fatalf("removed %q", removed)
	}
}
