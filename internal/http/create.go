package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	nethttp "net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/fsops"
	"musiclib/internal/importer"
	"musiclib/internal/jobs"
	"musiclib/internal/store"
)

const (
	maxCreationMetadata = 8 << 20
	maxCreationFileName = 255
	// NextRawPart may already have a 4 KiB read-ahead buffer. Limit its
	// additional reads, and count both those and all other raw reads in
	// the whole-body ceiling (including preamble, separators and epilogue).
	creationHeaderRead = 12 << 10
	creationOverhead   = 16 << 10
	maxCreationBytes   = int64(catalog.MaxTracks)*MaxTrackBytes + MaxCoverBytes + maxCreationMetadata + (catalog.MaxTracks+3)*creationOverhead
)

var (
	creationPartID   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	creationBoundary = regexp.MustCompile(`^[A-Za-z0-9'()+_,./:=? -]{1,70}$`)
)

// AlbumPreparer inspects the pinned originals outside the upload slot and
// catalog transaction. The importer's production implementation may pin a cover.
type AlbumPreparer interface {
	PrepareCreation(context.Context, importer.Creation) (catalog.AlbumCandidate, []jobs.Warning, error)
}

type creationFile struct {
	id, name  string
	size, max int64
}

type creationDraft struct {
	artistID uuid.UUID
	input    importer.Creation
	files    []creationFile // tracks in declaration order, then cover if present
	bytes    int64          // exact sum of file sizes
}

// Signal close before writing a refusal with a potentially unread creation
// body. Otherwise net/http may drain HTTP/1 body bytes before sending it.
func closeCreationOnRefusal(w nethttp.ResponseWriter, r *nethttp.Request) {
	if r.ProtoMajor == 1 && r.Method == nethttp.MethodPost && r.URL.Path == "/api/albums" {
		w.Header().Set("Connection", "close")
	}
}

func (h *handlers) postAlbum(w nethttp.ResponseWriter, r *nethttp.Request) {
	draft, id, res, err := h.receiveCreation(w, r)
	if err != nil {
		closeCreationOnRefusal(w, r)
		h.creationRefused(w, r, err)
		return
	}
	ctx := r.Context()
	c, warnings, err := func() (catalog.AlbumCandidate, []jobs.Warning, error) {
		defer res.Release()
		if err := ctx.Err(); err != nil {
			return catalog.AlbumCandidate{}, nil, err
		}
		if err := h.pinned(); err != nil {
			return catalog.AlbumCandidate{}, nil, err
		}
		return h.creation.PrepareCreation(ctx, draft.input)
	}()
	if err == nil && ctx.Err() != nil {
		err = incompleteUpload()
	}
	if err != nil {
		h.creationRefused(w, r, err)
		return
	}
	v, err := h.catalog.CreateAlbum(ctx, id, draft.artistID, c)
	if err != nil {
		h.creationRefused(w, r, err)
		return
	}
	w.Header().Set("Location", "/api/albums/"+id.String())
	w.Header().Set("ETag", ETag("album", id, v.Revision))
	h.api.writeJSON(w, nethttp.StatusCreated, albumWithWarnings(v, warnings))
}

func (h *handlers) creationRefused(w nethttp.ResponseWriter, r *nethttp.Request, err error) {
	var e *Error
	switch {
	case store.IsFatal(err), store.Code(err) == store.CodeCanceled:
		h.api.fail(w, r, err)
	case errors.As(err, &e):
		h.api.writeError(w, e)
	case importer.Code(err) == importer.CodeInsufficientSpace, blobstore.Code(err) == blobstore.CodeNoSpace, fsops.Code(err) == fsops.CodeNoSpace:
		h.api.writeError(w, newError(nethttp.StatusInsufficientStorage, CodeInsufficientSpace, "the disk is full while preparing the album"))
	case r.Context().Err() != nil && errors.Is(err, r.Context().Err()):
		// A native tool hitting its own deadline is not a client disconnect.
		h.api.writeError(w, incompleteUpload())
	default:
		h.trackRefused(w, r, err)
	}
}

func (h *handlers) receiveCreation(w nethttp.ResponseWriter, r *nethttp.Request) (creationDraft, uuid.UUID, *jobs.Reservation, error) {
	var draft creationDraft
	boundary, e := parseCreationType(r.Header)
	if e != nil {
		return draft, uuid.Nil, nil, e
	}
	if e := declaredTooLarge(r, maxCreationBytes); e != nil {
		return draft, uuid.Nil, nil, e
	}
	if h.creation == nil {
		return draft, uuid.Nil, nil, newError(nethttp.StatusServiceUnavailable, CodeNotReady, "album creation is not available")
	}
	release, slotErr, err := h.uploadSlot(r)
	if slotErr != nil {
		return draft, uuid.Nil, nil, slotErr
	}
	if err != nil {
		return draft, uuid.Nil, nil, err
	}
	defer release()

	parts := newCreationParts(w, r, boundary)
	obj, n, err := parts.metadata()
	if err != nil {
		return draft, uuid.Nil, nil, err
	}
	id, e := creationID(obj, "creation_id")
	if e != nil {
		return draft, uuid.Nil, nil, e
	}
	ctx := r.Context()
	if err := h.catalog.CheckCreationID(ctx, id); err != nil {
		return draft, uuid.Nil, nil, err
	}
	draft, e = decodeCreationDraft(obj)
	if e != nil {
		return draft, uuid.Nil, nil, e
	}
	// The inventory is validated and its sum is bounded by maxCreationBytes.
	parts.limit = draft.bytes + n + int64(len(draft.files)+2)*creationOverhead
	if r.ContentLength > parts.limit || parts.read > parts.limit {
		return draft, uuid.Nil, nil, tooLarge(parts.limit)
	}
	if draft.artistID != uuid.Nil {
		a, err := h.catalog.GetArtist(ctx, draft.artistID)
		if err != nil {
			return draft, uuid.Nil, nil, err
		}
		draft.input.Artist = a.Name
	}
	// Only automatic cover selection needs extraction space: one temporary
	// image and its put. The explicit cover is already in the inventory.
	estimate := draft.bytes
	if len(draft.files) == len(draft.input.Tracks) {
		estimate += 2 * MaxCoverBytes
	}
	res, err := h.reserveUpload(estimate)
	if err != nil {
		return draft, uuid.Nil, nil, err
	}
	preparedToInspect := false
	defer func() {
		if !preparedToInspect {
			res.Release()
		}
	}()
	if err := h.copyCreation(ctx, parts, &draft); err != nil {
		return draft, uuid.Nil, nil, err
	}
	preparedToInspect = true
	return draft, id, res, nil
}

func (h *handlers) copyCreation(ctx context.Context, parts *creationParts, draft *creationDraft) error {
	pending := make(map[string]int, len(draft.files))
	for i, f := range draft.files {
		pending[f.id] = i
	}
	p, err := parts.next()
	if err != nil && err != io.EOF {
		return err
	}
	for range draft.files {
		if p == nil {
			return invalidMultipart("one or more declared file parts are missing")
		}
		name, filename, ok := creationDisposition(p)
		i, found := pending[name]
		if !ok || !found || !creationFilenameMatches(draft.files[i].name, filename) {
			return invalidMultipart("file part is undeclared, repeated, or does not match its declared filename")
		}
		delete(pending, name)
		f := draft.files[i]
		parts.setReadLimit(math.MaxInt64)
		reader := &creationFileReader{part: p, body: parts, file: f, left: f.size}
		b, err := h.putUpload(ctx, reader, f.max)
		if err != nil {
			return err
		}
		p = reader.following
		if i < len(draft.input.Tracks) {
			draft.input.Tracks[i].Blob = b
		} else {
			draft.input.Cover = &b
		}
	}
	if p != nil {
		return invalidMultipart("unexpected part after the declared files")
	}
	// The multipart reader may have buffered some epilogue, already counted
	// by Read. Drain the rest with the same raw ceiling before allowing SQL.
	parts.setReadLimit(math.MaxInt64)
	if _, err := io.Copy(io.Discard, parts); err != nil {
		return parts.readError(err)
	}
	if parts.req.ContentLength >= 0 && parts.req.ContentLength != parts.read {
		return incompleteUpload()
	}
	if ctx.Err() != nil {
		return incompleteUpload()
	}
	return nil
}

func parseCreationType(h nethttp.Header) (string, *Error) {
	v := h.Values("Content-Type")
	if len(v) == 1 {
		mt, p, err := mime.ParseMediaType(v[0])
		if mt == "multipart/form-data" {
			b := p["boundary"]
			if err != nil || len(p) != 1 || !creationBoundary.MatchString(b) || strings.HasSuffix(b, " ") {
				return "", invalidMultipart("the multipart boundary is missing or invalid")
			}
			return b, nil
		}
	}
	return "", newError(nethttp.StatusUnsupportedMediaType, CodeUnsupportedMediaType, "the body must be multipart/form-data")
}

func invalidMultipart(message string) *Error {
	return newError(nethttp.StatusUnprocessableEntity, CodeInvalidMultipart, "%s", message)
}

// creationParts counts every body byte pulled through the multipart parser.
// Each parsing stage can further restrict how much raw data it can read.
// The first raw failure is sticky, even if the MIME parser buffered bytes.
type creationParts struct {
	w           nethttp.ResponseWriter
	req         *nethttp.Request
	parts       *multipart.Reader
	src         io.Reader
	limit, read int64
	readCap     int64
	rawErr      error
}

func newCreationParts(w nethttp.ResponseWriter, r *nethttp.Request, boundary string) *creationParts {
	b := &creationParts{w: w, req: r, limit: maxCreationBytes}
	b.parts = multipart.NewReader(b, boundary)
	return b
}

func (b *creationParts) setReadLimit(window int64) {
	remaining := b.limit - b.read
	b.readCap = b.limit
	if window < remaining {
		b.readCap = window
	}
	b.src = nethttp.MaxBytesReader(b.w, b.req.Body, min(window, remaining))
}

func (b *creationParts) Read(p []byte) (int, error) {
	if b.rawErr != nil {
		return 0, b.rawErr
	}
	if err := b.req.Context().Err(); err != nil {
		b.rawErr = err
		return 0, err
	}
	n, err := b.src.Read(p)
	b.read += int64(n)
	if err != nil && err != io.EOF {
		b.rawErr = err
	}
	return n, err
}

func (b *creationParts) next() (*multipart.Part, error) {
	b.setReadLimit(creationHeaderRead)
	p, err := b.parts.NextRawPart()
	if b.rawErr != nil || (err != nil && err != io.EOF) {
		return nil, b.readError(err)
	}
	return p, err
}

func (b *creationParts) readError(err error) *Error {
	var cap *nethttp.MaxBytesError
	switch {
	case errors.As(b.rawErr, &cap), errors.As(err, &cap):
		return tooLarge(b.readCap)
	case b.rawErr != nil:
		return incompleteUpload()
	default:
		return invalidMultipart("the multipart body or its delimiters are malformed")
	}
}

var creationKeys = []string{"creation_id", "artist_id?", "artist_name?", "title", "year?", "genre?", "compilation?", "tracks", "cover?"}

func (b *creationParts) metadata() (object, int64, error) {
	p, err := b.next()
	if err == io.EOF {
		return object{}, 0, invalidMultipart("metadata must be the first part")
	}
	if err != nil {
		return object{}, 0, err
	}
	if name, filename, ok := creationDisposition(p); !ok || name != "metadata" || filename != "" {
		return object{}, 0, invalidMultipart("metadata must be the first part, without a filename")
	}
	if len(p.Header.Values("Content-Type")) != 0 && checkContentType(nethttp.Header(p.Header)) != nil {
		return object{}, 0, invalidMultipart("metadata must have no Content-Type or application/json")
	}
	b.setReadLimit(maxCreationMetadata + creationOverhead)
	data, err := io.ReadAll(io.LimitReader(p, maxCreationMetadata+1))
	if len(data) > maxCreationMetadata {
		return object{}, 0, tooLarge(maxCreationMetadata)
	}
	if err != nil || b.rawErr != nil {
		return object{}, 0, b.readError(err)
	}
	if e := checkJSONDepth(data, 16); e != nil {
		return object{}, 0, e
	}
	o, e := decodeObject(data, "", creationKeys...)
	if e != nil {
		return object{}, 0, e
	}
	return o, int64(len(data)), nil
}

// creationDisposition reads MIME parameter values, not Part.FileName's
// basename. Require literal parameters: extended/continued parameters can
// override a different raw name, and duplicate parameters are ambiguous.
func creationDisposition(p *multipart.Part) (name, filename string, ok bool) {
	v := p.Header.Values("Content-Disposition")
	if len(v) != 1 || len(p.Header.Values("Content-Transfer-Encoding")) != 0 {
		return "", "", false
	}
	mt, params, err := mime.ParseMediaType(v[0])
	if err != nil || mt != "form-data" || params["name"] == "" {
		return "", "", false
	}
	_, rest, more := strings.Cut(v[0], ";")
	seen := map[string]bool{}
	for more {
		key, value, found := strings.Cut(rest, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		if !found || (key != "name" && key != "filename") || seen[key] {
			return "", "", false
		}
		seen[key] = true
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			// Allow a MIME-escaped quote, but not another use of a
			// backslash that the parser could silently unescape.
			value = value[1:]
			for {
				i := strings.IndexAny(value, `"\`)
				if i < 0 || (value[i] == '\\' && (i+1 == len(value) || value[i+1] != '"')) {
					return "", "", false
				}
				if value[i] == '"' {
					value = value[i+1:]
					break
				}
				value = value[i+2:]
			}
		}
		tail, next, hasNext := strings.Cut(value, ";")
		if strings.ContainsRune(tail, '\\') {
			return "", "", false
		}
		rest, more = next, hasNext
	}
	filename, hasFilename := params["filename"]
	// A browser can expand each original quote to three wire bytes.
	if hasFilename && (len(filename) > 3*maxCreationFileName || !creationFileComponent(filename)) {
		return "", "", false
	}
	return params["name"], filename, true
}

// creationFileReader only gives Put an EOF after confirming the size and
// the MIME part end. In particular, reaching the declared length is not EOF.
type creationFileReader struct {
	part      *multipart.Part
	body      *creationParts
	file      creationFile
	left      int64
	following *multipart.Part
}

func (r *creationFileReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	probing := r.left == 0
	if probing {
		p = p[:1]
	} else if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.part.Read(p)
	if r.body.rawErr != nil || (err != nil && err != io.EOF) {
		return 0, r.body.readError(err)
	}
	if probing && n != 0 {
		if r.file.size == r.file.max {
			return 0, tooLarge(r.file.max)
		}
		return 0, invalidMultipart("a file part exceeds its declared size")
	}
	r.left -= int64(n)
	if err == io.EOF {
		if r.left != 0 {
			return 0, invalidMultipart("a file part is shorter than its declared size")
		}
		// A Part can report EOF on a boundary prefix; NextRawPart
		// validates the whole delimiter before Put can trust that EOF.
		var nextErr error
		r.following, nextErr = r.body.next()
		if nextErr != nil && nextErr != io.EOF {
			return 0, nextErr
		}
	}
	return n, err
}

func creationID(o object, key string) (uuid.UUID, *Error) {
	id, e := o.ID(key)
	if e == nil && id == uuid.Nil {
		e = invalidField(joinPath(o.path, key), "must not be the nil UUID")
	}
	return id, e
}

func decodeCreationDraft(o object) (creationDraft, *Error) {
	var d creationDraft
	var e *Error
	_, byID := o.fields["artist_id"]
	_, byName := o.fields["artist_name"]
	if byID == byName {
		return d, invalidField("", "must contain exactly one of artist_id or artist_name")
	}
	if byID {
		d.artistID, e = creationID(o, "artist_id")
	} else {
		d.input.Artist, e = o.String("artist_name")
	}
	if e != nil {
		return d, e
	}
	if d.input.Title, e = o.String("title"); e != nil {
		return d, e
	}
	if d.input.Year, e = creationOptional(o, "year", object.Int); e != nil {
		return d, e
	}
	if d.input.Genre, e = creationOptional(o, "genre", object.String); e != nil {
		return d, e
	}
	if d.input.Compilation, e = creationOptional(o, "compilation", object.Bool); e != nil {
		return d, e
	}
	var rows []json.RawMessage
	if e := o.field("tracks", "must be an array", &rows); e != nil {
		return d, e
	}
	if len(rows) == 0 {
		return d, newError(nethttp.StatusUnprocessableEntity, catalog.CodeNoTracks, "an album needs at least one track")
	}
	if len(rows) > catalog.MaxTracks {
		return d, newError(nethttp.StatusRequestEntityTooLarge, CodeBodyTooLarge, "an album can have at most %d tracks", catalog.MaxTracks).with("limit", catalog.MaxTracks)
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for i, raw := range rows {
		t, e := decodeObject(raw, fmt.Sprintf("tracks[%d]", i), "part_id", "name", "size", "title?", "disc?", "number?", "artist?", "genre?")
		if e != nil {
			return d, e
		}
		f, e := decodeCreationFile(t, MaxTrackBytes)
		if e != nil {
			return d, e
		}
		if names[f.name] {
			return d, invalidMultipart("two tracks declare the same source filename")
		}
		names[f.name] = true
		if e := d.addFile(f, ids); e != nil {
			return d, e
		}
		track, e := decodeCreationTrack(t, f.name)
		if e != nil {
			return d, e
		}
		d.input.Tracks = append(d.input.Tracks, track)
	}
	if raw, has := o.fields["cover"]; has {
		cover, e := decodeObject(raw, "cover", "part_id", "name", "size")
		if e != nil {
			return d, e
		}
		f, e := decodeCreationFile(cover, MaxCoverBytes)
		if e != nil {
			return d, e
		}
		if e := d.addFile(f, ids); e != nil {
			return d, e
		}
	}
	return d, nil
}

func (d *creationDraft) addFile(f creationFile, seen map[string]bool) *Error {
	if seen[f.id] {
		return invalidMultipart("part IDs must be globally unique")
	}
	if f.size > math.MaxInt64-d.bytes {
		return tooLarge(maxCreationBytes)
	}
	seen[f.id] = true
	d.bytes += f.size
	d.files = append(d.files, f)
	return nil
}

func decodeCreationFile(o object, limit int64) (creationFile, *Error) {
	f := creationFile{max: limit}
	var e *Error
	if f.id, e = o.String("part_id"); e != nil {
		return f, e
	}
	if f.id == "metadata" || !creationPartID.MatchString(f.id) {
		return f, invalidMultipart("invalid or reserved part ID")
	}
	if f.name, e = o.String("name"); e != nil {
		return f, e
	}
	if !creationFileName(f.name) {
		return f, invalidMultipart("filenames must be single UTF-8 components of 1 to 255 bytes without controls")
	}
	if e := o.field("size", "must be an integer byte count", &f.size); e != nil {
		return f, e
	}
	if f.size > limit {
		return f, tooLarge(limit)
	}
	if f.size <= 0 {
		return f, invalidMultipart("file sizes must be positive")
	}
	return f, nil
}

func creationFileName(s string) bool {
	return len(s) <= maxCreationFileName && creationFileComponent(s)
}

func creationFileComponent(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`) && utf8.ValidString(s) && strings.IndexFunc(s, unicode.IsControl) < 0
}

// Match the original name or its browser FormData quote spelling after MIME
// parsing. Never percent-decode: %22 can itself be part of an original name.
func creationFilenameMatches(original, filename string) bool {
	return filename == original || filename == strings.ReplaceAll(original, `"`, "%22")
}

func decodeCreationTrack(o object, name string) (importer.CreationTrack, *Error) {
	t := importer.CreationTrack{Name: name}
	var e *Error
	if t.Title, e = creationOptional(o, "title", object.String); e != nil {
		return t, e
	}
	if t.Artist, e = creationOptional(o, "artist", object.String); e != nil {
		return t, e
	}
	if t.Genre, e = creationOptional(o, "genre", object.String); e != nil {
		return t, e
	}
	if t.Disc, e = creationOptional(o, "disc", object.Int); e != nil {
		return t, e
	}
	t.Number, e = creationOptional(o, "number", object.Int)
	return t, e
}

func creationOptional[T any](o object, key string, read func(object, string) (T, *Error)) (*T, *Error) {
	if _, ok := o.fields[key]; !ok {
		return nil, nil
	}
	v, e := read(o, key)
	return &v, e
}
