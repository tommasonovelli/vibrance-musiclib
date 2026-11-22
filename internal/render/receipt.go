package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
)

// ReceiptSchemaVersion is the version written by new builds.
const ReceiptSchemaVersion = 2

const (
	legacyReceiptVersion  = 1
	maxLegacyReceiptBytes = 16 << 20
	storeField            = `,"store_id":`
	trackField            = `,"track_id":`
	sourceField           = `,"source_blob_sha256":`
	storeFieldBytes       = len(storeField) + 2 + 36
	trackFieldsBytes      = len(trackField) + 2 + 36 + len(sourceField) + 2 + 64

	// MaxReceiptBytes is the absolute read bound for either version. V2
	// adds only canonical store/track metadata to the legacy byte budget;
	// attachments have no aggregate count bound. Freeze the on-disk track
	// allowance so a future catalog limit change cannot invalidate receipts.
	maxReceiptTracks = 1000
	MaxReceiptBytes  = maxLegacyReceiptBytes + storeFieldBytes + maxReceiptTracks*trackFieldsBytes
)

// Every track in a valid catalog album must fit the on-disk allowance.
var _ [maxReceiptTracks - catalog.MaxTracks]struct{}

// Receipt is the content of .musiclib.json (§9.2), exactly: no names, no
// titles, no timestamps, and not itself among its files.
type Receipt struct {
	SchemaVersion int
	StoreID       uuid.UUID // Nil only for schema 1, which has no store field.
	AlbumID       uuid.UUID
	BuildID       uuid.UUID
	AlbumRevision int64
	RenderVersion string
	// Files are sorted by the bytes of RelativePath (UTF-8), strictly: no
	// path twice.
	Files []ReceiptFile
}

// ReceiptFile is one file of the album directory, relative to it.
type ReceiptFile struct {
	RelativePath     string
	Size             int64
	SHA256           string
	TrackID          uuid.UUID // Nil for files that are not catalog track outputs.
	SourceBlobSHA256 string
}

// Encode serializes the receipt in its one canonical form, which is the
// file's bytes and whose SHA-256 is the receipt_hash (§9.2):
//
//	{"schema_version":1,"album_id":"<uuid>","build_id":"<uuid>",
//	 "album_revision":<n>,"render_version":"<s>",
//	 "files":[{"relative_path":"<s>","size":<n>,"sha256":"<hex>"},...]}
//
// on one line (the line break above is for reading only):
//   - compact JSON (RFC 8259), the keys in exactly this order, no space, no
//     trailing newline;
//   - UUIDs in lowercase 8-4-4-4-12 form; integers in decimal without sign,
//     leading zero or exponent;
//   - strings raw UTF-8, with only `"`, `\` and the control characters
//     U+0000..U+001F escaped: `\"`, `\\`, and `\u00XX` with lowercase hex.
//     Nothing else is escaped: not "<", ">", "&", U+2028 or U+2029 (unlike
//     encoding/json, whose choices this format does not depend on).
//
// Schema 2 inserts store_id after schema_version and adds the optional
// track_id and source_blob_sha256 pair after a file's sha256.
//
// It refuses a receipt that ParseReceipt would refuse: the two are exact
// inverses.
func (r Receipt) Encode() ([]byte, error) {
	extra, err := r.validate()
	if err != nil {
		return nil, err
	}
	data := r.encode()
	if err := checkReceiptSize(len(data), extra); err != nil {
		return nil, err
	}
	return data, nil
}

func (r Receipt) encode() []byte {
	var b bytes.Buffer
	b.WriteString(`{"schema_version":`)
	b.WriteString(strconv.Itoa(r.SchemaVersion))
	if r.SchemaVersion == ReceiptSchemaVersion {
		b.WriteString(storeField)
		writeString(&b, r.StoreID.String())
	}
	b.WriteString(`,"album_id":`)
	writeString(&b, r.AlbumID.String())
	b.WriteString(`,"build_id":`)
	writeString(&b, r.BuildID.String())
	b.WriteString(`,"album_revision":`)
	b.WriteString(strconv.FormatInt(r.AlbumRevision, 10))
	b.WriteString(`,"render_version":`)
	writeString(&b, r.RenderVersion)
	b.WriteString(`,"files":[`)
	for i, f := range r.Files {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"relative_path":`)
		writeString(&b, f.RelativePath)
		b.WriteString(`,"size":`)
		b.WriteString(strconv.FormatInt(f.Size, 10))
		b.WriteString(`,"sha256":`)
		writeString(&b, f.SHA256)
		if f.TrackID != uuid.Nil {
			b.WriteString(trackField)
			writeString(&b, f.TrackID.String())
			b.WriteString(sourceField)
			writeString(&b, f.SourceBlobSHA256)
		}
		b.WriteByte('}')
	}
	b.WriteString("]}")
	return b.Bytes()
}

// writeString writes s, valid UTF-8, as a JSON string of the canonical form.
func writeString(b *bytes.Buffer, s string) {
	const hexDigits = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

// ReceiptHash is the receipt_hash of encoded receipt bytes: their SHA-256 in
// lowercase hex (§9.2).
func ReceiptHash(encoded []byte) string {
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// receiptJSON is the shape ParseReceipt decodes before checking that the
// input is exactly the canonical encoding of the result.
type receiptJSON struct {
	SchemaVersion *int              `json:"schema_version"`
	StoreID       *string           `json:"store_id"`
	AlbumID       *string           `json:"album_id"`
	BuildID       *string           `json:"build_id"`
	AlbumRevision *int64            `json:"album_revision"`
	RenderVersion *string           `json:"render_version"`
	Files         []receiptFileJSON `json:"files"`
}

type receiptFileJSON struct {
	RelativePath     *string `json:"relative_path"`
	Size             *int64  `json:"size"`
	SHA256           *string `json:"sha256"`
	TrackID          *string `json:"track_id"`
	SourceBlobSHA256 *string `json:"source_blob_sha256"`
}

// ParseReceipt reads a receipt strictly: the publisher's recovery (§9.4) and
// doctor (§11.3) trust nothing else. It accepts exactly the bytes Encode
// produces, so it refuses unknown, missing or repeated fields, another key
// order, whitespace, another escaping or number form, and a trailing
// newline. It validates schema 1 or 2, including version-specific identity
// and provenance, and the common file inventory. Any refusal is CodeReceiptInvalid.
func ParseReceipt(data []byte) (Receipt, error) {
	fail := func(format string, args ...any) (Receipt, error) {
		return Receipt{}, errorf(CodeReceiptInvalid, "the receipt is not valid: "+format, args...)
	}
	if len(data) > MaxReceiptBytes {
		return fail("%d bytes, the maximum is %d", len(data), MaxReceiptBytes)
	}
	if !utf8.Valid(data) {
		return fail("not valid UTF-8")
	}
	var j receiptJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&j); err != nil {
		return fail("%v", err)
	}
	if dec.More() {
		return fail("data after the receipt")
	}
	if j.SchemaVersion == nil || j.AlbumID == nil || j.BuildID == nil || j.AlbumRevision == nil ||
		j.RenderVersion == nil || j.Files == nil {
		return fail("a field is missing")
	}
	switch *j.SchemaVersion {
	case legacyReceiptVersion, ReceiptSchemaVersion:
	default:
		return fail("unknown schema_version %d", *j.SchemaVersion)
	}
	albumID, err1 := uuid.Parse(*j.AlbumID)
	buildID, err2 := uuid.Parse(*j.BuildID)
	if err1 != nil || err2 != nil {
		return fail("album_id or build_id is not a UUID")
	}
	r := Receipt{SchemaVersion: *j.SchemaVersion, AlbumID: albumID, BuildID: buildID,
		AlbumRevision: *j.AlbumRevision, RenderVersion: *j.RenderVersion, Files: make([]ReceiptFile, len(j.Files))}
	if j.StoreID != nil {
		id, err := uuid.Parse(*j.StoreID)
		if err != nil || id == uuid.Nil {
			return fail("store_id is not a non-nil UUID")
		}
		r.StoreID = id
	}
	for i, f := range j.Files {
		if f.RelativePath == nil || f.Size == nil || f.SHA256 == nil {
			return fail("file %d misses a field", i)
		}
		file := ReceiptFile{RelativePath: *f.RelativePath, Size: *f.Size, SHA256: *f.SHA256}
		if (f.TrackID == nil) != (f.SourceBlobSHA256 == nil) {
			return fail("file %d has incomplete track provenance", i)
		}
		if f.TrackID != nil {
			id, err := uuid.Parse(*f.TrackID)
			if err != nil || id == uuid.Nil {
				return fail("file %d: track_id is not a non-nil UUID", i)
			}
			file.TrackID, file.SourceBlobSHA256 = id, *f.SourceBlobSHA256
		}
		r.Files[i] = file
	}
	extra, err := r.validate()
	if err != nil {
		return Receipt{}, err
	}
	if err := checkReceiptSize(len(data), extra); err != nil {
		return Receipt{}, err
	}
	if !bytes.Equal(r.encode(), data) {
		return fail("not in the canonical form")
	}
	return r, nil
}

// validate checks shared content rules and returns the exact extra bytes of
// canonical v2 provenance. Neither version can spend that on common fields.
func (r Receipt) validate() (int, error) {
	fail := func(format string, args ...any) (int, error) {
		return 0, errorf(CodeReceiptInvalid, "the receipt is not valid: "+format, args...)
	}
	extra := 0
	switch r.SchemaVersion {
	case legacyReceiptVersion:
		if r.StoreID != uuid.Nil {
			return fail("schema 1 cannot carry a store_id")
		}
	case ReceiptSchemaVersion:
		if r.StoreID == uuid.Nil {
			return fail("schema 2 requires a non-nil store_id")
		}
		extra = storeFieldBytes
	default:
		return fail("unknown schema_version %d", r.SchemaVersion)
	}
	switch {
	case r.AlbumID == uuid.Nil || r.BuildID == uuid.Nil:
		return fail("a nil album_id or build_id")
	case r.AlbumRevision <= 0:
		return fail("album_revision %d", r.AlbumRevision)
	case r.RenderVersion == "" || !utf8.ValidString(r.RenderVersion):
		return fail("an empty or invalid render_version")
	}
	seen := make(map[uuid.UUID]struct{})
	for i, f := range r.Files {
		if f.TrackID != uuid.Nil || f.SourceBlobSHA256 != "" {
			if r.SchemaVersion != ReceiptSchemaVersion || f.TrackID == uuid.Nil || blobstore.ValidateSHA(f.SourceBlobSHA256) != nil {
				return fail("file %d has invalid track provenance", i)
			}
			if _, ok := seen[f.TrackID]; ok {
				return fail("duplicate track_id %s", f.TrackID)
			}
			if len(seen) == maxReceiptTracks {
				return fail("more than %d track IDs", maxReceiptTracks)
			}
			seen[f.TrackID] = struct{}{}
			extra += trackFieldsBytes
		}
		if err := checkOutputPath(f.RelativePath); err != nil {
			return fail("file %d: %v", i, err)
		}
		if f.RelativePath == ReceiptName {
			return fail("the receipt lists itself")
		}
		if strings.ContainsFunc(f.RelativePath, func(c rune) bool { return c < 0x20 }) {
			return fail("file %d: a control character in the path", i)
		}
		if f.Size < 0 {
			return fail("file %q: size %d", f.RelativePath, f.Size)
		}
		if blobstore.ValidateSHA(f.SHA256) != nil {
			return fail("file %q: sha256 %q", f.RelativePath, f.SHA256)
		}
		if i > 0 && r.Files[i-1].RelativePath >= f.RelativePath {
			return fail("files not sorted by path bytes, or a path twice: %q then %q", r.Files[i-1].RelativePath, f.RelativePath)
		}
	}
	return extra, nil
}

func checkReceiptSize(size, extra int) error {
	if size > maxLegacyReceiptBytes+extra {
		return errorf(CodeReceiptInvalid, "the receipt is not valid: %d bytes, the maximum for its inventory is %d", size, maxLegacyReceiptBytes+extra)
	}
	return nil
}

// ForeignStore reports whether this valid receipt names a different store.
// V1 has no store identity; callers must still check their catalog anchors.
func (r Receipt) ForeignStore(expected uuid.UUID) bool {
	return r.SchemaVersion == ReceiptSchemaVersion && r.StoreID != expected
}

// String names the receipt for logs.
func (r Receipt) String() string {
	return fmt.Sprintf("receipt of album %s revision %d build %s (%d files)", r.AlbumID, r.AlbumRevision, r.BuildID, len(r.Files))
}
