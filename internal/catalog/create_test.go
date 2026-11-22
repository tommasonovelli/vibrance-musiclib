package catalog_test

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

func creationCandidate(artist, title string) catalog.AlbumCandidate {
	b := catalog.Blob{Hash: newHash(), Size: 1024, Format: catalog.FormatFLAC}
	return catalog.AlbumCandidate{Artist: artist, Title: title, Blobs: []catalog.Blob{b},
		Tracks: []catalog.ImportTrack{{SourcePath: "track.flac", Disc: 1, No: 1, Title: "Track", BlobHash: b.Hash}}}
}

func TestCreateAlbumConcurrent(t *testing.T) {
	for _, tc := range []struct {
		name, conflict  string
		oneID, oneTitle bool
		winners         int
	}{
		{"same ID", catalog.CodeCreationConflict, true, false, 1},
		{"same folder", catalog.CodeAlbumFolderConflict, false, true, 1},
		{"same artist", "", false, false, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			const n = 6
			sameID := store.NewID()
			var es [n]error
			var vs [n]catalog.AlbumView
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := range n {
				id := store.NewID()
				if tc.oneID {
					id = sameID
				}
				title := "Album"
				if !tc.oneTitle {
					title = fmt.Sprintf("Album %d", i)
				}
				artist := []string{"  Cafe\u0301 ", "CAFÉ"}[i%2]
				c := creationCandidate(artist, title)
				wg.Go(func() {
					<-start
					vs[i], es[i] = e.svc.CreateAlbum(t.Context(), id, uuid.Nil, c)
				})
			}
			close(start)
			wg.Wait()
			wins := 0
			var artistID uuid.UUID
			for i, err := range es {
				v := vs[i]
				if err != nil {
					ce := wantCode(t, err, tc.conflict)
					if v.ID != uuid.Nil || (tc.oneID && ce.Details.AlbumID != sameID) {
						t.Fatalf("refusal view %+v, err %+v", v, ce)
					}
					continue
				}
				wins++
				if artistID == uuid.Nil {
					artistID = v.ArtistID
				}
				if v.Revision != 1 || len(v.Tracks) != 1 || v.ArtistID != artistID {
					t.Fatalf("successful view %+v", v)
				}
				a := e.album(v.ID)
				if a.ImportFingerprint != nil || a.PublishedPath != nil || a.PublishedRevision != 0 {
					t.Fatalf("not a new desired-only album: %+v", a)
				}
				if j, ok := e.renderJob(v.ID); !ok || j.Requested <= 0 || j.State != "pending" {
					t.Fatalf("wanted one first render request, got %+v", j)
				}
			}
			if wins != tc.winners || e.count("SELECT count(*) FROM artists") != 1 ||
				e.count("SELECT count(*) FROM albums") != wins || e.count("SELECT count(*) FROM tracks") != wins ||
				e.count("SELECT count(*) FROM blobs") != wins || e.count("SELECT count(*) FROM path_claims") != wins ||
				e.count("SELECT count(*) FROM jobs") != wins || e.wakes.Load() != int32(wins) {
				t.Fatalf("creation leaked state or had %d winners, want %d", wins, tc.winners)
			}
		})
	}
}

func TestCreateAlbumAuthoritativeArtist(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	oldAlbum := e.importAlbum("Old", "Original")
	id := e.album(oldAlbum).ArtistID
	// Model a rename between the API's preparation lookup and commit. The old
	// folder now has its own album with the same title as the submission.
	if _, _, err := e.svc.RenameArtist(ctx, id, 1, "Renamed"); err != nil {
		t.Fatal(err)
	}
	e.importAlbum("Old", "New Album")
	v, err := e.svc.CreateAlbum(ctx, store.NewID(), id, creationCandidate("Old", "New Album"))
	if err != nil || v.ArtistID != id || v.ArtistName != "Renamed" {
		t.Fatalf("creation used stale artist: %+v, %v", v, err)
	}
	if got := e.claims(v.ID); !reflect.DeepEqual(got, map[string]string{"renamed/new album": "Renamed/New Album"}) {
		t.Fatalf("claims %v", got)
	}

	gone := e.importAlbum("Going away", "Last Album")
	oldArtist := e.album(gone).ArtistID
	if _, _, err := e.svc.TrashAlbum(ctx, gone, 1); err != nil {
		t.Fatal(err)
	}
	// Act as the worker after the never-published album's removal completes.
	e.exec("DELETE FROM jobs WHERE kind = 'render' AND album_id = $1", gone)
	if _, _, err := e.svc.EmptyTrash(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CreateAlbum(ctx, store.NewID(), oldArtist, creationCandidate("Going away", "Refused"))
	wantCode(t, err, catalog.CodeArtistNotFound)
	if e.count("SELECT count(*) FROM artists WHERE name = 'Going away'") != 0 {
		t.Fatal("a deleted artist was recreated on the ID path")
	}
}

func TestCreateAlbumExactArtistName(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	long := strings.Repeat("A", 200)
	artist, err := e.svc.CreateArtist(ctx, long)
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.svc.CreateAlbum(ctx, store.NewID(), uuid.Nil, creationCandidate(strings.ToLower(long), "Album"))
	if err != nil || v.ArtistID != artist.ID || v.ArtistName != long {
		t.Fatalf("casefold reuse across truncated folder keys: %+v, %v", v, err)
	}
	if _, err := e.svc.CreateArtist(ctx, "AC/DC"); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CreateAlbum(ctx, store.NewID(), uuid.Nil, creationCandidate("AC_DC", "Album"))
	wantCode(t, err, catalog.CodeArtistFolderConflict)
	if e.count("SELECT count(*) FROM artists") != 2 || e.count("SELECT count(*) FROM albums") != 1 || e.count("SELECT count(*) FROM blobs") != 1 {
		t.Fatal("a sanitization-only conflict created catalog state")
	}
}

func TestCreateAlbumRollbackAndLostAck(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	direct := pgtest.Pool(t, dbURL)
	if err := store.Migrate(t.Context(), direct); err != nil {
		t.Fatal(err)
	}
	e := newEnvOn(t, direct)
	ctx := t.Context()
	id := store.NewID()
	c := creationCandidate("New Artist", "New Album")
	proxy := pgtest.NewProxy(t, dbURL)
	via := e.service(pgtest.Pool(t, proxy.URL))
	proxy.CutBeforeNextCommit()
	v, err := via.CreateAlbum(ctx, id, uuid.Nil, c)
	if v.ID != uuid.Nil || catalog.Code(err) != store.CodeCommitUncertain || !store.IsFatal(err) {
		t.Fatalf("cut before COMMIT: %+v, %v", v, err)
	}
	waitIdle(t, e)
	for _, table := range []string{"artists", "albums", "tracks", "blobs", "path_claims", "jobs"} {
		if n := e.count("SELECT count(*) FROM " + table); n != 0 {
			t.Fatalf("lost transaction left %d rows in %s", n, table)
		}
	}
	proxy.LoseNextCommitAck()
	v, err = via.CreateAlbum(ctx, id, uuid.Nil, c)
	if v.ID != uuid.Nil || catalog.Code(err) != store.CodeCommitUncertain || !store.IsFatal(err) || e.wakes.Load() != 0 {
		t.Fatalf("lost ACK: %+v, %v", v, err)
	}
	v, err = e.svc.GetAlbum(ctx, id)
	if err != nil || v.Revision != 1 {
		t.Fatalf("lost ACK album: %+v, %v", v, err)
	}
	_, err = e.svc.CreateAlbum(ctx, id, uuid.Nil, creationCandidate("Another artist", "Changed input"))
	wantCode(t, err, catalog.CodeCreationConflict)
	if e.count("SELECT count(*) FROM jobs") != 1 || e.count("SELECT count(*) FROM artists") != 1 {
		t.Fatal("retry added catalog state")
	}
}
