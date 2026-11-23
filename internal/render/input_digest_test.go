package render

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"musiclib/internal/blobstore"
	"musiclib/internal/catalog"
	"musiclib/internal/media"
)

func TestBuildReusesInputDigests(t *testing.T) {
	e := newEnv(t)
	a := e.album("Artist", "Mixed formats")
	a.track(1, 1, "FLAC", song{id3v1: true, id3v2: true, tags: []string{"COMMENT=keep me"}})
	mp3 := append([]byte(nil), baseMP3(t, sine(550, 0.4))...)
	mp3 = append(mp3, apeText([2]string{"REPLAYGAIN_ALBUM_GAIN", "-7.00 dB"})...)
	a.mp3Track(1, 2, "MP3", append(mp3, v1Tag("Old title", "Old comment")...))
	a.m4aTrack(1, 3, "AAC", catalog.FormatM4AAAC,
		baseM4A(t, sine(660, 0.4), "aac", true, jpegImage(t, 8, 8, 2), "comment=keep me"))
	a.m4aTrack(2, 1, "ALAC", catalog.FormatM4AALAC,
		baseM4A(t, sine(770, 0.4), "alac", false, nil, "composer=keep me"))
	a.cover(jpegImage(t, 32, 24, 1), catalog.FormatJPEG)
	a.lyrics(&a.s.Tracks[0], "[00:00.15] lyrics")
	a.attach("booklet.txt", []byte("booklet"))
	p := a.plan()
	for i := range p.Tracks {
		p.Tracks[i].ProbeDuration = true
	}

	decodes := map[string]int{}
	e.setFailpoint(func(point, path string, _ *os.File) error {
		if point == "prewrite_digest" {
			decodes[path]++
		}
		return nil
	})
	cold, warm := e.mustBuild(p), e.mustBuild(p)
	compareBuilds(t, e, cold, e, warm)
	files, _, _ := e.staged(warm)
	receipt, err := ParseReceipt(files[ReceiptName])
	if err != nil {
		t.Fatal(err)
	}
	if warm.ReceiptHash != sha(files[ReceiptName]) {
		t.Fatal("wrong SHA-256 for the warm receipt")
	}
	for _, file := range receipt.Files {
		f := files[file.RelativePath]
		if file.Size != int64(len(f)) || file.SHA256 != sha(f) {
			t.Errorf("incorrect inventory of %q", file.RelativePath)
		}
	}
	for _, tr := range p.Tracks {
		if decodes[tr.Path] != 1 {
			t.Errorf("%s: %d pre-write decodes across both builds, want 1", tr.Path, decodes[tr.Path])
		}
		coldMS, warmMS := cold.Durations[tr.Blob.Hash], warm.Durations[tr.Blob.Hash]
		if coldMS <= 0 || warmMS != coldMS {
			t.Errorf("%s: duration changed from %d to %d", tr.Path, coldMS, warmMS)
		}
		if e.digest("original", e.blob(tr.Blob.Hash)) != e.digest("staged", files[tr.Path]) {
			t.Errorf("%s: rendered PCM differs from the original", tr.Path)
		}
	}
}

func discardBuild(t *testing.T, b *Builder, res Result) {
	t.Helper()
	defer res.Space.Release()
	if err := b.Discard(context.Background(), res.BuildID); err != nil {
		t.Fatal(err)
	}
}

func TestBuildChecksWarmInputDigests(t *testing.T) {
	for _, fault := range []struct {
		name, code string
	}{
		{"original", blobstore.CodeCorrupt},
		{"copy", CodeCopyMismatch},
		{"pcm", CodeAudioChanged},
	} {
		t.Run(fault.name, func(t *testing.T) {
			e := newEnv(t)
			a := e.album("Artist", "Warm")
			a.track(1, 1, "Track", song{tags: []string{"COMMENT=keep me"}})
			p := a.plan()
			discardBuild(t, e.b, e.mustBuild(p))

			var wroteTags bool
			e.setFailpoint(func(point, path string, f *os.File) error {
				if path != p.Tracks[0].Path {
					return nil
				}
				switch {
				case point == "prewrite_digest":
					return fmt.Errorf("unexpected pre-write decode for a warm track")
				case point == "write" && fault.name == "copy":
					_, err := f.Write([]byte{0})
					return err
				case point == "tags_written":
					wroteTags = true
					if fault.name == "pcm" {
						audio := baseFLAC(t, sine(441, 0.3)).audio
						return rewrite(t, f, func(ff flacFile) flacFile { ff.audio = audio; return ff })
					}
				}
				return nil
			})
			if fault.name == "original" {
				hash := p.Tracks[0].Blob.Hash
				original := e.blob(hash)
				at := bytes.Index(original, []byte("keep me"))
				if at < 0 {
					t.Fatal("expected original comment is absent")
				}
				// Corrupt the immutable original without changing the PCM.
				original[at] = 'K'
				name := filepath.Join(e.data, "originals", hash[:2], hash[2:4], hash)
				if err := os.Chmod(name, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, original, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(name, 0o444); err != nil {
					t.Fatal(err)
				}
			}
			_, err := e.build(p)
			wantRenderCode(t, err, fault.code)
			if wroteTags != (fault.name == "pcm") {
				t.Errorf("tags_written reached = %v", wroteTags)
			}
		})
	}
}

func TestBuildMemoizesOnlySyncedTracks(t *testing.T) {
	e := newEnv(t)
	a := e.album("Artist", "Sync")
	a.track(1, 1, "Track", song{})
	p := a.plan()
	e.setFailpoint(func(point, path string, _ *os.File) error {
		if point == "fsync_file" && path == p.Tracks[0].Path {
			return syscall.ENOSPC
		}
		return nil
	})
	_, err := e.build(p)
	wantRenderCode(t, err, CodeInsufficientSpace)

	var decoded bool
	e.setFailpoint(func(point, _ string, _ *os.File) error {
		decoded = decoded || point == "prewrite_digest"
		return nil
	})
	discardBuild(t, e.b, e.mustBuild(p))
	if !decoded {
		t.Fatal("a track with failed fsync was remembered")
	}
}

func TestBuildInputDigestEvictionAndReset(t *testing.T) {
	e := newEnv(t)
	a := e.album("Artist", "Eviction")
	a.track(1, 1, "Track", song{})
	p := a.plan()
	discardBuild(t, e.b, e.mustBuild(p))

	for i := range inputDigestLimit {
		e.b.digests.remember(Blob{Hash: fmt.Sprintf("%064x", i), Size: 1}, media.Digest{})
	}
	if got := len(e.b.digests.byBlob); got != inputDigestLimit {
		t.Fatalf("%d entries after exceeding the limit, want %d", got, inputDigestLimit)
	}
	var prewrites int
	e.setFailpoint(func(point, _ string, _ *os.File) error {
		if point == "prewrite_digest" {
			prewrites++
		}
		return nil
	})
	discardBuild(t, e.b, e.mustBuild(p)) // Evicted, decodes twice.
	discardBuild(t, e.b, e.mustBuild(p)) // Learned again, decodes once.
	if prewrites != 1 {
		t.Fatalf("%d pre-write decodes after eviction and repopulation, want 1", prewrites)
	}

	old := e.b
	var err error
	e.b, err = New(Config{Tools: e.tools, Blobs: e.blobs, Work: e.work, Budget: old.budget, Failpoints: e.fp.Hook()})
	if err != nil {
		t.Fatal(err)
	}
	discardBuild(t, e.b, e.mustBuild(p))
	if prewrites != 2 {
		t.Fatal("a fresh builder skipped its pre-write decode")
	}
}

func TestBuildInputDigestsConcurrent(t *testing.T) {
	e := newEnv(t)
	a := e.album("Artist", "Concurrent")
	a.track(1, 1, "Track", song{})
	p := a.plan()
	const workers = 4
	var prewrites atomic.Int32
	e.setFailpoint(func(point, _ string, _ *os.File) error {
		if point == "prewrite_digest" {
			prewrites.Add(1)
		}
		return nil
	})
	builds := make([]Result, workers)
	errs := make([]error, workers)
	for _, warm := range []bool{false, true} {
		before := prewrites.Load()
		start := make(chan struct{})
		done := make(chan int, workers)
		for i := range workers {
			go func() {
				<-start
				builds[i], errs[i] = e.b.Build(context.Background(), p)
				done <- i
			}()
		}
		close(start)
		for range workers {
			i := <-done
			if errs[i] != nil {
				t.Errorf("worker %d: %v", i, errs[i])
			}
		}
		if t.Failed() {
			t.FailNow()
		}
		for _, res := range builds[1:] {
			compareBuilds(t, e, builds[0], e, res)
		}
		for _, res := range builds {
			discardBuild(t, e.b, res)
		}
		if got := prewrites.Load() - before; (warm && got != 0) || (!warm && (got < 1 || got > workers)) {
			t.Errorf("warm=%v: unexpected %d pre-write decodes", warm, got)
		}
	}
}
