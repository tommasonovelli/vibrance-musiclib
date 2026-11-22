package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func goldenReceipt() Receipt {
	return Receipt{
		SchemaVersion: legacyReceiptVersion,
		AlbumID:       uuid.MustParse("0192a5f0-1c2d-7e3f-8a4b-5c6d7e8f9a0b"),
		BuildID:       uuid.MustParse("0192a5f0-ffff-7000-8000-000000000001"),
		AlbumRevision: 12,
		RenderVersion: "musiclib-render/1 names/1 go1.25.14 ffmpeg/8.1.3-musiclib1 musiclib-tags/2 taglib/2.3.2-musiclib1",
		Files: []ReceiptFile{
			{RelativePath: "01 - So What.flac", Size: 1234567, SHA256: strings.Repeat("a", 64)},
			{RelativePath: "01 - So What.lrc", Size: 0, SHA256: strings.Repeat("0", 64)},
			{RelativePath: "Extras/<&> é" + lineSep + ".pdf", Size: 9, SHA256: strings.Repeat("f", 64)},
			{RelativePath: "cover.jpg", Size: 20, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		},
	}
}

// Keep these v1 bytes independent of the current writer. The key order,
// absence of whitespace and trailing newline, and raw UTF-8 (including
// HTML characters and U+2028) are part of the on-disk format.
func goldenV1ReceiptBytes() string {
	return `{"schema_version":1,"album_id":"0192a5f0-1c2d-7e3f-8a4b-5c6d7e8f9a0b","build_id":"0192a5f0-ffff-7000-8000-000000000001",` +
		`"album_revision":12,"render_version":"musiclib-render/1 names/1 go1.25.14 ffmpeg/8.1.3-musiclib1 musiclib-tags/2 taglib/2.3.2-musiclib1",` +
		`"files":[{"relative_path":"01 - So What.flac","size":1234567,"sha256":"` + strings.Repeat("a", 64) + `"},` +
		`{"relative_path":"01 - So What.lrc","size":0,"sha256":"` + strings.Repeat("0", 64) + `"},` +
		`{"relative_path":"Extras/<&> ` + "é" + lineSep + "" + `.pdf","size":9,"sha256":"` + strings.Repeat("f", 64) + `"},` +
		`{"relative_path":"cover.jpg","size":20,"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}]}`
}

func TestReceiptGolden(t *testing.T) {
	want := goldenV1ReceiptBytes()
	r, err := ParseReceipt([]byte(want))
	if err != nil {
		t.Fatalf("parse the v1 golden receipt: %v", err)
	}
	got, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("encoding\n%s\nwant\n%s", got, want)
	}
	if h := ReceiptHash(got); h != hashOf(want) {
		t.Fatalf("receipt_hash %s is not the SHA-256 of the bytes", h)
	}
	// It is valid JSON, and its content decodes as written.
	var generic map[string]any
	if err := json.Unmarshal(got, &generic); err != nil {
		t.Fatal(err)
	}
	if len(generic) != 6 {
		t.Fatalf("%d top-level fields, want exactly six", len(generic))
	}
	empty := r
	empty.Files = nil
	got, err = empty.Encode()
	if err != nil || !bytes.HasSuffix(got, []byte(`"files":[]}`)) {
		t.Fatalf("no files: %s %v", got, err)
	}
}

func goldenV2Receipt() Receipt {
	r := goldenReceipt()
	r.SchemaVersion = ReceiptSchemaVersion
	r.StoreID = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	r.Files[0].TrackID = uuid.MustParse("abcdef01-2345-4789-8abc-def012345678")
	r.Files[0].SourceBlobSHA256 = strings.Repeat("b", 64)
	// An audio-looking attachment has no track identity.
	r.Files[2].RelativePath = "Extras/<&> é" + lineSep + ".flac"
	return r
}

func goldenV2ReceiptBytes() string {
	return `{"schema_version":2,"store_id":"11111111-1111-4111-8111-111111111111","album_id":"0192a5f0-1c2d-7e3f-8a4b-5c6d7e8f9a0b","build_id":"0192a5f0-ffff-7000-8000-000000000001",` +
		`"album_revision":12,"render_version":"musiclib-render/1 names/1 go1.25.14 ffmpeg/8.1.3-musiclib1 musiclib-tags/2 taglib/2.3.2-musiclib1",` +
		`"files":[{"relative_path":"01 - So What.flac","size":1234567,"sha256":"` + strings.Repeat("a", 64) + `",` +
		`"track_id":"abcdef01-2345-4789-8abc-def012345678","source_blob_sha256":"` + strings.Repeat("b", 64) + `"},` +
		`{"relative_path":"01 - So What.lrc","size":0,"sha256":"` + strings.Repeat("0", 64) + `"},` +
		`{"relative_path":"Extras/<&> é` + lineSep + `.flac","size":9,"sha256":"` + strings.Repeat("f", 64) + `"},` +
		`{"relative_path":"cover.jpg","size":20,"sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}]}`
}

func TestReceiptV2Golden(t *testing.T) {
	want := goldenV2Receipt()
	literal := goldenV2ReceiptBytes()
	got, err := ParseReceipt([]byte(literal))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("parse v2: %+v, %v; want %+v", got, err, want)
	}
	encoded, err := got.Encode()
	if err != nil || string(encoded) != literal || ReceiptHash(encoded) != hashOf(literal) {
		t.Fatalf("encode v2: %s, %v", encoded, err)
	}
	// Different tracks may reference the same original.
	got.Files[1].TrackID, got.Files[1].SourceBlobSHA256 = uuid.New(), got.Files[0].SourceBlobSHA256
	encoded, err = got.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseReceipt(encoded); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptV2Strict(t *testing.T) {
	literal := goldenV2ReceiptBytes()
	const id = `"track_id":"abcdef01-2345-4789-8abc-def012345678"`
	source := `"source_blob_sha256":"` + strings.Repeat("b", 64) + `"`
	store := `"store_id":"11111111-1111-4111-8111-111111111111"`
	for _, tc := range []struct{ name, old, new string }{
		{"unknown version", `"schema_version":2`, `"schema_version":3`},
		{"v1 with v2 fields", `"schema_version":2`, `"schema_version":1`},
		{"v1 with track fields only", `"schema_version":2,` + store, `"schema_version":1`},
		{"no store", store + ",", ""},
		{"duplicate store field", store, store + "," + store},
		{"null store", store, `"store_id":null`},
		{"nil store", store, `"store_id":"00000000-0000-0000-0000-000000000000"`},
		{"track ID only", "," + source, ""},
		{"source hash only", id + ",", ""},
		{"null pair", id + "," + source, `"track_id":null,"source_blob_sha256":null`},
		{"nil track ID", id, `"track_id":"00000000-0000-0000-0000-000000000000"`},
		{"invalid track ID", id, `"track_id":"bad"`},
		{"uppercase track ID", id, `"track_id":"ABCDEF01-2345-4789-8ABC-DEF012345678"`},
		{"duplicate track ID", source + `},{"relative_path":"01 - So What.lrc","size":0,"sha256":"` + strings.Repeat("0", 64) + `"`,
			source + `},{"relative_path":"01 - So What.lrc","size":0,"sha256":"` + strings.Repeat("0", 64) + `",` + id + "," + source},
		{"uppercase source", source, strings.Replace(source, strings.Repeat("b", 64), strings.Repeat("B", 64), 1)},
		{"short source", source, `"source_blob_sha256":"bad"`},
		{"duplicate track field", id, id + "," + id},
		{"reordered provenance", id + "," + source, source + "," + id},
		{"invalid path", "cover.jpg", "../cover.jpg"},
		{"invalid size", `"size":0`, `"size":-1`},
		{"invalid output hash", strings.Repeat("a", 64), strings.Repeat("A", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(literal, tc.old) {
				t.Fatal("fixture did not match")
			}
			_, err := ParseReceipt([]byte(strings.Replace(literal, tc.old, tc.new, 1)))
			wantRenderCode(t, err, CodeReceiptInvalid)
		})
	}
}

func TestReceiptSizeBounds(t *testing.T) {
	// Exercise the boundary with real file entries, not padding outside
	// the JSON. There can be more attachments than an import can supply.
	legacy := goldenReceipt()
	legacy.Files = nil
	sum := hashOf("file")
	for i := range maxReceiptTracks {
		legacy.Files = append(legacy.Files, ReceiptFile{RelativePath: fmt.Sprintf("%04d - Track.flac", i), SHA256: sum})
	}
	longPath := strings.Repeat(strings.Repeat("x", 169)+"/", 5) + strings.Repeat("y", 160)
	base := len(legacy.encode())
	for i := 0; ; i++ {
		f := ReceiptFile{RelativePath: fmt.Sprintf("Extras/%06d/%s", i, longPath), SHA256: sum}
		// e.g. ,{"relative_path":"...","size":0,"sha256":"..."}
		n := len(`,{"relative_path":"","size":0,"sha256":""}`) + len(f.RelativePath) + len(sum)
		if base+n > maxLegacyReceiptBytes {
			break
		}
		legacy.Files = append(legacy.Files, f)
		base += n
	}
	legacy.RenderVersion += strings.Repeat("x", maxLegacyReceiptBytes-base)
	v2 := legacy
	v2.SchemaVersion, v2.StoreID = ReceiptSchemaVersion, uuid.New()
	v2.Files = slices.Clone(legacy.Files)
	for i := range maxReceiptTracks {
		v2.Files[i].TrackID, v2.Files[i].SourceBlobSHA256 = uuid.New(), sum
	}
	for _, tc := range []struct {
		r     Receipt
		limit int
	}{{legacy, 16 << 20}, {v2, MaxReceiptBytes}} {
		b, err := tc.r.Encode()
		if err != nil || len(b) != tc.limit {
			t.Fatalf("v%d boundary: %d bytes, %v; want %d", tc.r.SchemaVersion, len(b), err, tc.limit)
		}
		if _, err := ParseReceipt(b); err != nil {
			t.Fatal(err)
		}
		tc.r.RenderVersion += "x"
		_, err = ParseReceipt(tc.r.encode()) // canonical but one byte too large
		wantRenderCode(t, err, CodeReceiptInvalid)
		_, err = tc.r.Encode()
		wantRenderCode(t, err, CodeReceiptInvalid)
	}
	// A v2 receipt cannot use unused provenance allowance for attachments.
	v2.Files[0].TrackID, v2.Files[0].SourceBlobSHA256 = uuid.Nil, ""
	v2.RenderVersion += "x"
	_, err := ParseReceipt(v2.encode())
	wantRenderCode(t, err, CodeReceiptInvalid)
	v2.RenderVersion = legacy.RenderVersion
	v2.Files[0].TrackID, v2.Files[0].SourceBlobSHA256 = uuid.New(), sum
	v2.Files[maxReceiptTracks].TrackID, v2.Files[maxReceiptTracks].SourceBlobSHA256 = uuid.New(), sum
	_, err = v2.Encode()
	wantRenderCode(t, err, CodeReceiptInvalid)
}

// Escaping: only '"', '\' and control characters; the parser accepts only
// that form. Paths never hold them after §5.2, but render_version could.
func TestReceiptEscaping(t *testing.T) {
	r := goldenReceipt()
	r.RenderVersion = "a\"b\\c\x01\x1f/</>"
	got, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`"render_version":"a\"b\\c\u0001\u001f/</>"`)) {
		t.Fatalf("%s", got)
	}
	back, err := ParseReceipt(got)
	if err != nil || back.RenderVersion != r.RenderVersion {
		t.Fatalf("%+v %v", back, err)
	}
}

// Encode and ParseReceipt are inverses, on random receipts.
func TestReceiptRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	alphabet := []rune("aZ09 -_.()'!éß漢😀<>&" + lineSep)
	for i := range 200 {
		r := goldenReceipt()
		r.AlbumID, r.BuildID, r.AlbumRevision = uuid.New(), uuid.New(), rng.Int64N(1<<62)+1
		seen := map[string]bool{}
		r.Files = nil
		for range rng.IntN(30) {
			var b strings.Builder
			for range 1 + rng.IntN(20) {
				b.WriteRune(alphabet[rng.IntN(len(alphabet))])
			}
			p := strings.TrimSpace(b.String())
			if p == "" || p == "." || p == ".." || seen[p] || p == ReceiptName {
				continue
			}
			seen[p] = true
			sum := hashOf(p)
			r.Files = append(r.Files, ReceiptFile{RelativePath: p, Size: rng.Int64N(1 << 40), SHA256: sum})
		}
		sortFiles(r.Files)
		enc, err := r.Encode()
		if err != nil {
			t.Fatalf("%d: %v", i, err)
		}
		back, err := ParseReceipt(enc)
		if err != nil {
			t.Fatalf("%d: %v\n%s", i, err, enc)
		}
		if r.Files == nil {
			r.Files = []ReceiptFile{}
		}
		if !reflect.DeepEqual(back, r) {
			t.Fatalf("%d: round trip\n%+v\n%+v", i, back, r)
		}
		again, _ := back.Encode()
		if !bytes.Equal(again, enc) {
			t.Fatalf("%d: re-encoding differs", i)
		}
	}
}

func sortFiles(fs []ReceiptFile) {
	for i := 1; i < len(fs); i++ {
		for j := i; j > 0 && fs[j-1].RelativePath > fs[j].RelativePath; j-- {
			fs[j-1], fs[j] = fs[j], fs[j-1]
		}
	}
}

// The parser refuses anything but the canonical encoding of a valid receipt.
// Mutate the literal v1 bytes so these cases keep exercising the v1 parser.
func TestParseReceiptStrict(t *testing.T) {
	s := goldenV1ReceiptBytes()
	replace := func(old, new string) string {
		if !strings.Contains(s, old) {
			t.Fatalf("%q not in the receipt", old)
		}
		return strings.Replace(s, old, new, 1)
	}
	sha := strings.Repeat("a", 64)
	tests := map[string]string{
		"empty":                    "",
		"null":                     "null",
		"an array":                 "[]",
		"a trailing newline":       s + "\n",
		"leading whitespace":       " " + s,
		"a space after a colon":    replace(`"schema_version":1`, `"schema_version": 1`),
		"two documents":            s + s,
		"trailing garbage":         s + "x",
		"an unknown field":         replace(`"schema_version":1,`, `"schema_version":1,"extra":0,`),
		"an unknown file field":    replace(`"size":9,`, `"size":9,"mtime":0,`),
		"a repeated field":         replace(`"album_revision":12,`, `"album_revision":12,"album_revision":12,`),
		"a missing field":          replace(`"album_revision":12,`, ``),
		"a missing file field":     replace(`"size":9,`, ``),
		"files null":               s[:strings.Index(s, `"files":`)] + `"files":null}`,
		"keys reordered":           replace(`"album_id":"0192a5f0-1c2d-7e3f-8a4b-5c6d7e8f9a0b","build_id":"0192a5f0-ffff-7000-8000-000000000001"`, `"build_id":"0192a5f0-ffff-7000-8000-000000000001","album_id":"0192a5f0-1c2d-7e3f-8a4b-5c6d7e8f9a0b"`),
		"v2 declared on v1 bytes":  replace(`"schema_version":1`, `"schema_version":2`),
		"schema_version 1.0":       replace(`"schema_version":1`, `"schema_version":1.0`),
		"schema_version as text":   replace(`"schema_version":1`, `"schema_version":"1"`),
		"an upper-case UUID":       replace(`0192a5f0-1c2d-7e3f-8a4b-5c6d7e8f9a0b`, `0192A5F0-1C2D-7E3F-8A4B-5C6D7E8F9A0B`),
		"a URN UUID":               replace(`"0192a5f0-1c2d-7e3f-8a4b-5c6d7e8f9a0b"`, `"urn:uuid:0192a5f0-1c2d-7e3f-8a4b-5c6d7e8f9a0b"`),
		"a nil UUID":               replace(`0192a5f0-ffff-7000-8000-000000000001`, `00000000-0000-0000-0000-000000000000`),
		"not a UUID":               replace(`0192a5f0-ffff-7000-8000-000000000001`, `x`),
		"revision 0":               replace(`"album_revision":12`, `"album_revision":0`),
		"revision negative":        replace(`"album_revision":12`, `"album_revision":-12`),
		"revision with exponent":   replace(`"album_revision":12`, `"album_revision":1.2e1`),
		"an empty render_version":  s[:strings.Index(s, `"render_version":`)] + `"render_version":"",` + s[strings.Index(s, `"files":`):],
		"unsorted files":           replace(`"relative_path":"01 - So What.flac"`, `"relative_path":"zz.flac"`),
		"a path twice":             replace(`"relative_path":"01 - So What.lrc"`, `"relative_path":"01 - So What.flac"`),
		"the receipt itself":       replace(`"relative_path":"cover.jpg"`, `"relative_path":".musiclib.json"`),
		"an absolute path":         replace(`"relative_path":"cover.jpg"`, `"relative_path":"/cover.jpg"`),
		"a dot-dot path":           replace(`"relative_path":"cover.jpg"`, `"relative_path":"x/../y"`),
		"an empty segment":         replace(`"relative_path":"cover.jpg"`, `"relative_path":"x//y"`),
		"a control character":      replace(`"relative_path":"cover.jpg"`, `"relative_path":"x\u0001"`),
		"an escaped slash":         replace(`"relative_path":"cover.jpg"`, `"relative_path":"x\/y"`),
		"an escaped letter":        replace(`"relative_path":"cover.jpg"`, `"relative_path":"cover`+"\\u002e"+`jpg"`),
		"a negative size":          replace(`"size":9`, `"size":-9`),
		"a fractional size":        replace(`"size":9`, `"size":9.5`),
		"a size with leading zero": replace(`"size":9`, `"size":09`),
		"an upper-case sha256":     replace(`"sha256":"`+strings.Repeat("f", 64), `"sha256":"`+strings.Repeat("F", 64)),
		"a short sha256":           replace(`"sha256":"`+strings.Repeat("f", 64), `"sha256":"`+strings.Repeat("f", 63)),
		"a sha256 that is not hex": replace(`"sha256":"`+sha, `"sha256":"`+strings.Repeat("g", 64)),
		"invalid UTF-8":            replace(`cover.jpg`, "cover\xff.jpg"),
		"a lone surrogate":         replace(`cover.jpg`, `cover\ud800.jpg`),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseReceipt([]byte(input))
			wantRenderCode(t, err, CodeReceiptInvalid)
		})
	}
	if _, err := ParseReceipt(bytes.Repeat([]byte(" "), maxLegacyReceiptBytes+1)); Code(err) != CodeReceiptInvalid {
		t.Errorf("an oversized receipt: %v", err)
	}
}

// Encode refuses what the parser would refuse: the two are inverses.
func TestReceiptEncodeRefusesInvalid(t *testing.T) {
	edits := map[string]func(*Receipt){
		"nil album":       func(r *Receipt) { r.AlbumID = uuid.Nil },
		"nil build":       func(r *Receipt) { r.BuildID = uuid.Nil },
		"revision 0":      func(r *Receipt) { r.AlbumRevision = 0 },
		"no version":      func(r *Receipt) { r.RenderVersion = "" },
		"unsorted":        func(r *Receipt) { r.Files[0], r.Files[1] = r.Files[1], r.Files[0] },
		"duplicate":       func(r *Receipt) { r.Files[1].RelativePath = r.Files[0].RelativePath },
		"itself":          func(r *Receipt) { r.Files[3].RelativePath = ReceiptName },
		"bad sha":         func(r *Receipt) { r.Files[0].SHA256 = "x" },
		"negative size":   func(r *Receipt) { r.Files[0].Size = -1 },
		"invalid path":    func(r *Receipt) { r.Files[3].RelativePath = "../x" },
		"invalid UTF-8":   func(r *Receipt) { r.RenderVersion = "\xff" },
		"control in path": func(r *Receipt) { r.Files[3].RelativePath = "z\x02" },
		"out of order":    func(r *Receipt) { r.Files[3].RelativePath = "Extras/" + strconv.Itoa(0) },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			r := goldenReceipt()
			edit(&r)
			if _, err := r.Encode(); Code(err) != CodeReceiptInvalid {
				t.Fatalf("Encode accepted it: %v", err)
			}
		})
	}
}

// The order of files is the order of the UTF-8 bytes of the paths, which is
// the order of code points: U+FF21 (EF BC A1) before U+1F600 (F0 9F 98 80),
// although UTF-16 would put the emoji's surrogates (D83D) first.
func TestReceiptOrderIsUTF8Bytes(t *testing.T) {
	fullwidthA, emoji := string(rune(0xFF21)), string(rune(0x1F600))
	r := goldenReceipt()
	r.Files = []ReceiptFile{
		{RelativePath: "B", SHA256: hashOf("1")}, {RelativePath: "a", SHA256: hashOf("2")},
		{RelativePath: fullwidthA, SHA256: hashOf("3")}, {RelativePath: emoji, SHA256: hashOf("4")},
	}
	if _, err := r.Encode(); err != nil {
		t.Fatalf("byte order refused: %v", err)
	}
	r.Files[2], r.Files[3] = r.Files[3], r.Files[2]
	if _, err := r.Encode(); Code(err) != CodeReceiptInvalid {
		t.Fatalf("UTF-16 order accepted: %v", err)
	}
	r.Files[2], r.Files[3] = r.Files[3], r.Files[2]
	r.Files[0], r.Files[1] = r.Files[1], r.Files[0]
	if _, err := r.Encode(); Code(err) != CodeReceiptInvalid {
		t.Fatalf("case-insensitive order accepted: %v", err)
	}
}

// lineSep is U+2028, which encoding/json would escape and Encode must not.
var lineSep = string(rune(0x2028))

// The receipt accepts exactly the paths the planner produces (one rule,
// checkOutputPath): §5.2's 16 levels and 1,024 bytes, measured below
// Extras/ for an attachment and on the path itself otherwise, and 180 bytes
// per segment. At the maximum it round-trips; one level or one byte more is
// refused, by Encode and by ParseReceipt.
func TestReceiptPathLimits(t *testing.T) {
	levels := func(n int) string { return strings.Repeat("d/", n-1) + "f" }
	bytesLong := func(n int) string { // 6 segments of 169 bytes, then the rest
		return strings.Repeat(strings.Repeat("x", 169)+"/", 6) + strings.Repeat("y", n-6*170)
	}
	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{"16 levels", levels(16), true},
		{"17 levels", levels(17), false},
		{"Extras and 16 levels", "Extras/" + levels(16), true},
		{"Extras and 17 levels", "Extras/" + levels(17), false},
		{"1,024 bytes", bytesLong(1024), true},
		{"1,025 bytes", bytesLong(1025), false},
		{"Extras and 1,024 bytes", "Extras/" + bytesLong(1024), true},
		{"Extras and 1,025 bytes", "Extras/" + bytesLong(1025), false},
		{"a segment of 180 bytes", strings.Repeat("s", 180), true},
		{"a segment of 181 bytes", strings.Repeat("s", 181), false},
		{"Extras and a segment of 181 bytes", "Extras/" + strings.Repeat("s", 181), false},
		{"extras is not Extras", "extras/" + levels(16), false},
		{"Extras and nothing", "Extras/", false},
		{"Extras and a dot-dot", "Extras/../x", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := goldenReceipt()
			r.Files = []ReceiptFile{{RelativePath: tc.path, Size: 1, SHA256: hashOf("x")}}
			enc, err := r.Encode()
			if !tc.ok {
				if Code(err) != CodeReceiptInvalid {
					t.Fatalf("Encode accepted %d bytes, %d levels: %v", len(tc.path), strings.Count(tc.path, "/")+1, err)
				}
				// ParseReceipt refuses the same bytes, written by hand.
				forged := strings.Replace(goldenV1ReceiptBytes(), `"cover.jpg"`, `"`+tc.path+`"`, 1)
				if _, err := ParseReceipt([]byte(forged)); Code(err) != CodeReceiptInvalid {
					t.Fatalf("ParseReceipt accepted it: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if back, err := ParseReceipt(enc); err != nil || back.Files[0].RelativePath != tc.path {
				t.Fatalf("ParseReceipt: %v", err)
			}
		})
	}
}
