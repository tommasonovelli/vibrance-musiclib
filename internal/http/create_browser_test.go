package http

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"

	"github.com/chromedp/chromedp"
	"github.com/google/uuid"
)

func TestAlbumDraftRoute(t *testing.T) {
	e := authEnv(t)
	const path = "/albums/new"
	for _, method := range []string{"GET", "HEAD"} {
		if a := send(t, e, method, path, ""); a.status != 303 || a.header.Get("Location") != "/login" {
			t.Fatalf("unsigned %s: %d %v", method, a.status, a.header)
		}
	}
	cookie := session(t, e)
	idPattern := regexp.MustCompile(`id="draft" data-id="([^"]+)"`)
	ids := make(map[uuid.UUID]bool)
	for range 2 {
		a := send(t, e, "GET", path, "", "Cookie", cookie)
		m := idPattern.FindStringSubmatch(a.body)
		if a.status != 200 || a.header.Get("Cache-Control") != "no-store" || len(m) != 2 {
			t.Fatalf("draft GET: %d %v, no draft ID: %t", a.status, a.header, len(m) != 2)
		}
		id, err := uuid.Parse(m[1])
		if err != nil || id == uuid.Nil || id.String() != m[1] || ids[id] {
			t.Fatalf("invalid or reused creation ID %q", m[1])
		}
		ids[id] = true
	}
	if a := send(t, e, "HEAD", path, "", "Cookie", cookie); a.status != 200 || a.body != "" {
		t.Fatalf("draft HEAD: %d, body length %d", a.status, len(a.body))
	}
	if a := send(t, e, "POST", path, "", "Cookie", cookie, "X-Musiclib-Request", "1"); a.status != 405 {
		t.Fatalf("draft POST: %d", a.status)
	}
	e.noCreationState()
}

func TestBrowserAlbumDraft(t *testing.T) {
	audio := [][]byte{
		creationAudio(t, "flac", "flac", "Source A", "Artist", "Rock", 1),
		creationAudio(t, "flac", "flac", "Source B", "Artist", "Rock", 1),
	}
	names := []string{`A"B.flac`, "A%22B.flac"}
	dir := t.TempDir()
	trackPath, coverPath := filepath.Join(dir, names[0]), filepath.Join(dir, "cover.jpg")
	cover, replacement := jpegImage(t, 12, 12), jpegImage(t, 16, 16)
	if err := os.WriteFile(trackPath, audio[0], 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(coverPath, cover, 0600); err != nil {
		t.Fatal(err)
	}

	fileJS := func(name, kind string, data []byte) string {
		return fmt.Sprintf(`new File([Uint8Array.from(atob(%q), c => c.charCodeAt(0))], %q, {type: %q})`,
			base64.StdEncoding.EncodeToString(data), name, kind)
	}
	first := fileJS(names[0], "audio/flac", audio[0])
	second := fileJS(names[1], "application/octet-stream", audio[1])
	art := fileJS("cover.jpg", "image/jpeg", cover)
	other := fileJS("cover.jpg", "image/jpeg", replacement)
	const dropJS = `const drop = (target, ...files) => {
		const dataTransfer = new DataTransfer();
		for (const file of files) dataTransfer.items.add(file);
		return !$(target).dispatchEvent(new DragEvent('drop', {bubbles: true, cancelable: true, dataTransfer}));
	};`
	albumLookup := regexp.MustCompile(`^/api/albums/[^/]+$`)
	for _, mode := range []string{"confirmed", "lost_reply", "unverified_retry"} {
		t.Run(mode, func(t *testing.T) {
			var posts, lookups atomic.Int32
			e, tab := browserEnvWithWrapper(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "POST" && r.URL.Path == "/api/albums" && posts.Add(1) == 1 && mode != "confirmed" {
						// The real server commits, but a gateway returns its own reply.
						reply := httptest.NewRecorder()
						next.ServeHTTP(reply, r)
						if reply.Code != 201 {
							t.Errorf("creation behind gateway: %d %s", reply.Code, reply.Body)
						}
						http.Error(w, "Gateway timeout", 504)
						return
					}
					if r.Method == "GET" && albumLookup.MatchString(r.URL.Path) && lookups.Add(1) == 1 && mode == "unverified_retry" {
						http.Error(w, "Gateway unavailable", 503)
						return
					}
					next.ServeHTTP(w, r)
				})
			})
			e.enableCreation(e.testImporter(t))
			if err := chromedp.Run(tab, chromedp.Navigate(e.srv.URL+"/albums/new"), chromedp.WaitVisible("#draft"),
				chromedp.SetUploadFiles("#draft-files", []string{trackPath}, chromedp.NodeReady)); err != nil {
				t.Fatal(err)
			}
			if got := js(t, tab, dropJS+fmt.Sprintf(`drop('#track-formats', %s, %s); return String($$('#draft-tracks li').length)`, second, first)); got != "1" {
				t.Fatalf("duplicate drop partly applied: %s tracks", got)
			}
			if mode == "confirmed" {
				if got := js(t, tab, dropJS+fmt.Sprintf(`
					return String([new File(['text'], 'notes.txt'), new File([], 'empty.flac')].every(invalid =>
						drop('#track-formats', %s, invalid) && $$('#draft-tracks li').length === 1));`, second)); got != "true" {
					t.Fatal("an unsupported or empty file did not reject the entire audio batch")
				}
			}
			if got := js(t, tab, dropJS+fmt.Sprintf(`return String(drop('#track-formats', %s) && drop('#draft-art-note', %s))`, second, other)); got != "true" {
				t.Fatal("file drops did not cancel browser defaults")
			}
			pickCover := chromedp.SetUploadFiles("#draft-cover", []string{coverPath}, chromedp.NodeReady)
			if err := chromedp.Run(tab, pickCover); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "confirmed":
				// The hash check after the real POST also checks that the good cover survived these drops.
				js(t, tab, dropJS+`drop('#draft-cover-drop', new File(['GIF89a'], 'art.gif', {type: 'image/gif'}));
					drop('#draft-cover-drop', new File(['%PDF-1.7'], 'art.pdf', {type: 'application/pdf'}));
					drop('#draft-cover-drop', new File([], 'art.png', {type: 'image/png'})); return ''`)
			case "lost_reply":
				// The last single drop wins over the picker, even with the same name.
				// A subsequent multi-file drop must not replace or clear it.
				js(t, tab, dropJS+fmt.Sprintf(`drop('#draft-cover-drop', %s); drop('#draft-cover-drop', %s); drop('#draft-cover-drop', %s, %s); return ''`, art, other, art, second))
			case "unverified_retry":
				js(t, tab, dropJS+fmt.Sprintf(`drop('#draft-cover-drop', %s); return ''`, other))
				if err := chromedp.Run(tab, chromedp.Click("#clear-cover", chromedp.ByQuery)); err != nil {
					t.Fatal(err)
				}
			}
			id := uuid.MustParse(browserEval(t, tab, `document.querySelector('#draft').dataset.id`))
			const firstTrack = "#draft-tracks > li:first-child "
			const genre = firstTrack + "[name=genre]"
			const noGenre = firstTrack + "[name=nogenre]"
			if err := chromedp.Run(tab, chromedp.Click(firstTrack+"summary", chromedp.ByQuery),
				chromedp.SendKeys(genre, "Soul", chromedp.ByQuery), chromedp.Click(noGenre, chromedp.ByQuery),
				chromedp.SendKeys(firstTrack+"[name=title]", "Edited track", chromedp.ByQuery)); err != nil {
				t.Fatal(err)
			}
			if browserEval(t, tab, `String(document.querySelector('`+genre+`').disabled)`) != "true" {
				t.Fatal("the genre override remains editable while No genre is selected")
			}
			js(t, tab, `$("[name=artist_name]").value = ' Browser Artist '; $("#draft-album [name=title]").value = '   ';
				$("#draft-album [name=genre]").value = 'Rock'; return ''`)
			e.noCreationState()
			if pinned, _ := e.blobFiles(); posts.Load() != 0 || pinned != 0 {
				t.Fatal("editing the draft uploaded files")
			}
			wantGenre := ""
			if mode == "confirmed" {
				if got := js(t, tab, dropJS+fmt.Sprintf(`$('#draft').requestSubmit();
					return String($('#draft-fields').disabled && drop('#draft-cover-drop', %s) && drop('.draft-picker', %s) && drop('#sidebar', %s));`,
					other, fileJS("unwanted.flac", "audio/flac", audio[0]), art)); got != "true" {
					t.Fatal("locked and off-target drops did not cancel browser defaults")
				}
				browserWait(t, tab, `!document.querySelector('#draft-error').hidden && !document.querySelector('#create-album').disabled`)
				if lookups.Load() != 0 {
					t.Fatal("a validated refusal was treated as an ambiguous reply")
				}
				if err := chromedp.Run(tab, chromedp.Click(noGenre, chromedp.ByQuery)); err != nil {
					t.Fatal(err)
				}
				if browserEval(t, tab, `String(document.querySelector('`+genre+`').disabled)`) != "false" {
					t.Fatal("deselecting No genre did not re-enable the genre override")
				}
				// Rock wins the inference tie, so track two still inherits it.
				wantGenre = "Soul"
			}
			js(t, tab, `$("#draft-album [name=title]").value = 'Browser album'; $('#draft').requestSubmit(); $('#draft').requestSubmit(); return ''`)
			if mode == "unverified_retry" {
				browserWait(t, tab, `!document.querySelector('#draft-error').hidden && !document.querySelector('#create-album').disabled`)
				if posts.Load() != 1 || lookups.Load() != 1 {
					t.Fatalf("unexpected automatic retry or missing lookup: %d POSTs, %d lookups", posts.Load(), lookups.Load())
				}
				js(t, tab, `$("#draft-album [name=title]").value = 'Changed draft'; $('#draft').requestSubmit(); return ''`)
				browserWait(t, tab, `!document.querySelector('#draft-existing').hidden`)
				if got := browserEval(t, tab, `document.querySelector('#draft-existing').getAttribute('href')`); got != "/albums/"+id.String() {
					t.Fatalf("conflict points to %s", got)
				}
			} else {
				browserWait(t, tab, fmt.Sprintf(`location.pathname === %q && document.querySelector('#creation-warnings li') && document.querySelector('#status').dataset.pending === 'true'`, "/albums/"+id.String()))
				if got := browserEval(t, tab, `String(sessionStorage.getItem('creation-warnings'))`); got != "null" {
					t.Fatal("the editor retained its one-time handoff")
				}
			}
			wantPosts := int32(2)
			if mode == "lost_reply" {
				wantPosts = 1
			}
			v, err := e.svc.GetAlbum(t.Context(), id)
			if err != nil || v.Revision != 1 || v.Title != "Browser album" || v.ArtistName != "Browser Artist" ||
				len(v.Tracks) != 2 || posts.Load() != wantPosts {
				t.Fatalf("album after confirmation: %+v, error %v, POSTs %d", v, err, posts.Load())
			}
			wantCover := cover
			if mode == "lost_reply" {
				wantCover = replacement
			}
			if mode == "unverified_retry" {
				if v.Cover != nil {
					t.Fatalf("cleared cover was submitted: %+v", v.Cover)
				}
			} else if v.Cover == nil || v.Cover.Hash != sha(wantCover) {
				t.Fatalf("wrong cover bytes after confirmation: %+v", v.Cover)
			}
			for i, tr := range v.Tracks {
				if tr.SourcePath != names[i] || tr.Blob.Hash != sha(audio[i]) {
					t.Fatalf("original filename or bytes changed: %+v", tr)
				}
			}
			if g := v.Tracks[0].Genre; g == nil || *g != wantGenre || v.Tracks[1].Genre != nil {
				t.Fatalf("want first track genre %q and no override on the second: %+v", wantGenre, v.Tracks)
			}
			if got := v.Tracks[0].Title; got != "Edited track" {
				t.Fatalf("track title edited with No genre selected was not saved: %q", got)
			}
			if e.count("SELECT count(*) FROM jobs WHERE kind='render' AND requested=1") != 1 || e.count("SELECT count(*) FROM albums") != 1 {
				t.Fatal("confirmation or retry created extra albums or render requests")
			}
		})
	}
}
