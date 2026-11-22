package importer

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/jobs"
	"musiclib/internal/media"
	"musiclib/internal/store"
)

func TestPrepareCreationFinalCoverOrder(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	put := func(data []byte) blobstore.Blob {
		t.Helper()
		b, err := e.blobs.Put(ctx, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	first, second := pngImage(t, 6, 6, 1), pngImage(t, 6, 6, 2)
	a := put(track{tags: []string{"ALBUM=First", "ALBUMARTIST=One", "TRACKNUMBER=1"},
		pictures: []flacBlock{picture(t, 0, first)}}.flac(t))
	b := put(track{freq: 500, tags: []string{"ALBUM=Second", "ALBUMARTIST=Two", "TRACKNUMBER=2"},
		pictures: []flacBlock{picture(t, media.PictureFrontCover, []byte("\xff\xd8\xff garbage")), picture(t, 0, second)}}.flac(t))
	one := 1
	input := Creation{Artist: "Selected artist", Title: "Selected album", Tracks: []CreationTrack{
		{Name: "2.flac", Blob: a}, {Name: "10.flac", Blob: b, Number: &one},
	}}
	c, ws, err := e.im.PrepareCreation(ctx, input)
	if err != nil || c.CoverHash == nil || *c.CoverHash != sha(second) || len(c.Tracks) != 2 || c.Tracks[0].BlobHash != b.SHA256 || c.Tracks[1].No != 2 {
		t.Fatalf("prepared %+v, warnings %+v, err %v", c, ws, err)
	}
	if !hasWarning(ws, jobs.WarnCoverSkipped, "10.flac") || !hasWarning(ws, jobs.WarnTracksRenumbered, "") {
		t.Fatalf("missing cover or fallback warning: %+v", ws)
	}
	if got := len(c.Attachments); got != 0 {
		t.Fatalf("browser creation made %d attachments", got)
	}

	// A valid image within the upload limit can still be too large for the
	// target tag. An explicit cover never falls back to the embedded one.
	big := put(rawPNG(t, 2400, 2340))
	input.Cover = &big
	c, _, err = e.im.PrepareCreation(ctx, input)
	if err != nil || c.CoverHash == nil || *c.CoverHash != big.SHA256 {
		t.Fatalf("explicit cover was not prepared: %+v, %v", c, err)
	}
	id := store.NewID()
	if _, err := e.cat.CreateAlbum(ctx, id, uuid.Nil, c); catalog.Code(err) != catalog.CodeInvalidCover {
		t.Fatalf("incompatible explicit cover: %v", err)
	}
	if _, err := e.cat.GetAlbum(ctx, id); catalog.Code(err) != catalog.CodeAlbumNotFound {
		t.Fatalf("incompatible cover changed the catalog: %v", err)
	}
}

type failedCoverRead struct{ *bytes.Reader }

func (failedCoverRead) Read([]byte) (int, error) {
	return 0, errors.New("failed to read /data/work/private.img")
}

func TestPrepareCreationCoverWarningRedactsReadFailure(t *testing.T) {
	_, err := validateCover(failedCoverRead{bytes.NewReader(nil)}, 1)
	if err == nil || strings.Contains(err.Error(), "/data/") {
		t.Fatalf("unsafe cover warning: %v", err)
	}
}

func TestPrepareCreationRejectsUnrenderableTag(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	data := track{tags: []string{"COMMENT=caf\xe9"}}.flac(t)
	b, err := e.blobs.Put(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	input := Creation{Artist: "Artist", Title: "Album", Tracks: []CreationTrack{{Name: "1.flac", Blob: b}}}
	c, _, err := e.im.PrepareCreation(ctx, input)
	if Code(err) != CodeUnrenderableTag || len(c.Tracks) != 0 {
		t.Fatalf("unrenderable tag: content %+v, err %v", c, err)
	}
}
