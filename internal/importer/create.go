package importer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/names"
)

// Creation describes one browser album after all its files have been pinned.
// The caller holds the disk reservation through preparation (including space
// to extract and pin one embedded cover).
type Creation struct {
	Artist, Title string
	Year          *int
	Genre         *string
	Compilation   *bool
	Tracks        []CreationTrack
	Cover         *blobstore.Blob
}

type CreationTrack struct {
	Name                 string
	Blob                 blobstore.Blob
	Title, Artist, Genre *string
	Disc, Number         *int
}

// PrepareCreation uses the same full audio inspection and cover selection
// as a directory import, without a source directory, fingerprint or job.
func (im *Importer) PrepareCreation(ctx context.Context, c Creation) (catalog.AlbumCandidate, []jobs.Warning, error) {
	var err error
	if c.Title, err = creationText("album title", c.Title, false); err != nil {
		return catalog.AlbumCandidate{}, nil, err
	}
	if c.Artist, err = creationText("album artist", c.Artist, false); err != nil {
		return catalog.AlbumCandidate{}, nil, err
	}
	files := slices.Clone(c.Tracks)
	slices.SortFunc(files, func(a, b CreationTrack) int { return pathNaturalCompare(a.Name, b.Name) })
	var ws []jobs.Warning
	tracks := make([]*candidateFile, len(files))
	read := make([]readTrack, len(files))
	for i, f := range files {
		if err := ctx.Err(); err != nil {
			return catalog.AlbumCandidate{}, nil, err
		}
		a, audio, warnings, err := im.readAudio(ctx, f.Blob.SHA256, f.Name)
		for _, w := range warnings {
			// Browser creation validates the final genres. A replaced source
			// genre is not a rendering problem and needs no warning.
			if w.Code != jobs.WarnGenreNotWritable {
				ws = append(ws, w)
			}
		}
		if err != nil {
			return catalog.AlbumCandidate{}, nil, err
		}
		if !audio {
			return catalog.AlbumCandidate{}, nil, notAudio(f.Name)
		}
		read[i], err = readCreationTrack(f, a.tags.Managed)
		if err != nil {
			return catalog.AlbumCandidate{}, nil, err
		}
		// Later stages need only the picture descriptions, not the full
		// native metadata report for every file in the submission.
		a.tags = media.Inspection{Pictures: a.tags.Pictures}
		tracks[i] = &candidateFile{rel: f.Name, blob: f.Blob, audio: true, audioRead: a}
	}
	meta, err := inferReadMetadata("", read, jobs.Overrides{Title: &c.Title, Artist: &c.Artist})
	if err != nil {
		return catalog.AlbumCandidate{}, nil, err
	}
	for _, w := range meta.Warnings {
		if w.Code != jobs.WarnYearDiscordant || c.Year == nil {
			ws = append(ws, w)
		}
	}
	for i, f := range files {
		if f.Genre != nil {
			meta.Tracks[i].Genre = &read[i].genre
		}
		if f.Artist != nil {
			meta.Tracks[i].Artist = &read[i].artist
		}
	}
	if c.Year != nil {
		meta.Year = c.Year
	}
	if c.Genre != nil {
		meta.Genre = c.Genre
	}
	if c.Compilation != nil {
		meta.Compilation = *c.Compilation
	}
	var cover *catalog.Blob
	if c.Cover != nil {
		// The catalog checks compatibility with every verified audio format.
		cover, err = im.explicitCover(*c.Cover)
	} else {
		var sel coverSelection
		sel, err = im.chooseCover(ctx, nil, coverTracks(meta, tracks))
		cover, ws = sel.Cover, append(ws, sel.Warnings...)
	}
	if err != nil {
		return catalog.AlbumCandidate{}, nil, err
	}
	return assemble(meta, tracks, nil, nil, cover), ws, nil
}

func readCreationTrack(f CreationTrack, tags media.ManagedTags) (readTrack, error) {
	// Album identity is always explicitly supplied. Per-track overrides
	// replace their tags before inference, including disc and track numbers.
	tags.Album, tags.AlbumArtist = nil, nil
	for _, text := range []struct {
		value *string
		field string
		tag   *[]string
	}{
		{f.Title, "title", &tags.Title}, {f.Artist, "artist", &tags.Artist}, {f.Genre, "genre", &tags.Genre},
	} {
		if text.value == nil {
			continue
		}
		label := fmt.Sprintf("%q: track %s", f.Name, text.field)
		v, err := creationText(label, *text.value, text.field == "genre")
		if err != nil {
			return readTrack{}, err
		}
		*text.tag = []string{v}
	}
	if f.Disc != nil {
		if *f.Disc < 1 || *f.Disc > catalog.MaxDisc {
			return readTrack{}, &catalog.Error{Code: catalog.CodeInvalidDisc,
				Message: fmt.Sprintf("%q: disc must be in 1..%d", f.Name, catalog.MaxDisc)}
		}
		tags.Disc = []string{strconv.Itoa(*f.Disc)}
	}
	if f.Number != nil {
		if *f.Number < 1 || *f.Number > catalog.MaxTrackNumber {
			return readTrack{}, &catalog.Error{Code: catalog.CodeInvalidTrackNumber,
				Message: fmt.Sprintf("%q: track number must be in 1..%d", f.Name, catalog.MaxTrackNumber)}
		}
		tags.Track = []string{strconv.Itoa(*f.Number)}
	}
	r, err := readTags(trackTags{Path: f.Name, Tags: tags})
	r.explicitNumber = f.Number != nil
	return r, err
}

func creationText(label, value string, allowEmpty bool) (string, error) {
	normalize := names.NormalizeRequiredText
	if allowEmpty {
		normalize = names.NormalizeText
	}
	out, err := normalize(value)
	if err != nil {
		return "", &catalog.Error{Code: names.Code(err), Message: label, Err: err}
	}
	return out, nil
}

func (im *Importer) explicitCover(b blobstore.Blob) (*catalog.Blob, error) {
	r, err := im.blobs.Open(b.SHA256)
	if err != nil {
		return nil, err
	}
	format, err := media.ValidateCover(r, b.Size)
	if cerr := closeErr(r, "the cover blob"); cerr != nil {
		return nil, errors.Join(cerr, err)
	}
	if media.Code(err) == media.CodeInvalidImage {
		return nil, &catalog.Error{Code: catalog.CodeInvalidCover, Message: "the cover must be a valid JPEG or PNG within the byte and pixel limits", Err: err}
	}
	if err != nil {
		return nil, err
	}
	return &catalog.Blob{Hash: b.SHA256, Size: b.Size, Format: format}, nil
}
