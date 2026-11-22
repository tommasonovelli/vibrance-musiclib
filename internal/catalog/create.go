package catalog

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"musiclib/internal/names"
	"musiclib/internal/store"
)

const CodeCreationConflict = "creation_conflict"

// CheckCreationID is an early refusal, before receiving file parts. Only
// CreateAlbum's check under the catalog lock decides whether an ID is free.
func (s *Service) CheckCreationID(ctx context.Context, id uuid.UUID) error {
	return store.InSnapshotTx(ctx, s.db, func(q *store.Queries) error {
		return checkCreationID(ctx, q, id)
	})
}

func checkCreationID(ctx context.Context, q interface {
	GetAlbum(context.Context, uuid.UUID) (store.Album, error)
}, id uuid.UUID) error {
	_, err := q.GetAlbum(ctx, id)
	switch {
	case err == nil:
		return &Error{Code: CodeCreationConflict, Message: "an album already has this creation ID", Details: Details{AlbumID: id}}
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	default:
		return dbErr("checking the creation ID", err)
	}
}

// CreateAlbum creates desired catalog state only, with no import fingerprint
// or job. With artistID set, the candidate's artist is the preparation label;
// the existing artist's current name under the lock determines the path.
// The returned view is the committed revision 1, not a later snapshot.
func (s *Service) CreateAlbum(ctx context.Context, id, artistID uuid.UUID, c AlbumCandidate) (AlbumView, error) {
	if id == uuid.Nil {
		return AlbumView{}, errorf(CodeInvalidArgument, "an album needs a creation ID")
	}
	plan, err := planAlbumMetadata(c, id)
	if err != nil {
		return AlbumView{}, err
	}
	seen := make(map[string]string, len(c.Tracks))
	for _, t := range c.Tracks {
		if first, ok := seen[t.BlobHash]; ok {
			return AlbumView{}, &Error{Code: CodeTrackExists, Message: "two tracks have identical audio files", Details: Details{Names: []string{first, t.SourcePath}}}
		}
		seen[t.BlobHash] = t.SourcePath
	}
	if err := planAlbumContent(c, plan, s.coverFits); err != nil {
		return AlbumView{}, err
	}
	if err := checkCreationGenres(plan, s.genreFits); err != nil {
		return AlbumView{}, err
	}

	newArtistID := store.NewID()
	var view AlbumView
	err = store.InCatalogTx(ctx, s.db, func(tx *store.CatalogTx) error {
		if err := checkCreationID(ctx, tx, id); err != nil {
			return err
		}
		artist, err := creationArtist(ctx, tx, artistID, plan.artist, newArtistID)
		if err != nil {
			return err
		}
		if err := checkAlbumPath(ctx, tx, plan, artist, AlbumPath(artist.Name, plan.album.Title)); err != nil {
			return err
		}
		if err := checkBlobs(ctx, tx, plan); err != nil {
			return err
		}
		if err := insertAlbum(ctx, tx, plan, artist, nil); err != nil {
			return err
		}
		view, err = readAlbumView(ctx, tx, id)
		if err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return AlbumView{}, err
	}
	s.notify()
	return view, nil
}

func creationArtist(ctx context.Context, tx *store.CatalogTx, id uuid.UUID, name string, newID uuid.UUID) (artistRef, error) {
	if id == uuid.Nil {
		return creationArtistByName(ctx, tx, name, newID)
	}
	a, err := tx.GetArtist(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return artistRef{}, errorf(CodeArtistNotFound, "artist %s does not exist", id)
	}
	if err != nil {
		return artistRef{}, dbErr("reading artist "+id.String(), err)
	}
	return artistRef{ID: a.ID, Name: a.Name, FolderKey: a.FolderKey, Exists: true}, nil
}

func creationArtistByName(ctx context.Context, tx *store.CatalogTx, name string, newID uuid.UUID) (artistRef, error) {
	ar, err := resolveArtistWithID(ctx, tx, name, newID)
	if ar.Exists || (err != nil && Code(err) != CodeArtistFolderConflict) {
		return ar, err
	}
	// A long folder name hashes its spelling before casefolding, so an
	// exact normalized name can exist under another folder key. Do not
	// change the frozen folder key or the directory import's lookup.
	rows, listErr := tx.ListArtists(ctx)
	if listErr != nil {
		return artistRef{}, dbErr("looking up the normalized artist name", listErr)
	}
	key := names.Key(name)
	var match artistRef
	for _, a := range rows {
		if names.Key(a.Name) != key {
			continue
		}
		if match.Exists {
			return artistRef{}, errorf(CodeArtistExists, "more than one artist has this normalized name: select an existing artist")
		}
		match = artistRef{ID: a.ID, Name: a.Name, FolderKey: a.FolderKey, Exists: true}
	}
	if match.Exists {
		return match, nil
	}
	return ar, err
}

func checkCreationGenres(plan *albumPlan, fits GenreFits) error {
	var formats []string
	for _, b := range plan.blobs {
		if plan.roles[b.Hash]&roleAudio != 0 && !slices.Contains(formats, b.Format) {
			formats = append(formats, b.Format)
		}
	}
	slices.Sort(formats)
	fs := make([]*string, len(formats))
	for i := range formats {
		fs[i] = &formats[i]
	}
	gs := make([]*string, len(plan.tracks))
	for i, t := range plan.tracks {
		gs[i] = t.Genre
	}
	return checkGenreFormats(fs, plan.album.Genre, gs, fits)
}
