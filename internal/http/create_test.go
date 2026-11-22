package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	nethttp "net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/names"
	"musiclib/internal/store"
	"musiclib/internal/store/pgtest"
)

type prepareFunc func(context.Context, importer.Creation) (catalog.AlbumCandidate, []jobs.Warning, error)

func (f prepareFunc) PrepareCreation(ctx context.Context, c importer.Creation) (catalog.AlbumCandidate, []jobs.Warning, error) {
	return f(ctx, c)
}

func (e *env) enableCreation(p AlbumPreparer) {
	b := e.backend()
	b.Creation = p
	e.api.Enable(b)
}

type creationTestPart struct {
	id, name string
	data     []byte
	header   textproto.MIMEHeader
}

func testFile(id, name string, data []byte) creationTestPart {
	return creationTestPart{id: id, name: name, data: data}
}

func (f creationTestPart) declaration() map[string]any {
	return map[string]any{"part_id": f.id, "name": f.name, "size": len(f.data)}
}

func testDraft(id uuid.UUID, tracks ...creationTestPart) map[string]any {
	rows := make([]map[string]any, len(tracks))
	for i, f := range tracks {
		rows[i] = f.declaration()
	}
	return map[string]any{"creation_id": id.String(), "artist_name": "New Artist", "title": "New Album", "tracks": rows}
}

func creationRequest(t *testing.T, metadata any, files ...creationTestPart) req {
	t.Helper()
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	if metadata != nil {
		var raw []byte
		var err error
		if s, ok := metadata.(string); ok {
			raw = []byte(s)
		} else if part, ok := metadata.(creationTestPart); ok {
			files = append([]creationTestPart{part}, files...)
		} else {
			raw, err = json.Marshal(metadata)
		}
		if err != nil {
			t.Fatal(err)
		}
		if raw != nil {
			if err := m.WriteField("metadata", string(raw)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, f := range files {
		var w io.Writer
		var err error
		if f.header != nil {
			w, err = m.CreatePart(f.header)
		} else {
			w, err = m.CreateFormFile(f.id, f.name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return req{method: "POST", path: "/api/albums", body: body.Bytes(), headers: map[string][]string{"Content-Type": {m.FormDataContentType()}}}
}

func (e *env) noCreationState() {
	e.t.Helper()
	for _, table := range []string{"artists", "albums", "tracks", "blobs", "path_claims", "jobs"} {
		if n := e.count("SELECT count(*) FROM " + table); n != 0 {
			e.t.Fatalf("refused creation left %d rows in %s", n, table)
		}
	}
	if n := len(e.api.uploads); n != 0 || e.budget.Reserved() != 0 {
		e.t.Fatalf("%d upload slots and %d bytes still held", n, e.budget.Reserved())
	}
	if _, temps := e.blobFiles(); temps != 0 {
		e.t.Fatalf("%d temporary blobs remain", temps)
	}
}

func forbidPreparation(t *testing.T) prepareFunc {
	return func(context.Context, importer.Creation) (catalog.AlbumCandidate, []jobs.Warning, error) {
		t.Error("a refused request reached media preparation")
		return catalog.AlbumCandidate{}, nil, errors.New("unexpected media preparation")
	}
}

func TestAlbumCreationMultipart(t *testing.T) {
	e := newEnv(t)
	e.enableCreation(forbidPreparation(t))
	id := store.NewID()
	file := testFile("audio_1", "é; 1.flac", []byte("first file"))
	second := testFile("audio_2", "2.flac", []byte("second file"))
	meta := testDraft(id, file, second)
	bad := func(disposition string) creationTestPart {
		p := second
		p.header = textproto.MIMEHeader{"Content-Disposition": {disposition}}
		return p
	}
	for _, tc := range []struct {
		name    string
		request req
		pinned  int
	}{
		{"wrong first", creationRequest(t, nil, file, second), 0},
		{"metadata filename", creationRequest(t, nil, testFile("metadata", "meta.json", []byte("{}")), file), 0},
		{"file with slash", creationRequest(t, meta, bad(`form-data; name="audio_2"; filename="x/2.flac"`), file), 0},
		{"file with backslash", creationRequest(t, meta, bad(`form-data; name="audio_2"; filename="x\2.flac"`), file), 0},
		{"ambiguous filename", creationRequest(t, meta, bad(`form-data; name="audio_2"; filename="2.flac"; filename*=UTF-8''2.flac`), file), 0},
		{"ambiguous name", creationRequest(t, meta, bad(`form-data; name="audio_2"; name="audio_2"; filename="2.flac"`), file), 0},
		{"invalid utf8 filename", creationRequest(t, meta, bad("form-data; name=audio_2; filename=\"\xff\""), file), 0},
		{"wrong filename", creationRequest(t, meta, testFile(second.id, "wrong.flac", second.data), file), 0},
		{"no percent decoding", creationRequest(t, meta, testFile(second.id, "%32.flac", second.data), file), 0},
		{"too short", creationRequest(t, meta, testFile(second.id, second.name, second.data[:2]), file), 0},
		{"too long", creationRequest(t, meta, testFile(second.id, second.name, []byte("second file+")), file), 0},
		{"missing", creationRequest(t, meta, file), 1},
		{"repeated", creationRequest(t, meta, file, file), 1},
		{"undeclared", creationRequest(t, meta, file, testFile("another", second.name, second.data)), 1},
		{"extra", creationRequest(t, meta, file, second, second), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := e.t
			e.t = t
			defer func() { e.t = parent }()
			e.wantError(tc.request, 422, CodeInvalidMultipart)
			e.noCreationState()
			if n, _ := e.blobFiles(); n != tc.pinned {
				t.Fatalf("pinned %d files, want %d", n, tc.pinned)
			}
		})
	}
	if !bytes.Equal(e.pinnedBytes(sha(file.data)), file.data) || !bytes.Equal(e.pinnedBytes(sha(second.data)), second.data) {
		t.Fatal("completed puts did not survive later errors")
	}
}

func TestAlbumCreationMetadataAndLimits(t *testing.T) {
	e := newEnv(t)
	e.enableCreation(forbidPreparation(t))
	id := store.NewID()
	file := testFile("f", "x.flac", []byte("abc"))
	for _, tc := range []struct {
		name, code string
		status     int
		change     func(map[string]any, map[string]any)
	}{
		{"nil UUID", CodeInvalidField, 422, func(m, _ map[string]any) { m["creation_id"] = uuid.Nil.String() }},
		{"uppercase UUID", CodeInvalidField, 422, func(m, _ map[string]any) { m["creation_id"] = "F" + id.String()[1:] }},
		{"both artist forms", CodeInvalidField, 422, func(m, _ map[string]any) { m["artist_id"] = id.String() }},
		{"null override", CodeInvalidField, 422, func(_, f map[string]any) { f["genre"] = nil }},
		{"missing size", CodeMissingField, 422, func(_, f map[string]any) { delete(f, "size") }},
		{"unknown nested key", CodeUnknownField, 422, func(_, f map[string]any) { f["hash"] = strings.Repeat("a", 64) }},
		{"literal optional marker", CodeUnknownField, 422, func(m, _ map[string]any) { m["year?"] = 2000 }},
		{"no tracks", catalog.CodeNoTracks, 422, func(m, _ map[string]any) { m["tracks"] = []any{} }},
		{"too many tracks", CodeBodyTooLarge, 413, func(m, _ map[string]any) { m["tracks"] = make([]bool, catalog.MaxTracks+1) }},
		{"reserved ID", CodeInvalidMultipart, 422, func(_, f map[string]any) { f["part_id"] = "metadata" }},
		{"overlong ID", CodeInvalidMultipart, 422, func(_, f map[string]any) { f["part_id"] = strings.Repeat("a", 65) }},
		{"path", CodeInvalidMultipart, 422, func(_, f map[string]any) { f["name"] = "../x.flac" }},
		{"dot", CodeInvalidMultipart, 422, func(_, f map[string]any) { f["name"] = "." }},
		{"control", CodeInvalidMultipart, 422, func(_, f map[string]any) { f["name"] = "a\u0085.flac" }},
		{"overlong filename", CodeInvalidMultipart, 422, func(_, f map[string]any) { f["name"] = strings.Repeat("é", 128) }},
		{"duplicate filename", CodeInvalidMultipart, 422, func(m, f map[string]any) { m["tracks"] = []any{f, testFile("q", file.name, file.data).declaration()} }},
		{"cover ID matches track", CodeInvalidMultipart, 422, func(m, f map[string]any) { m["cover"] = f }},
		{"oversized file", CodeBodyTooLarge, 413, func(_, f map[string]any) { f["size"] = int64(MaxTrackBytes) + 1 }},
		{"oversized cover", CodeBodyTooLarge, 413, func(m, _ map[string]any) {
			m["cover"] = map[string]any{"part_id": "cover", "name": "c.png", "size": MaxCoverBytes + 1}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := e.t
			e.t = t
			defer func() { e.t = parent }()
			m := testDraft(id, file)
			tc.change(m, m["tracks"].([]map[string]any)[0])
			e.wantError(creationRequest(t, m, file), tc.status, tc.code)
		})
	}
	for _, tc := range []struct{ meta, code string }{
		{`{"tracks":[{"size":4,"size":5}]}`, CodeDuplicateKey},
		{"\xff", CodeInvalidUTF8},
		{`{"artist_name":"\ud800"}`, CodeInvalidUTF8},
		{`{`, CodeInvalidJSON},
	} {
		e.wantError(creationRequest(t, tc.meta, file), 400, tc.code)
	}
	metadataCap := creationRequest(t, strings.Repeat(" ", maxCreationMetadata+1), file)
	metadataCap.body = struct{ io.Reader }{bytes.NewReader(metadataCap.body.([]byte))}
	e.wantError(metadataCap, 413, CodeBodyTooLarge)
	withHeader := file
	withHeader.header = textproto.MIMEHeader{"Content-Disposition": {`form-data; name=f; filename=x.flac`}, "X-Large": {strings.Repeat("h", 2*creationOverhead)}}
	e.wantError(creationRequest(t, testDraft(id, file), withHeader), 413, CodeBodyTooLarge)
	malformed := creationRequest(t, testDraft(id, file), file)
	malformed.headers["Content-Type"] = []string{"multipart/form-data; boundary=wrong"}
	e.wantError(malformed, 422, CodeInvalidMultipart)
	malformed.headers["Content-Type"] = []string{"multipart/form-data; boundary=\"bad@boundary\""}
	e.wantError(malformed, 422, CodeInvalidMultipart)
	e.noCreationState()
	if n, _ := e.blobFiles(); n != 0 {
		t.Fatalf("invalid metadata pinned %d files", n)
	}
}

func TestAlbumCreationRawBodyAndBoundary(t *testing.T) {
	e := newEnv(t)
	e.enableCreation(forbidPreparation(t))
	file := testFile("one", "one.flac", []byte("valid part"))
	post := creationRequest(t, testDraft(store.NewID(), file), file)
	raw := post.body.([]byte)
	makeRequest := func(body io.Reader) *nethttp.Request {
		r := httptest.NewRequest("POST", "/api/albums", body)
		r.Host, r.Header = testHost, nethttp.Header{"Content-Type": post.headers["Content-Type"], RequestHeader: {"1"}}
		r.AddCookie(&nethttp.Cookie{Name: sessionCookie, Value: e.session})
		return r
	}
	for _, tc := range []struct {
		code   string
		status int
		change func(*nethttp.Request)
	}{
		{CodeHostNotAllowed, 421, func(r *nethttp.Request) { r.Host = "wrong.invalid" }},
		{CodeOriginNotAllowed, 403, func(r *nethttp.Request) { r.Header.Set("Origin", "https://wrong.invalid") }},
		{CodeRequestHeaderRequired, 403, func(r *nethttp.Request) { r.Header.Del(RequestHeader) }},
		{CodeLoginRequired, 401, func(r *nethttp.Request) { r.Header.Del("Cookie") }},
		{CodeBodyTooLarge, 413, func(r *nethttp.Request) { r.ContentLength = maxCreationBytes + 1 }},
	} {
		r := makeRequest(creationErrorReader(func() (int, error) {
			t.Error("rejected request read its body")
			return 0, io.EOF
		}))
		tc.change(r)
		w := httptest.NewRecorder()
		e.api.ServeHTTP(w, r)
		if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
			t.Fatalf("wanted %s, got %d %s", tc.code, w.Code, w.Body)
		}
	}
	// A short/disconnected wire body (as net/http reports it) is not MIME EOF.
	cut := bytes.Index(raw, file.data) + len(file.data) - 1
	r := makeRequest(io.MultiReader(bytes.NewReader(raw[:cut]), creationErrorReader(func() (int, error) { return 0, io.ErrUnexpectedEOF })))
	w := httptest.NewRecorder()
	e.api.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"upload_incomplete"`) {
		t.Fatalf("disconnect: %d %s", w.Code, w.Body)
	}
	if n, _ := e.blobFiles(); n != 0 {
		t.Fatalf("disconnect pinned %d files", n)
	}
	// A boundary prefix alone is not proof of an exact file part.
	badEnd := post
	badEnd.body = append(bytes.Clone(raw[:len(raw)-2]), []byte("invalid\r\n")...)
	e.wantError(badEnd, 422, CodeInvalidMultipart)
	if n, _ := e.blobFiles(); n != 0 {
		t.Fatalf("malformed closing delimiter pinned %d files", n)
	}
	// Chunked epilogue, including bytes already buffered inside multipart,
	// is counted even after the closing boundary.
	post.body = io.MultiReader(bytes.NewReader(raw), strings.NewReader(strings.Repeat("x", 3*creationOverhead)))
	e.wantError(post, 413, CodeBodyTooLarge)
	if !bytes.Equal(e.pinnedBytes(sha(file.data)), file.data) {
		t.Fatal("earlier completed put was removed")
	}
	r = makeRequest(bytes.NewReader(raw))
	r.ContentLength++
	w = httptest.NewRecorder()
	e.api.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"upload_incomplete"`) {
		t.Fatalf("short Content-Length body: %d %s", w.Code, w.Body)
	}
	e.noCreationState()
}

type creationErrorReader func() (int, error)

func (f creationErrorReader) Read([]byte) (int, error) { return f() }

func creationAudio(t *testing.T, ext, codec, album, artist, genre string, number int) []byte {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "track."+ext)
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=0.25"}
	if ext == "mp3" {
		wav, mp3 := filepath.Join(dir, "source.wav"), filepath.Join(dir, "source.mp3")
		if out, err := exec.Command("ffmpeg", append(args, wav)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, out)
		}
		if out, err := exec.Command("lame", "--silent", wav, mp3).CombinedOutput(); err != nil {
			t.Fatalf("lame: %v: %s", err, out)
		}
		args, codec = []string{"-hide_banner", "-loglevel", "error", "-i", mp3}, "copy"
	}
	args = append(args, "-c:a", codec,
		"-metadata", "album="+album, "-metadata", "album_artist="+artist, "-metadata", "artist=Track Artist", "-metadata", "genre="+genre,
		"-metadata", "track="+fmt.Sprint(number), p)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v: %s", err, out)
	}
	audio, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return audio
}

func TestAlbumCreationRealMedia(t *testing.T) {
	e := newEnv(t)
	e.enableCreation(e.testImporter(t))
	id := store.NewID()
	flac := testFile("flac", "2.flac", creationAudio(t, "flac", "flac", "Source A", "Artist A", "Rock", 1))
	mp3 := testFile("mp3", "3.mp3", creationAudio(t, "mp3", "libmp3lame", "Source B", "Artist B", "Rock", 2))
	m4a := testFile("m4a", "11.m4a", creationAudio(t, "m4a", "aac", "Source C", "Artist C", "Rock", 2))
	cover := testFile("cover", "cover.jpg", jpegImage(t, 12, 12))
	m := testDraft(id, m4a, mp3, flac)
	m["cover"], m["genre"], m["year"], m["compilation"] = cover.declaration(), "Rock", 2001, true
	rows := m["tracks"].([]map[string]any)
	rows[1]["number"], rows[1]["title"], rows[1]["genre"] = 1, "  Edited ", ""
	rows[0]["artist"] = "Guest"
	meta, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	metadataPart := creationTestPart{data: meta, header: textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="metadata"`}, "Content-Type": {"application/json; charset=utf-8"},
	}}
	r := e.must(creationRequest(t, metadataPart, cover, flac, m4a, mp3), 201)
	if r.header.Get("Location") != albumPath(id) || r.header.Get("ETag") != ETag("album", id, 1) || r.body["id"] != id.String() || r.body["revision"] != float64(1) || r.body["title"] != "New Album" || r.body["year"] != float64(2001) || r.body["compilation"] != true {
		t.Fatalf("creation response: %+v", r)
	}
	if coverOf(r.body)["hash"] != sha(cover.data) || len(r.body["warnings"].([]any)) == 0 {
		t.Fatalf("cover or renumber warning missing: %s", r.raw)
	}
	v, err := e.svc.GetAlbum(t.Context(), id)
	if err != nil || len(v.Tracks) != 3 {
		t.Fatalf("created album: %+v %v", v, err)
	}
	for i, want := range []creationTestPart{mp3, flac, m4a} {
		track := v.Tracks[i]
		if track.Blob.Hash != sha(want.data) || track.SourcePath != want.name || int(track.No) != i+1 || track.DurationMS == nil {
			t.Fatalf("track %d = %+v", i, track)
		}
	}
	if g := v.Tracks[0].Genre; v.Tracks[0].Title != "Edited" || g == nil || *g != "" || v.Tracks[2].Artist == nil || *v.Tracks[2].Artist != "Guest" {
		t.Fatalf("overrides lost: %+v", v.Tracks)
	}
	m["title"], m["artist_name"] = "Different title", "Different Artist"
	// The duplicate needs no file parts at all. It cannot create or update anything.
	dup := e.wantError(creationRequest(t, m), 409, catalog.CodeCreationConflict)
	if dup.details()["album_id"] != id.String() || dup.header.Get("Location") != "" || dup.header.Get("ETag") != "" ||
		e.count("SELECT count(*) FROM jobs WHERE kind='render' AND requested=1") != 1 || e.count("SELECT count(*) FROM jobs") != 1 || e.count("SELECT count(*) FROM artists") != 1 {
		t.Fatalf("replay response or queue was wrong: %s", dup.raw)
	}
	conflictingFile := testFile("f", "other.flac", creationAudio(t, "flac", "flac", "Another source", "Another artist", "", 1))
	conflict := testDraft(store.NewID(), conflictingFile)
	conflict["artist_name"], conflict["title"] = " new artist ", "NEW ALBUM"
	e.wantError(creationRequest(t, conflict, conflictingFile), 409, catalog.CodeAlbumFolderConflict)
	if e.count("SELECT count(*) FROM blobs WHERE hash=$1", sha(conflictingFile.data)) != 0 ||
		!bytes.Equal(e.pinnedBytes(sha(conflictingFile.data)), conflictingFile.data) || e.count("SELECT count(*) FROM albums") != 1 ||
		e.count("SELECT count(*) FROM jobs") != 1 || e.count("SELECT count(*) FROM artists") != 1 {
		t.Fatal("folder conflict changed the catalog or lost its original")
	}
}

func TestAlbumCreationOriginalFilenames(t *testing.T) {
	e := newEnv(t)
	e.enableCreation(e.testImporter(t))
	id := store.NewID()
	quoted := testFile("quoted", `A"B.flac`, creationAudio(t, "flac", "flac", "Source", "Artist", "", 1))
	percent := testFile("percent", `A%22B.flac`, creationAudio(t, "flac", "flac", "Source", "Artist", "", 2))
	mimeEscaped := testFile("mime", `C"D.flac`, creationAudio(t, "flac", "flac", "Source", "Artist", "", 3))
	manifest := testDraft(id, quoted, percent, mimeEscaped)

	// Browser FormData sends the same filename parameter for both files.
	// Supply distinct bytes in reverse order so only the IDs can bind them.
	browserQuoted := quoted
	browserQuoted.name = `A%22B.flac`
	e.wantError(creationRequest(t, manifest, testFile(quoted.id, `A%2522B.flac`, quoted.data), percent, mimeEscaped), 422, CodeInvalidMultipart)
	e.wantError(creationRequest(t, manifest, testFile(percent.id, quoted.name, percent.data), quoted, mimeEscaped), 422, CodeInvalidMultipart)
	e.noCreationState()
	e.must(creationRequest(t, manifest, mimeEscaped, percent, browserQuoted), 201)

	v, err := e.svc.GetAlbum(t.Context(), id)
	if err != nil || len(v.Tracks) != 3 {
		t.Fatalf("created album: %+v %v", v, err)
	}
	for i, want := range []creationTestPart{quoted, percent, mimeEscaped} {
		tr := v.Tracks[i]
		if tr.SourcePath != want.name || tr.Title != strings.TrimSuffix(want.name, ".flac") || tr.Blob.Hash != sha(want.data) {
			t.Fatalf("source filename, inferred title or blob changed: track %d = %+v", i, tr)
		}
	}
}

func TestAlbumCreationRefusedMediaKeepsOriginals(t *testing.T) {
	e := newEnv(t)
	e.enableCreation(e.testImporter(t))
	audio := testFile("f", "track.flac", creationAudio(t, "flac", "flac", "Album", "Artist", "", 1))
	mp3 := testFile("m", "another.mp3", creationAudio(t, "mp3", "libmp3lame", "Album", "Artist", "", 2))
	wav := testFile("w", "unsupported.wav", creationAudio(t, "wav", "pcm_s16le", "Album", "Artist", "", 2))
	text := testFile("text", "notes.txt", []byte("not an audio file"))
	truncated := testFile("x", "truncated.flac", audio.data[:len(audio.data)/2])
	badCover := testFile("c", "cover.png", pngHeader(8000, 5001))
	id := store.NewID()
	m := testDraft(id, audio)
	m["title"] = "  "
	e.wantError(creationRequest(t, m, audio), 422, names.CodeTextEmpty)
	m = testDraft(id, audio)
	m["year"] = 0
	e.wantError(creationRequest(t, m, audio), 422, catalog.CodeInvalidYear)
	m = testDraft(id, audio)
	m["cover"] = badCover.declaration()
	e.wantError(creationRequest(t, m, audio, badCover), 422, CodeInvalidCover)
	e.wantError(creationRequest(t, testDraft(id, audio, wav), audio, wav), 422, importer.CodeUnsupportedAudio)
	e.wantError(creationRequest(t, testDraft(id, text), text), 422, importer.CodeUnsupportedAudio)
	e.wantError(creationRequest(t, testDraft(id, audio, truncated), audio, truncated), 422, importer.CodeCorruptAudio)
	duplicate := testFile("d", "copy.flac", audio.data)
	e.wantError(creationRequest(t, testDraft(id, audio, duplicate), audio, duplicate), 409, catalog.CodeTrackExists)
	m = testDraft(id, audio, mp3)
	for _, tr := range m["tracks"].([]map[string]any) {
		tr["number"] = 1
	}
	e.wantError(creationRequest(t, m, audio, mp3), 422, catalog.CodeDuplicateTrackNumber)
	m = testDraft(id, audio, mp3)
	m["genre"] = "13"
	e.wantError(creationRequest(t, m, audio, mp3), 422, catalog.CodeGenreNotWritable)
	e.noCreationState()
	for _, file := range []creationTestPart{audio, wav, text, truncated, badCover, mp3} {
		if !bytes.Equal(e.pinnedBytes(sha(file.data)), file.data) {
			t.Fatalf("original %q not retained", file.name)
		}
	}
}

func TestAlbumCreationSlotBudgetAndCancellation(t *testing.T) {
	e := newEnv(t)
	file := testFile("one", "one.flac", []byte("a file pinned before cancellation"))
	post := creationRequest(t, testDraft(store.NewID(), file), file)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, "POST", post.path, bytes.NewReader(post.body.([]byte)))
	r.Host, r.Header = testHost, nethttp.Header{"Content-Type": post.headers["Content-Type"], RequestHeader: {"1"}}
	r.AddCookie(&nethttp.Cookie{Name: sessionCookie, Value: e.session})
	e.enableCreation(prepareFunc(func(ctx context.Context, _ importer.Creation) (catalog.AlbumCandidate, []jobs.Warning, error) {
		if n := len(e.api.uploads); n != 0 || e.budget.Reserved() != int64(len(file.data)+2*MaxCoverBytes) {
			t.Errorf("preparation holds %d slots and %d bytes", n, e.budget.Reserved())
		}
		cancel()
		return catalog.AlbumCandidate{}, nil, ctx.Err()
	}))
	w := httptest.NewRecorder()
	e.api.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"upload_incomplete"`) {
		t.Fatalf("cancelled prep: %d %s", w.Code, w.Body)
	}
	e.noCreationState()
	if !bytes.Equal(e.pinnedBytes(sha(file.data)), file.data) {
		t.Fatal("cancelled request removed a pinned blob")
	}
	e.enableCreation(prepareFunc(func(context.Context, importer.Creation) (catalog.AlbumCandidate, []jobs.Warning, error) {
		return catalog.AlbumCandidate{}, nil, &fsops.Error{Code: fsops.CodeNoSpace, Op: "put cover", Path: "/not-for-clients"}
	}))
	res := e.wantError(post, 507, CodeInsufficientSpace)
	if bytes.Contains(res.raw, []byte("/not-for-clients")) {
		t.Fatalf("filesystem error leaked: %s", res.raw)
	}
	e.noCreationState()
	e.enableCreation(prepareFunc(func(context.Context, importer.Creation) (catalog.AlbumCandidate, []jobs.Warning, error) {
		return catalog.AlbumCandidate{}, nil, fmt.Errorf("native tool stderr: %w", context.DeadlineExceeded)
	}))
	res = e.wantError(post, 500, CodeInternal)
	if bytes.Contains(res.raw, []byte("native tool stderr")) {
		t.Fatalf("native tool failure leaked: %s", res.raw)
	}
	e.noCreationState()
	e.enableCreation(forbidPreparation(t))
	hold, _ := e.budget.Reserve(1<<62, 1<<61)
	if hold == nil {
		t.Fatal("could not reserve test budget")
	}
	e.wantError(post, 507, CodeInsufficientSpace)
	hold.Release()
	e.noCreationState()

	// A corrupt existing original must be refused, never overwritten or
	// trusted just because the submitted bytes hash to its path.
	h := sha(file.data)
	p := filepath.Join(e.data, "originals", h[:2], h[2:4], h)
	bad := bytes.Repeat([]byte{'X'}, len(file.data))
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	e.wantError(post, 500, CodeInternal)
	if !bytes.Equal(e.pinnedBytes(h), bad) {
		t.Fatal("corrupt existing original was replaced")
	}
	e.noCreationState()
}

func TestAlbumCreationConcurrentSameID(t *testing.T) {
	e := newEnv(t)
	prepared := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	im := e.testImporter(t)
	e.enableCreation(prepareFunc(func(ctx context.Context, c importer.Creation) (catalog.AlbumCandidate, []jobs.Warning, error) {
		prepared <- struct{}{}
		<-release
		return im.PrepareCreation(ctx, c)
	}))
	id := store.NewID()
	file := testFile("t", "track.flac", creationAudio(t, "flac", "flac", "Album", "Artist", "", 1))
	r1 := creationRequest(t, testDraft(id, file), file)
	m := testDraft(id, file)
	m["artist_name"], m["title"] = "Second Artist", "Second Album"
	r2 := creationRequest(t, m, file)
	out := make(chan resp, 2)
	go func() { out <- e.do(r1) }()
	go func() { out <- e.do(r2) }()
	wait(t, prepared, "first preparation")
	wait(t, prepared, "second preparation")
	if n := len(e.api.uploads); n != 0 || e.budget.Reserved() != 2*int64(len(file.data)+2*MaxCoverBytes) {
		t.Fatalf("concurrent preparation holds %d slots and %d bytes", n, e.budget.Reserved())
	}
	once.Do(func() { close(release) })
	a, b := <-out, <-out
	if a.status == 409 {
		a, b = b, a
	}
	if a.status != 201 || b.status != 409 || b.code() != catalog.CodeCreationConflict || b.details()["album_id"] != id.String() ||
		e.count("SELECT count(*) FROM artists") != 1 || e.count("SELECT count(*) FROM jobs") != 1 {
		t.Fatalf("same creation ID: %d %s; %d %s", a.status, a.raw, b.status, b.raw)
	}
}

func TestAlbumCreationArtistRace(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%v", remove), func(t *testing.T) {
			e := newEnv(t)
			a := e.seedReal("Old Artist", "Existing Album", catalog.FormatFLAC)
			old, err := e.svc.GetAlbum(t.Context(), a.id)
			if err != nil {
				t.Fatal(err)
			}
			im := e.testImporter(t)
			e.enableCreation(prepareFunc(func(ctx context.Context, c importer.Creation) (catalog.AlbumCandidate, []jobs.Warning, error) {
				if c.Artist != "Old Artist" {
					t.Errorf("preparation used an unexpected label: %q", c.Artist)
				}
				if remove {
					if _, _, err := e.svc.TrashAlbum(ctx, a.id, 1); err != nil {
						return catalog.AlbumCandidate{}, nil, err
					}
					// Complete the removal of the never-published album.
					e.exec("DELETE FROM jobs WHERE kind='render' AND album_id=$1", a.id)
					if _, _, err := e.svc.EmptyTrash(ctx); err != nil {
						return catalog.AlbumCandidate{}, nil, err
					}
				} else if _, _, err := e.svc.RenameArtist(ctx, old.ArtistID, 1, "Renamed Artist"); err != nil {
					return catalog.AlbumCandidate{}, nil, err
				}
				return im.PrepareCreation(ctx, c)
			}))
			id := store.NewID()
			file := testFile("t", "track.flac", creationAudio(t, "flac", "flac", "Album", "Artist", "", 1))
			m := testDraft(id, file)
			delete(m, "artist_name")
			m["artist_id"] = old.ArtistID.String()
			post := creationRequest(t, m, file)
			if remove {
				e.wantError(post, 404, catalog.CodeArtistNotFound)
				if e.count("SELECT count(*) FROM artists") != 0 || e.count("SELECT count(*) FROM albums") != 0 ||
					e.count("SELECT count(*) FROM jobs WHERE kind='render'") != 0 || e.count("SELECT count(*) FROM blobs WHERE hash=$1", sha(file.data)) != 0 {
					t.Fatal("creation after the artist's deletion left catalog state")
				}
				if !bytes.Equal(file.data, e.pinnedBytes(sha(file.data))) {
					t.Fatal("the failed creation changed its original")
				}
			} else {
				e.must(post, 201)
				v, err := e.svc.GetAlbum(t.Context(), id)
				if err != nil || v.ArtistName != "Renamed Artist" || v.ArtistID != old.ArtistID || v.Revision != 1 {
					t.Fatalf("the artist's old state won: %+v, %v", v, err)
				}
			}
		})
	}
}

func TestAlbumCreationUncertainCommit(t *testing.T) {
	dbURL := pgtest.EmptyDB(t)
	direct := pgtest.Pool(t, dbURL)
	if err := store.Migrate(t.Context(), direct); err != nil {
		t.Fatal(err)
	}
	proxy := pgtest.NewProxy(t, dbURL)
	e := newEnvOn(t, pgtest.Pool(t, proxy.URL), true)
	im := e.testImporter(t)
	e.enableCreation(prepareFunc(func(ctx context.Context, c importer.Creation) (catalog.AlbumCandidate, []jobs.Warning, error) {
		out, warnings, err := im.PrepareCreation(ctx, c)
		if err == nil {
			proxy.LoseNextCommitAck()
		}
		return out, warnings, err
	}))
	id := store.NewID()
	file := testFile("t", "track.flac", creationAudio(t, "flac", "flac", "Album", "Artist", "", 1))
	post := creationRequest(t, testDraft(id, file), file)
	r := e.wantError(post, 503, store.CodeCommitUncertain)
	if e.fatalCount() != 1 || r.header.Get("Location") != "" || r.header.Get("ETag") != "" {
		t.Fatalf("uncertain commit claimed success or lacked fatal handling: %+v", r)
	}
	e.wantError(req{method: "GET", path: albumPath(id)}, 503, store.CodeCommitUncertain)
	// Re-enable as after recovery; the same known ID is resolvable and not replayable.
	e.enableCreation(im)
	e.must(req{method: "GET", path: albumPath(id)}, 200)
	e.wantError(post, 409, catalog.CodeCreationConflict)
	if e.count("SELECT count(*) FROM albums") != 1 || e.count("SELECT count(*) FROM jobs") != 1 {
		t.Fatal("an uncertain commit was retried as another creation")
	}
}
